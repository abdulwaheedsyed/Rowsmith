package oracle

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	sqldriver "database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	go_ora "github.com/sijms/go-ora/v2"

	"rowsmith/internal/driver"
)

// connector opens go-ora connections. Each one dials through the configured
// DialFunc (an SSH tunnel resolves the host on its far side) and gets its own
// copy of the TLS config, because go-ora writes ServerName into it.
type connector struct {
	dsn     string
	dial    driver.DialFunc
	tls     *tls.Config
	timeout time.Duration
}

func (c *connector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	oc, ok := go_ora.NewConnector(c.dsn).(*go_ora.OracleConnector)
	if !ok {
		return nil, errors.New("unexpected go-ora connector")
	}
	d := &dialer{dial: c.dial, timeout: c.timeout}
	oc.Dialer(d)
	if c.tls != nil {
		oc.WithTLSConfig(c.tls.Clone())
	}
	dc, err := oc.Connect(ctx)
	if err != nil {
		if d.conn != nil {
			d.conn.Close()
		}
		return nil, err
	}
	oconn, ok := dc.(*go_ora.Connection)
	if !ok {
		dc.Close()
		return nil, errors.New("unexpected go-ora connection")
	}
	return &safeConn{Connection: oconn, raw: d.conn}, nil
}

func (c *connector) Driver() sqldriver.Driver { return go_ora.GetDefaultDriver() }

// dialer implements go-ora's DialerContext and remembers the socket so a
// broken connection can be torn down without speaking the protocol.
type dialer struct {
	dial    driver.DialFunc
	timeout time.Duration
	conn    net.Conn
}

func (d *dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	var nc net.Conn
	var err error
	if d.dial != nil {
		nc, err = d.dial(ctx, "tcp", addr)
	} else {
		nc, err = (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	if err == nil {
		d.conn = nc
	}
	return nc, err
}

// tlsConfig adapts driver.TLSConfig to go-ora, which overwrites ServerName
// with the dialed host: for verify-full the certificate name is checked here.
func tlsConfig(p driver.OpenParams, host string) (*tls.Config, error) {
	switch p.String("tls") {
	case "", "disable":
		return nil, nil
	case "prefer":
		return nil, errors.New(`TLS: "preferred" is not available for Oracle, which serves TCPS on a separate listener; choose Required or Disabled`)
	}
	cfg, err := driver.TLSConfig(p, host)
	if err != nil || cfg == nil {
		return cfg, err
	}
	if p.String("tls") == "verify-full" {
		name, roots := cfg.ServerName, cfg.RootCAs
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("TLS: server sent no certificate")
			}
			opts := x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: x509.NewCertPool()}
			for _, cert := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(cert)
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			return err
		}
	}
	return cfg, nil
}

// errDriverFailure marks results go-ora could not decode. go-ora panics on
// object-type columns (SDO_GEOMETRY, user-defined types) it was not told
// about; safeConn turns that into this error and retires the connection,
// whose protocol stream is left in an unknown state.
var errDriverFailure = errors.New("the Oracle driver could not read this result; object-type columns " +
	"(SDO_GEOMETRY, user-defined types, collections) must be converted in SQL, e.g. SDO_UTIL.TO_WKTGEOMETRY(col). " +
	"The connection was reset and uncommitted changes were rolled back")

// safeConn wraps a go-ora connection so driver panics become errors instead
// of leaking database/sql locks and pool slots.
type safeConn struct {
	*go_ora.Connection
	raw net.Conn
	bad atomic.Bool
}

func (c *safeConn) guard(err *error) {
	if r := recover(); r != nil {
		c.bad.Store(true)
		*err = fmt.Errorf("%w (%v)", errDriverFailure, r)
	}
}

func (c *safeConn) QueryContext(ctx context.Context, query string, args []sqldriver.NamedValue) (rows sqldriver.Rows, err error) {
	if c.bad.Load() {
		return nil, sqldriver.ErrBadConn
	}
	defer c.guard(&err)
	rows, err = c.Connection.QueryContext(ctx, query, args)
	if ds, ok := rows.(*go_ora.DataSet); ok && err == nil {
		return &safeRows{DataSet: ds, conn: c}, nil
	}
	return rows, err
}

func (c *safeConn) ExecContext(ctx context.Context, query string, args []sqldriver.NamedValue) (res sqldriver.Result, err error) {
	if c.bad.Load() {
		return nil, sqldriver.ErrBadConn
	}
	defer c.guard(&err)
	return c.Connection.ExecContext(ctx, query, args)
}

// CheckNamedValue converts json.Number, which the API decodes numbers into:
// go-ora's own check accepts every value, so database/sql never converts it,
// and go-ora cannot bind it. Integers bind as int64, other numbers as exact
// decimal text that Oracle converts implicitly.
func (c *safeConn) CheckNamedValue(nv *sqldriver.NamedValue) error {
	if n, ok := nv.Value.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			nv.Value = i
		} else {
			nv.Value = n.String()
		}
	}
	return c.Connection.CheckNamedValue(nv)
}

func (c *safeConn) ResetSession(ctx context.Context) error {
	if c.bad.Load() {
		return sqldriver.ErrBadConn
	}
	return c.Connection.ResetSession(ctx)
}

// IsValid implements driver.Validator, so a retired connection is never pooled again.
func (c *safeConn) IsValid() bool { return !c.bad.Load() }

func (c *safeConn) Close() error {
	if c.bad.Load() {
		if c.raw != nil {
			return c.raw.Close()
		}
		return nil
	}
	return c.Connection.Close()
}

type safeRows struct {
	*go_ora.DataSet
	conn *safeConn
}

func (r *safeRows) Next(dest []sqldriver.Value) (err error) {
	if r.conn.bad.Load() {
		return sqldriver.ErrBadConn
	}
	defer r.conn.guard(&err)
	return r.DataSet.Next(dest)
}

func (r *safeRows) Close() (err error) {
	if r.conn.bad.Load() {
		return nil
	}
	defer r.conn.guard(&err)
	return r.DataSet.Close()
}
