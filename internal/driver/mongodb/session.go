package mongodb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"rowsmith/internal/driver"
)

// SplitScript implements driver.ScriptSplitter for the console language.
func (mongoDriver) SplitScript(script string) ([]driver.ScriptStatement, error) {
	cmds, err := parseScript(script)
	if err != nil {
		return nil, err
	}
	out := make([]driver.ScriptStatement, len(cmds))
	for i, c := range cmds {
		k := c.kind()
		out[i] = driver.ScriptStatement{SQL: c.text, Start: c.start, End: c.end, Line: c.line, Kind: k, Danger: c.danger(k)}
	}
	return out, nil
}

type session struct {
	c  *conn
	mu sync.Mutex
	db string
}

func (c *conn) NewSession(_ context.Context, s driver.Scope) (driver.Session, error) {
	db := s.Database
	if db == "" {
		db = c.defaultDB
	}
	if db == "" {
		db = "test"
	}
	return &session{c: c, db: db}, nil
}

func (s *session) InTransaction() bool { return false }
func (s *session) Close() error        { return nil }

func (s *session) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	if !s.mu.TryLock() {
		return errors.New("this console is still running a previous command")
	}
	defer s.mu.Unlock()
	cmds, err := parseScript(script)
	if err != nil {
		if e := sink.BeginStatement(driver.StatementInfo{SQL: strings.TrimSpace(script), Line: 1, Kind: driver.StmtUnknown}); e != nil {
			return e
		}
		return sink.EndStatement(&driver.QueryError{Message: err.Error(), Line: lineOf(err)})
	}
	for i, cmd := range cmds {
		k := cmd.kind()
		if err := sink.BeginStatement(driver.StatementInfo{Index: i, SQL: cmd.text, Line: cmd.line, Kind: k}); err != nil {
			return err
		}
		var runErr error
		if (opts.ReadOnly || s.c.ro) && !k.Safe() {
			runErr = &driver.QueryError{Message: "Blocked: this connection is read-only for you, and the command may modify data (" + string(k) + ")."}
		} else {
			runErr = s.run(ctx, cmd, opts, sink)
		}
		if runErr != nil {
			runErr = mapError(runErr)
			var qe *driver.QueryError
			if !errors.As(runErr, &qe) {
				runErr = &driver.QueryError{Message: runErr.Error()}
			}
		}
		if ctx.Err() != nil && runErr != nil {
			runErr = &driver.QueryError{Message: "Cancelled"}
		}
		if err := sink.EndStatement(runErr); err != nil {
			return err
		}
		if runErr != nil && (opts.StopOnError || ctx.Err() != nil) {
			break
		}
	}
	return nil
}

func lineOf(err error) int {
	if m := lineRe.FindStringSubmatch(err.Error()); m != nil {
		var n int
		fmt.Sscanf(m[1], "%d", &n)
		return n
	}
	return 0
}

func argDoc(args []any, i int) (bson.D, error) {
	if i >= len(args) || args[i] == nil {
		return bson.D{}, nil
	}
	d, ok := args[i].(bson.D)
	if !ok {
		return nil, fmt.Errorf("argument %d must be a document", i+1)
	}
	return d, nil
}

func argArray(args []any, i int) (bson.A, error) {
	if i >= len(args) {
		return bson.A{}, nil
	}
	a, ok := args[i].(bson.A)
	if !ok {
		return nil, fmt.Errorf("argument %d must be an array", i+1)
	}
	return a, nil
}

func argInt(args []any, i int) int64 {
	if i >= len(args) {
		return 0
	}
	return toInt64(args[i])
}

