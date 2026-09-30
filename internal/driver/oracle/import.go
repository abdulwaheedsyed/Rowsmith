package oracle

import (
	"context"

	"rowsmith/internal/driver"
)

var _ driver.BulkImporter = (*conn)(nil)

// BeginImport implements driver.BulkImporter.
func (c *conn) BeginImport(ctx context.Context, t *driver.Table, cols []string, empty bool) (driver.RowImporter, error) {
	return c.eng.BeginImport(ctx, t, cols, empty)
}
