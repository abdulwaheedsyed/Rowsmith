package bigquery

import (
	"context"

	"rowsmith/internal/driver"
)

var _ driver.BrowseQuerier = (*conn)(nil)

// BrowseQuery implements driver.BrowseQuerier: the browse query without
// paging, with its named parameters passed through ExecOptions.Params.
// Exports run it as a query job, so the connection's cost guard applies.
func (c *conn) BrowseQuery(ctx context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	tbl, _, err := c.metadata(ctx, req.Ref)
	if err != nil {
		return "", nil, err
	}
	q, params, err := buildQuery(tablePath(tbl.ProjectID, tbl.DatasetID, tbl.TableID), t, req, false, false)
	if err != nil {
		return "", nil, err
	}
	args := make([]any, len(params))
	for i, p := range params {
		args[i] = p
	}
	return q, args, nil
}
