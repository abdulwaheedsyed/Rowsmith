package snowflake

import (
	"context"

	"rowsmith/internal/driver"
)

// BrowseQuery implements driver.BrowseQuerier for exports and migrations.
func (c *conn) BrowseQuery(_ context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	return c.eng.ExportQuery(t, req)
}

// BeginImport implements driver.BulkImporter: batches of INSERT … SELECT
// inside one transaction.
func (c *conn) BeginImport(ctx context.Context, t *driver.Table, cols []string, empty bool) (driver.RowImporter, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	return c.eng.BeginImport(ctx, t, cols, empty)
}
