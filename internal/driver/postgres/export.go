package postgres

import (
	"context"

	"rowsmith/internal/driver"
)

var _ driver.BrowseQuerier = (*conn)(nil)

// BrowseQuery implements driver.BrowseQuerier for whole-table exports.
func (c *conn) BrowseQuery(ctx context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	p, err := c.pool(ctx, t.Ref.Database)
	if err != nil {
		return "", nil, err
	}
	return p.eng.ExportQuery(t, req)
}
