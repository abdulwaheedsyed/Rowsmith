package sqlite

import (
	"context"

	"rowsmith/internal/driver"
)

var _ driver.BrowseQuerier = (*conn)(nil)

// BrowseQuery implements driver.BrowseQuerier for whole-table exports.
func (c *conn) BrowseQuery(_ context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	return c.eng.ExportQuery(t, req)
}