func (s *session) run(ctx context.Context, cmd command, opts driver.ExecOptions, sink driver.Sink) error {
	start := time.Now()
	db := s.c.client.Database(s.db)
	if cmd.use != "" {
		s.db = cmd.use
		sink.Notice("info", "switched to db "+cmd.use)
		return sink.EndResult(driver.ResultSummary{DurationMS: ms(start)})
	}
	if cmd.show != "" {
		switch cmd.show {
		case "dbs", "databases":
			res, err := s.c.client.ListDatabases(ctx, bson.D{})
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(res.Databases))
			for _, d := range res.Databases {
				rows = append(rows, []any{d.Name, d.SizeOnDisk, d.Empty})
			}
			return emitTable(sink, []string{"name", "sizeOnDisk", "empty"}, rows, start)
		case "collections", "tables":
			names, err := db.ListCollectionNames(ctx, bson.D{})
			if err != nil {
				return err
			}
			sort.Strings(names)
			rows := make([][]any, len(names))
			for i, n := range names {
				rows[i] = []any{n}
			}
			return emitTable(sink, []string{"collection"}, rows, start)
		case "users":
			return s.runCommand(ctx, db, bson.D{{Key: "usersInfo", Value: 1}}, sink, start)
		case "roles":
			return s.runCommand(ctx, db, bson.D{{Key: "rolesInfo", Value: 1}}, sink, start)
		case "profile":
			cur, err := db.Collection("system.profile").Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "ts", Value: -1}}).SetLimit(100))
			if err != nil {
				return err
			}
			return streamCursor(ctx, cur, opts.MaxRows, sink, start)
		}
		return fmt.Errorf("unknown show command %q (try dbs, collections, users, roles, profile)", cmd.show)
	}
	if cmd.target == "" {
		return s.dbCall(ctx, db, cmd.calls, opts, sink, start)
	}
	return s.collCall(ctx, db.Collection(cmd.target), cmd.calls, opts, sink, start)
}

func (s *session) runCommand(ctx context.Context, db *mongo.Database, doc bson.D, sink driver.Sink, start time.Time) error {
	var out bson.D
	if err := db.RunCommand(ctx, doc).Decode(&out); err != nil {
		return err
	}
	return emitDocs(sink, []bson.D{out}, start)
}

func (s *session) dbCall(ctx context.Context, db *mongo.Database, calls []call, opts driver.ExecOptions, sink driver.Sink, start time.Time) error {
	c := calls[0]
	switch c.name {
	case "runCommand", "adminCommand":
		d, err := argDoc(c.args, 0)
		if err != nil {
			return err
		}
		if c.name == "adminCommand" {
			db = s.c.client.Database("admin")
		}
		return s.runCommand(ctx, db, d, sink, start)
	case "getCollectionNames":
		names, err := db.ListCollectionNames(ctx, bson.D{})
		if err != nil {
			return err
		}
		sort.Strings(names)
		rows := make([][]any, len(names))
		for i, n := range names {
			rows[i] = []any{n}
		}
		return emitTable(sink, []string{"collection"}, rows, start)
	case "getCollectionInfos":
		f, err := argDoc(c.args, 0)
		if err != nil {
			return err
		}
		cur, err := db.ListCollections(ctx, f)
		if err != nil {
			return err
		}
		return streamCursor(ctx, cur, opts.MaxRows, sink, start)
	case "createCollection":
		name, _ := firstString(c.args)
		o, err := argDoc(c.args, 1)
		if err != nil {
			return err
		}
		return s.runCommand(ctx, db, append(bson.D{{Key: "create", Value: name}}, o...), sink, start)
	case "createView":
		name, _ := firstString(c.args)
		src := ""
		if len(c.args) > 1 {
			src, _ = c.args[1].(string)
		}
		p, err := argArray(c.args, 2)
		if err != nil {
			return err
		}
		return s.runCommand(ctx, db, bson.D{{Key: "create", Value: name}, {Key: "viewOn", Value: src}, {Key: "pipeline", Value: p}}, sink, start)
	case "dropDatabase":
		if err := db.Drop(ctx); err != nil {
			return err
		}
		sink.Notice("info", "dropped database "+db.Name())
		return sink.EndResult(driver.ResultSummary{DurationMS: ms(start)})
	case "stats":
		return s.runCommand(ctx, db, bson.D{{Key: "dbStats", Value: 1}}, sink, start)
	case "serverStatus":
		return s.runCommand(ctx, s.c.client.Database("admin"), bson.D{{Key: "serverStatus", Value: 1}}, sink, start)
	case "hostInfo":
		return s.runCommand(ctx, s.c.client.Database("admin"), bson.D{{Key: "hostInfo", Value: 1}}, sink, start)
	case "currentOp":
		return s.runCommand(ctx, s.c.client.Database("admin"), bson.D{{Key: "currentOp", Value: 1}}, sink, start)
	case "killOp":
		return s.runCommand(ctx, s.c.client.Database("admin"), bson.D{{Key: "killOp", Value: 1}, {Key: "op", Value: argInt(c.args, 0)}}, sink, start)
	case "version":
		return emitTable(sink, []string{"version"}, [][]any{{s.c.version}}, start)
	case "getName":
		return emitTable(sink, []string{"database"}, [][]any{{db.Name()}}, start)
	case "aggregate":
		p, err := argArray(c.args, 0)
		if err != nil {
			return err
		}
		cur, err := db.Aggregate(ctx, p)
		if err != nil {
			return err
		}
		return streamCursor(ctx, cur, opts.MaxRows, sink, start)
	}
	return fmt.Errorf("db.%s() is not supported", c.name)
}

