package mongodb

import (
	"context"
	"errors"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"rowsmith/internal/driver"
)

var (
	_ driver.BrowseQuerier = (*conn)(nil)
	_ driver.BulkImporter  = (*conn)(nil)
)

// BrowseQuery renders the browse request as a console find() over the whole
// collection, for exports.
func (c *conn) BrowseQuery(ctx context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	filter, err := c.buildFilter(ctx, req, t)
	if err != nil {
		return "", nil, err
	}
	db := req.Ref.Database
	if db == "" {
		db = c.defaultDB
	}
	var b strings.Builder
	b.WriteString("db.getSiblingDB(" + quoteStr(db) + ").getCollection(" + quoteStr(req.Ref.Name) + ").find(" + shellLiteral(filter))
	if len(req.Columns) > 0 {
		proj := bson.D{}
		for _, n := range req.Columns {
			proj = append(proj, bson.E{Key: n, Value: int32(1)})
		}
		b.WriteString(", " + shellLiteral(proj))
	}
	b.WriteString(")")
	if len(req.Sort) > 0 {
		s := bson.D{}
		for _, x := range req.Sort {
			dir := int32(1)
			if x.Desc {
				dir = -1
			}
			s = append(s, bson.E{Key: x.Column, Value: dir})
		}
		b.WriteString(".sort(" + shellLiteral(s) + ")")
	}
	return b.String(), nil, nil
}

// BeginImport inserts documents in batches. MongoDB has no multi-document
// transaction on standalone servers, so a failed import removes the
// documents it inserted instead of rolling back.
func (c *conn) BeginImport(ctx context.Context, t *driver.Table, cols []string, empty bool) (driver.RowImporter, error) {
	if empty {
		return nil, errors.New("MongoDB imports cannot empty the collection first; empty it separately, then import")
	}
	hints := map[string]string{}
	for _, col := range t.Columns {
		hints[col.Name] = col.Type
	}
	return &docImporter{coll: c.db(t.Ref.Database).Collection(t.Ref.Name), cols: cols, hints: hints}, nil
}

type docImporter struct {
	coll  *mongo.Collection
	cols  []string
	hints map[string]string
	ids   []any
}

func (d *docImporter) Insert(ctx context.Context, rows [][]any) error {
	docs := make([]any, 0, len(rows))
	for _, r := range rows {
		doc := bson.D{}
		var id any
		for i, name := range d.cols {
			if i >= len(r) || r[i] == nil {
				continue // absent field
			}
			if name == driver.OtherFields {
				// Fields outside the known columns (a whole document copied
				// from another collection) join the document as they are.
				if other, ok := r[i].(driver.Doc); ok {
					sub, err := decodeNested(other)
					if err != nil {
						return err
					}
					for _, e := range sub.(bson.D) {
						if e.Key == "_id" {
							id = e.Value
						}
						doc = append(doc, e)
					}
				}
				continue
			}
			v, err := decodeCell(r[i], d.hints[name])
			if err != nil {
				return errors.New(name + ": " + err.Error())
			}
			if name == "_id" {
				id = v
			}
			doc = append(doc, bson.E{Key: name, Value: v})
		}
		if id == nil {
			id = bson.NewObjectID()
			doc = append(bson.D{{Key: "_id", Value: id}}, doc...)
		}
		d.ids = append(d.ids, id)
		docs = append(docs, doc)
	}
	res, err := d.coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(true))
	if err != nil {
		// Keep only the ids that made it in: an ordered insert stops at the
		// first failing document, and a duplicate _id belongs to a document
		// that existed before the import, which the clean-up must not touch.
		inserted := 0
		var bwe mongo.BulkWriteException
		if errors.As(err, &bwe) && len(bwe.WriteErrors) > 0 {
			inserted = bwe.WriteErrors[0].Index
		} else if res != nil {
			inserted = len(res.InsertedIDs)
		}
		d.ids = d.ids[:len(d.ids)-len(docs)+inserted]
		return mapError(err)
	}
	return nil
}

func (d *docImporter) Commit() error { return nil }

func (d *docImporter) Rollback() error {
	for len(d.ids) > 0 {
		n := min(len(d.ids), 1000)
		if _, err := d.coll.DeleteMany(context.Background(), bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: d.ids[:n]}}}}); err != nil {
			return err
		}
		d.ids = d.ids[n:]
	}
	return nil
}
