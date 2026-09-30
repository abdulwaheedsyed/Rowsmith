package mssql

import (
	"context"
	sqldriver "database/sql/driver"
	"io"
	"net"
	"strings"
	"sync"

	ms "github.com/microsoft/go-mssqldb"

	"rowsmith/internal/driver"
)

// connector wraps go-mssqldb's connector for two engine quirks:
//
//   - A single UPDATE or DELETE reports its own row count (@@ROWCOUNT).
//     go-mssqldb adds up the counts of every statement the server ran,
//     including those inside triggers, which would make one-row grid edits
//     look like multi-row ones.
//   - Each physical connection carries a liveness signal. The message queue
//     consoles read stays silent when a connection dies before the server
//     answers (e.g. a killed session), so consoles watch it to stop waiting.
type connector struct{ inner *ms.Connector }

func (c connector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	live := &liveness{dead: make(chan struct{})}
	dc, err := c.inner.Connect(context.WithValue(ctx, livenessKey{}, live))
	if err != nil {
		return nil, err
	}
	if mc, ok := dc.(*ms.Conn); ok {
		return &driverConn{Conn: mc, live: live}, nil
	}
	return dc, nil
}

func (c connector) Driver() sqldriver.Driver { return c.inner.Driver() }

// liveness is closed once a read on the physical connection fails; after
// login that only happens when the connection is gone.
type liveness struct {
	once sync.Once
	dead chan struct{}
}

func (l *liveness) fail() { l.once.Do(func() { close(l.dead) }) }

type livenessKey struct{}

type watchedConn struct {
	net.Conn
	live *liveness
}

func (w watchedConn) Read(b []byte) (int, error) {
	n, err := w.Conn.Read(b)
	if err != nil {
		w.live.fail()
	}
	return n, err
}

// watch attaches the liveness signal of the connection being opened.
func watch(ctx context.Context, nc net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	if l, ok := ctx.Value(livenessKey{}).(*liveness); ok {
		return watchedConn{Conn: nc, live: l}, nil
	}
	return nc, nil
}

// directDialer leaves name resolution (and multi-subnet failover) to go-mssqldb.
type directDialer struct{ d net.Dialer }

func (d directDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	nc, err := d.d.DialContext(ctx, network, addr)
	return watch(ctx, nc, err)
}

// tunnelDialer routes connections through an SSH tunnel. As a HostDialer it
// receives the host name unresolved, so names resolve on the far side.
type tunnelDialer struct {
	dial driver.DialFunc
	host string
}

func (d tunnelDialer) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	nc, err := d.dial(ctx, "tcp", addr)
	return watch(ctx, nc, err)
}

func (d tunnelDialer) HostName() string { return d.host }

type driverConn struct {
	*ms.Conn
	live *liveness
}

func (c *driverConn) PrepareContext(ctx context.Context, query string) (sqldriver.Stmt, error) {
	st, err := c.Conn.PrepareContext(ctx, query)
	if err != nil || !(strings.HasPrefix(query, "UPDATE ") || strings.HasPrefix(query, "DELETE ")) {
		return st, err
	}
	mst, ok := st.(*ms.Stmt)
	if !ok {
		return st, nil
	}
	return &dmlStmt{Stmt: mst, conn: c.Conn, query: query}, nil
}

type dmlStmt struct {
	*ms.Stmt
	conn  *ms.Conn
	query string
}

func (s *dmlStmt) ExecContext(ctx context.Context, args []sqldriver.NamedValue) (sqldriver.Result, error) {
	st, err := s.conn.PrepareContext(ctx, s.query+"\nSELECT @@ROWCOUNT")
	if err != nil {
		return nil, err
	}
	defer st.Close()
	rows, err := st.(sqldriver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Triggers may return result sets of their own; the count is the last value.
	var n int64
	for {
		dest := make([]sqldriver.Value, len(rows.Columns()))
		for {
			err := rows.Next(dest)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if len(dest) == 1 {
				if v, ok := dest[0].(int64); ok {
					n = v
				}
			}
		}
		next, ok := rows.(sqldriver.RowsNextResultSet)
		if !ok || !next.HasNextResultSet() || next.NextResultSet() != nil {
			break
		}
	}
	return sqldriver.RowsAffected(n), nil
}