func (s *session) collCall(ctx context.Context, coll *mongo.Collection, calls []call, opts driver.ExecOptions, sink driver.Sink, start time.Time) error {
	c := calls[0]
	rest := calls[1:]
	switch c.name {
	case "find", "findOne":
		f, err := argDoc(c.args, 0)
		if err != nil {
			return err
		}
		fo := options.Find()
		if len(c.args) > 1 {
			p, err := argDoc(c.args, 1)
			if err != nil {
				return err
			}
			fo.SetProjection(p)
		}
		var explain string
		var countOnly bool
		limit := int64(0)
		for _, m := range rest {
			switch m.name {
			case "sort":
				d, err := argDoc(m.args, 0)
				if err != nil {
					return err
				}
				fo.SetSort(d)
			case "limit":
				limit = argInt(m.args, 0)
				fo.SetLimit(limit)
			case "skip":
				fo.SetSkip(argInt(m.args, 0))
			case "project", "projection":
				d, err := argDoc(m.args, 0)
				if err != nil {
					return err
				}
				fo.SetProjection(d)
			case "hint":
				if len(m.args) > 0 {
					fo.SetHint(m.args[0])
				}
			case "maxTimeMS":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(argInt(m.args, 0))*time.Millisecond)
				defer cancel()
			case "collation":
				d, err := argDoc(m.args, 0)
				if err != nil {
					return err
				}
				var col options.Collation
				if b, err := bson.Marshal(d); err == nil {
					_ = bson.Unmarshal(b, &col)
				}
				fo.SetCollation(&col)
			case "comment":
				if len(m.args) > 0 {
					fo.SetComment(m.args[0])
				}
			case "batchSize":
				fo.SetBatchSize(int32(argInt(m.args, 0)))
			case "count", "size", "itcount":
				countOnly = true
			case "explain":
				explain = "queryPlanner"
				if v, ok := firstString(m.args); ok {
					explain = v
				}
			case "toArray", "pretty":
			default:
				return fmt.Errorf("cursor method %s() is not supported", m.name)
			}
		}
		if c.name == "findOne" {
			fo.SetLimit(1)
		}
		if explain != "" {
			return s.runCommand(ctx, coll.Database(), bson.D{{Key: "explain", Value: bson.D{{Key: "find", Value: coll.Name()}, {Key: "filter", Value: f}}}, {Key: "verbosity", Value: explain}}, sink, start)
		}
		if countOnly {
			n, err := coll.CountDocuments(ctx, f)
			if err != nil {
				return err
			}
			return emitTable(sink, []string{"count"}, [][]any{{n}}, start)
		}
		cur, err := coll.Find(ctx, f, fo)
		if err != nil {
			return err
		}
		return streamCursor(ctx, cur, opts.MaxRows, sink, start)
	case "aggregate":
		p, err := argArray(c.args, 0)
		if err != nil {
			return err
		}
		for _, m := range rest {
			if m.name == "explain" {
				v, ok := firstString(m.args)
				if !ok {
					v = "queryPlanner"
				}
				return s.runCommand(ctx, coll.Database(), bson.D{{Key: "explain", Value: bson.D{{Key: "aggregate", Value: coll.Name()}, {Key: "pipeline", Value: p}, {Key: "cursor", Value: bson.D{}}}}, {Key: "verbosity", Value: v}}, sink, start)
			}
		}
		cur, err := coll.Aggregate(ctx, p, options.Aggregate().SetAllowDiskUse(true))
		if err != nil {
			return err
		}
		return streamCursor(ctx, cur, opts.MaxRows, sink, start)
	case "countDocuments", "count":
		f, err := argDoc(c.args, 0)
		if err != nil {
			return err
		}
		n, err := coll.CountDocuments(ctx, f)
		if err != nil {
			return err
		}
		return emitTable(sink, []string{"count"}, [][]any{{n}}, start)
	case "estimatedDocumentCount":
		n, err := coll.EstimatedDocumentCount(ctx)
		if err != nil {
			return err
		}
		return emitTable(sink, []string{"count"}, [][]any{{n}}, start)
	case "distinct":
		field, _ := firstString(c.args)
		f, err := argDoc(c.args, 1)
		if err != nil {
			return err
		}
		res := coll.Distinct(ctx, field, f)
		if err := res.Err(); err != nil {
			return err
		}
		var vals bson.A
		if err := res.Decode(&vals); err != nil {
			return err
		}
		rows := make([][]any, len(vals))
		for i, v := range vals {
			rows[i] = []any{encode(v)}
		}
		return emitTable(sink, []string{field}, rows, start)
	case "insertOne":
		d, err := argDoc(c.args, 0)
		if err != nil {
			return err
		}
		r, err := coll.InsertOne(ctx, d)
		if err != nil {
			return err
		}
		sink.Notice("info", fmt.Sprintf("inserted _id %v", describeID(r.InsertedID)))
		one := int64(1)
		return sink.EndResult(driver.ResultSummary{RowsAffected: &one, DurationMS: ms(start)})
	case "insertMany":
		a, err := argArray(c.args, 0)
		if err != nil {
			return err
		}
		docs := make([]any, len(a))
		copy(docs, a)
		r, err := coll.InsertMany(ctx, docs)
		if err != nil {
			return err
		}
		n := int64(len(r.InsertedIDs))
		return sink.EndResult(driver.ResultSummary{RowsAffected: &n, DurationMS: ms(start)})
	case "updateOne", "updateMany", "replaceOne":
		f, err := argDoc(c.args, 0)
		if err != nil {
			return err
		}
		if len(c.args) < 2 {
			return fmt.Errorf("%s needs a filter and an update", c.name)
		}
		o, err := argDoc(c.args, 2)
		if err != nil {
			return err
		}
		upsert := false
		for _, e := range o {
			if e.Key == "upsert" {
				upsert, _ = e.Value.(bool)
			}
		}
		var r *mongo.UpdateResult
		switch c.name {
		case "updateOne":
			r, err = coll.UpdateOne(ctx, f, c.args[1], options.UpdateOne().SetUpsert(upsert))
		case "updateMany":
			r, err = coll.UpdateMany(ctx, f, c.args[1], options.UpdateMany().SetUpsert(upsert))
		default:
			r, err = coll.ReplaceOne(ctx, f, c.args[1], options.Replace().SetUpsert(upsert))
		}
		if err != nil {
			return err
		}
		sink.Notice("info", fmt.Sprintf("matched %d, modified %d, upserted %d", r.MatchedCount, r.ModifiedCount, r.UpsertedCount))
		n := r.ModifiedCount + r.UpsertedCount
		return sink.EndResult(driver.ResultSummary{RowsAffected: &n, DurationMS: ms(start)})
	case "deleteOne", "deleteMany":
		f, err := argDoc(c.args, 0)
		if err != nil {
			return err
		}
		var r *mongo.DeleteResult
		if c.name == "deleteOne" {
			r, err = coll.DeleteOne(ctx, f)
		} else {
			r, err = coll.DeleteMany(ctx, f)
		}
		if err != nil {
			return err
		}
		n := r.DeletedCount
		return sink.EndResult(driver.ResultSummary{RowsAffected: &n, DurationMS: ms(start)})
	case "findOneAndUpdate", "findOneAndReplace", "findOneAndDelete":
		f, err := argDoc(c.args, 0)
		if err != nil {
			return err
		}
		var res *mongo.SingleResult
		switch c.name {
		case "findOneAndDelete":
			res = coll.FindOneAndDelete(ctx, f)
		case "findOneAndReplace":
			if len(c.args) < 2 {
				return errors.New("findOneAndReplace needs a replacement document")
			}
			res = coll.FindOneAndReplace(ctx, f, c.args[1], options.FindOneAndReplace().SetReturnDocument(options.After))
		default:
			if len(c.args) < 2 {
				return errors.New("findOneAndUpdate needs an update")
			}
			res = coll.FindOneAndUpdate(ctx, f, c.args[1], options.FindOneAndUpdate().SetReturnDocument(options.After))
		}
		var d bson.D
		if err := res.Decode(&d); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return emitDocs(sink, nil, start)
			}
			return err
		}
		return emitDocs(sink, []bson.D{d}, start)
	case "createIndex":
		keys, err := argDoc(c.args, 0)
		if err != nil {
			return err
		}
		o, err := argDoc(c.args, 1)
		if err != nil {
			return err
		}
		spec := append(bson.D{{Key: "key", Value: keys}}, o...)
		hasName := false
		for _, e := range spec {
			hasName = hasName || e.Key == "name"
		}
		if !hasName {
			var parts []string
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s_%v", k.Key, k.Value))
			}
			spec = append(spec, bson.E{Key: "name", Value: strings.Join(parts, "_")})
		}
		return s.runCommand(ctx, coll.Database(), bson.D{{Key: "createIndexes", Value: coll.Name()}, {Key: "indexes", Value: bson.A{spec}}}, sink, start)
	case "dropIndex":
		if len(c.args) == 0 {
			return errors.New("dropIndex needs an index name or key pattern")
		}
		return s.runCommand(ctx, coll.Database(), bson.D{{Key: "dropIndexes", Value: coll.Name()}, {Key: "index", Value: c.args[0]}}, sink, start)
	case "dropIndexes":
		return s.runCommand(ctx, coll.Database(), bson.D{{Key: "dropIndexes", Value: coll.Name()}, {Key: "index", Value: "*"}}, sink, start)
	case "getIndexes", "getIndexKeys":
		cur, err := coll.Indexes().List(ctx)
		if err != nil {
			return err
		}
		return streamCursor(ctx, cur, opts.MaxRows, sink, start)
	case "drop":
		if err := coll.Drop(ctx); err != nil {
			return err
		}
		sink.Notice("info", "dropped collection "+coll.Name())
		return sink.EndResult(driver.ResultSummary{DurationMS: ms(start)})
	case "renameCollection":
		to, _ := firstString(c.args)
		if to == "" {
			return errors.New("renameCollection needs the new name")
		}
		from := coll.Database().Name() + "." + coll.Name()
		return s.runCommand(ctx, s.c.client.Database("admin"), bson.D{{Key: "renameCollection", Value: from}, {Key: "to", Value: coll.Database().Name() + "." + to}}, sink, start)
	case "stats":
		cur, err := coll.Aggregate(ctx, mongo.Pipeline{{{Key: "$collStats", Value: bson.D{{Key: "storageStats", Value: bson.D{}}, {Key: "count", Value: bson.D{}}}}}})
		if err != nil {
			return err
		}
		return streamCursor(ctx, cur, opts.MaxRows, sink, start)
	}
	return fmt.Errorf("%s() is not supported on collections", c.name)
}

