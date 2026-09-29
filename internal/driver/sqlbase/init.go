package sqlbase

import (
	"context"
	sqldriver "database/sql/driver"
)

// InitConnector runs setup statements on every new physical connection, e.g.
// switching the session to read-only.
type InitConnector struct {
	Inner sqldriver.Connector
	Init  []string
}

func (c *InitConnector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	conn, err := c.Inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if len(c.Init) == 0 {
		return conn, nil
	}
	ex, ok := conn.(sqldriver.ExecerContext)
	if !ok {
		conn.Close()
		return nil, errNoExecer
	}
	for _, q := range c.Init {
		if _, err := ex.ExecContext(ctx, q, nil); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func (c *InitConnector) Driver() sqldriver.Driver { return c.Inner.Driver() }

type initErr string

func (e initErr) Error() string { return string(e) }

const errNoExecer = initErr("driver connection cannot run initialization statements")
