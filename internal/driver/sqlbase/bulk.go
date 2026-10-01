package sqlbase

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

// BeginImport implements driver.BulkImporter for engines built on Engine:
// multi-row INSERTs inside one transaction, with values converted exactly as
// grid edits are.
func (e *Engine) BeginImport(ctx context.Context, t *driver.Table, cols []string, empty bool) (driver.RowImporter, error) {
	byName := map[string]*driver.Column{}
	for i := range t.Columns {
		byName[t.Columns[i].Name] = &t.Columns[i]
	}
	im := &importer{e: e, t: t}
	identity, always := false, false
	for _, n := range cols {
		c, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("column %s does not exist", n)
		}
		if c.Generated != "" {
			return nil, fmt.Errorf("column %s is computed and cannot be imported into", n)
		}
		im.cols = append(im.cols, c)
		identity = identity || c.AutoIncrement
		always = always || (c.Default != nil && strings.HasPrefix(strings.ToUpper(*c.Default), "GENERATED ALWAYS"))
	}
	if len(im.cols) == 0 {
		return nil, fmt.Errorf("map at least one column")
	}
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	im.tx = tx
	target := e.D.Qualify(t.Ref)
	names := make([]string, len(im.cols))
	for i, c := range im.cols {
		names[i] = e.D.QuoteIdent(c.Name)
	}
	values := "VALUES"
	switch e.D.Split() {
	case sqlsplit.MSSQL:
		im.maxParams, im.maxRows = 2000, 1000
		if identity {
			im.after = "SET IDENTITY_INSERT " + target + " OFF"
			if _, err := tx.ExecContext(ctx, "SET IDENTITY_INSERT "+target+" ON"); err != nil {
				tx.Rollback()
				return nil, err
			}
		}
	case sqlsplit.Oracle:
		// Oracle has no multi-row VALUES before 23ai; INSERT ALL loads a
		// batch in one statement on every version.
		im.maxParams, im.maxRows, im.insertAll = 4000, 200, true
	case sqlsplit.SQLite:
		im.maxParams, im.maxRows = 30000, 500
	case sqlsplit.Postgres:
		im.maxParams, im.maxRows = 60000, 1000
		if always {
			values = "OVERRIDING SYSTEM VALUE VALUES"
		}
	default:
		im.maxParams, im.maxRows = 60000, 1000
	}
	im.prefix = "INSERT INTO " + target + " (" + strings.Join(names, ", ") + ") " + values + " "
	im.into = "INTO " + target + " (" + strings.Join(names, ", ") + ") VALUES "
	if empty {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+target); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	return im, nil
}

type importer struct {
	e         *Engine
	t         *driver.Table
	tx        *sql.Tx
	cols      []*driver.Column
	prefix    string
	into      string // INSERT ALL form (Oracle)
	insertAll bool
	after     string
	maxParams int
	maxRows   int
}

func (im *importer) Insert(ctx context.Context, rows [][]any) error {
	per := min(im.maxRows, max(1, im.maxParams/len(im.cols)))
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		b := &queryBuilder{d: im.e.D}
		var q strings.Builder
		if im.insertAll {
			q.WriteString("INSERT ALL")
		} else {
			q.WriteString(im.prefix)
		}
		for i, r := range rows[start:end] {
			switch {
			case im.insertAll:
				q.WriteString(" " + im.into)
			case i > 0:
				q.WriteString(", ")
			}
			q.WriteByte('(')
			for j, c := range im.cols {
				if j > 0 {
					q.WriteString(", ")
				}
				var v any
				if j < len(r) {
					v = r[j]
				}
				expr, arg, err := im.e.D.InputExpr(c, v, placeholderToken)
				if err != nil {
					return err
				}
				q.WriteString(b.bind(expr, arg))
			}
			q.WriteByte(')')
		}
		if im.insertAll {
			q.WriteString(" SELECT 1 FROM DUAL")
		}
		if _, err := im.tx.ExecContext(ctx, q.String(), b.args...); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) Commit() error {
	if im.after != "" {
		if _, err := im.tx.Exec(im.after); err != nil {
			im.tx.Rollback()
			return err
		}
	}
	return im.tx.Commit()
}

func (im *importer) Rollback() error { return im.tx.Rollback() }