func describeID(v any) string {
	switch x := v.(type) {
	case bson.ObjectID:
		return "ObjectId(\"" + x.Hex() + "\")"
	}
	return fmt.Sprint(v)
}

func ms(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000 }

func emitTable(sink driver.Sink, cols []string, rows [][]any, start time.Time) error {
	rc := make([]driver.ResultColumn, len(cols))
	for i, c := range cols {
		k := driver.KindOther
		if len(rows) > 0 {
			switch rows[0][i].(type) {
			case string:
				k = driver.KindString
			case int64, int32, int:
				k = driver.KindInt
			case bool:
				k = driver.KindBool
			}
		}
		rc[i] = driver.ResultColumn{Name: c, Type: string(k), Kind: k}
	}
	for _, r := range rows {
		for i, v := range r {
			r[i] = encode(v)
		}
	}
	if err := sink.Columns(rc); err != nil {
		return err
	}
	if len(rows) > 0 {
		if err := sink.Rows(rows); err != nil {
			return err
		}
	}
	return sink.EndResult(driver.ResultSummary{RowCount: int64(len(rows)), DurationMS: ms(start)})
}

func emitDocs(sink driver.Sink, docs []bson.D, start time.Time) error {
	cols := columnsOf(docs)
	rc, rows := docsToRows(docs, cols, inferredTable(docs))
	if err := sink.Columns(rc); err != nil {
		return err
	}
	if len(rows) > 0 {
		if err := sink.Rows(rows); err != nil {
			return err
		}
	}
	return sink.EndResult(driver.ResultSummary{RowCount: int64(len(rows)), DurationMS: ms(start)})
}

func columnsOf(docs []bson.D) []string {
	fields := inferFields(docs)
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = f.name
	}
	return out
}

func inferredTable(docs []bson.D) *driver.Table {
	t := &driver.Table{}
	for _, f := range inferFields(docs) {
		typ := f.dominant()
		t.Columns = append(t.Columns, driver.Column{Name: f.name, Type: typ, Kind: kindOf(typ)})
	}
	return t
}

// streamCursor infers columns from the first documents, then streams the rest.
func streamCursor(ctx context.Context, cur *mongo.Cursor, maxRows int, sink driver.Sink, start time.Time) error {
	defer cur.Close(context.Background())
	if maxRows <= 0 {
		maxRows = 1000
	}
	head := min(maxRows, 1000)
	var docs []bson.D
	for len(docs) < head && cur.Next(ctx) {
		var d bson.D
		if err := cur.Decode(&d); err != nil {
			return err
		}
		docs = append(docs, d)
	}
	if err := cur.Err(); err != nil {
		return err
	}
	cols := columnsOf(docs)
	t := inferredTable(docs)
	rc, rows := docsToRows(docs, cols, t)
	// Keep a stable column set so later batches line up: always carry the
	// trailing "…" column once any document had extra fields.
	if err := sink.Columns(append(rc[:len(cols):len(cols)], driver.ResultColumn{Name: "…", Type: "other fields", Kind: driver.KindObject})); err != nil {
		return err
	}
	pad := func(rs [][]any) [][]any {
		for i, r := range rs {
			if len(r) == len(cols) {
				rs[i] = append(r, nil)
			}
		}
		return rs
	}
	if len(rows) > 0 {
		if err := sink.Rows(pad(rows)); err != nil {
			return err
		}
	}
	count := int64(len(rows))
	truncated := false
	var batch []bson.D
	for cur.Next(ctx) {
		if count >= int64(maxRows) {
			truncated = true
			break
		}
		var d bson.D
		if err := cur.Decode(&d); err != nil {
			return err
		}
		batch = append(batch, d)
		count++
		if len(batch) >= 500 {
			_, br := docsToRows(batch, cols, t)
			if err := sink.Rows(pad(br)); err != nil {
				return err
			}
			batch = nil
		}
	}
	if len(batch) > 0 {
		_, br := docsToRows(batch, cols, t)
		if err := sink.Rows(pad(br)); err != nil {
			return err
		}
	}
	if err := cur.Err(); err != nil && !truncated {
		return err
	}
	return sink.EndResult(driver.ResultSummary{RowCount: count, Truncated: truncated, DurationMS: ms(start)})
}
