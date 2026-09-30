// Package driver defines the contract every database engine implements.
//
// A Driver describes itself (connection form, capabilities, object kinds) and
// opens Conns. A Conn exposes a uniform catalog, browsing, editing and
// execution API. Engine-specific extras (EXPLAIN, process lists, users, DDL
// generation, dumps) are optional interfaces discovered with type assertions,
// so adding an engine only requires implementing what it supports.
package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// DialFunc opens a network connection. When a connection is tunneled over
// SSH, drivers receive a DialFunc that routes through the tunnel.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

type Driver interface {
	Info() Info
	Open(ctx context.Context, p OpenParams) (Conn, error)
}

type OpenParams struct {
	Params   map[string]any    // non-secret form values
	Secrets  map[string]string // decrypted secret form values
	Dial     DialFunc          // nil means dial directly
	ReadOnly bool              // engine-level read-only session where supported
	AppName  string            // reported to servers that accept an application name
}

func (p OpenParams) String(key string) string {
	switch v := p.Params[key].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(int64(v))
	case int:
		return fmt.Sprint(v)
	case bool:
		return fmt.Sprint(v)
	}
	if s, ok := p.Secrets[key]; ok {
		return s
	}
	return ""
}

func (p OpenParams) Int(key string, def int) int {
	switch v := p.Params[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func (p OpenParams) Bool(key string) bool {
	switch v := p.Params[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1" || v == "yes"
	}
	return false
}

func (p OpenParams) Secret(key string) string { return p.Secrets[key] }

// Conn is a live, pooled handle to a server. Implementations must be safe for
// concurrent use; long operations must honor ctx cancellation.
type Conn interface {
	Close() error
	Ping(ctx context.Context) error
	Server(ctx context.Context) (*ServerInfo, error)

	// Catalog. Scope fields are ignored at levels the engine does not have.
	Databases(ctx context.Context) ([]Database, error)
	Schemas(ctx context.Context, database string) ([]Schema, error)
	Objects(ctx context.Context, scope Scope) ([]Object, error)
	Describe(ctx context.Context, ref ObjectRef) (*Table, error)

	// Data.
	Browse(ctx context.Context, req BrowseRequest) (*Result, error)
	Count(ctx context.Context, req BrowseRequest) (Count, error)
	ApplyEdits(ctx context.Context, ref ObjectRef, edits []RowEdit) (*EditResult, error)

	// NewSession pins a dedicated server connection for the SQL console so
	// that transactions and session state survive between runs.
	NewSession(ctx context.Context, scope Scope) (Session, error)
}

type Session interface {
	Execute(ctx context.Context, script string, opts ExecOptions, sink Sink) error
	InTransaction() bool
	Close() error
}

// ---- optional capabilities --------------------------------------------------

// DDLGenerator produces statements for structural changes. The UI always
// previews generated SQL before running it through a Session.
type DDLGenerator interface {
	CreateTableSQL(def TableDef) ([]string, error)
	AlterTableSQL(from *Table, to TableDef) ([]string, error)
	DropObjectSQL(ref ObjectRef, cascade bool) ([]string, error)
	TruncateSQL(ref ObjectRef) ([]string, error)
	RenameObjectSQL(ref ObjectRef, newName string) ([]string, error)
	CreateDatabaseSQL(name string, opts map[string]string) ([]string, error)
	DropDatabaseSQL(name string) ([]string, error)
}

type SchemaDDL interface {
	CreateSchemaSQL(database, name string) ([]string, error)
	DropSchemaSQL(database, name string, cascade bool) ([]string, error)
}

type Explainer interface {
	Explain(ctx context.Context, scope Scope, statement string, analyze bool) (*Plan, error)
}

type ProcessManager interface {
	Processes(ctx context.Context) (*Result, error)
	KillProcess(ctx context.Context, id string) error
}

type VariablesReader interface {
	Variables(ctx context.Context, kind string) (*Result, error) // kind: "variables" | "status"
}

type UserManager interface {
	Users(ctx context.Context) (*Result, error)
	UserGrants(ctx context.Context, user string) ([]string, error)
}

// Catalog returns column metadata for a whole scope in one round trip, used
// for editor autocompletion and ER diagrams.
type Catalog interface {
	CatalogColumns(ctx context.Context, scope Scope) ([]CatalogTable, error)
}

// Definer returns the source of views, routines, triggers and similar objects.
type Definer interface {
	Definition(ctx context.Context, ref ObjectRef) (string, error)
}

// BrowseQuerier returns a console statement (with bind arguments) that
// selects every row a browse request matches, in order and without paging.
// Exports run it through a Session to stream whole tables.
type BrowseQuerier interface {
	BrowseQuery(ctx context.Context, t *Table, req BrowseRequest) (query string, args []any, err error)
}

// BulkImporter loads rows into a table inside one transaction.
type BulkImporter interface {
	// BeginImport starts loading into cols of t; empty deletes existing rows first.
	BeginImport(ctx context.Context, t *Table, cols []string, empty bool) (RowImporter, error)
}

// RowImporter receives rows whose values are in column order and shaped like
// grid edits (strings, numbers, booleans, nil, {"$bin": …}, GeoJSON…).
type RowImporter interface {
	Insert(ctx context.Context, rows [][]any) error
	Commit() error
	Rollback() error
}

// Dumper writes a portable script (DDL and/or data) for export.
type Dumper interface {
	Dump(ctx context.Context, req DumpRequest, w DumpWriter) error
}

// Classifier lets an engine decide how dangerous a statement is. Engines
// without it fall back to the generic SQL classifier.
type Classifier interface {
	Classify(statement string) StatementKind
}

// ScriptStatement is one statement of a console script.
type ScriptStatement struct {
	SQL    string        `json:"sql"`
	Start  int           `json:"start"` // byte offsets within the script
	End    int           `json:"end"`
	Line   int           `json:"line"`
	Kind   StatementKind `json:"kind"`
	Danger Danger        `json:"danger"`
}

// ScriptSplitter is implemented by drivers whose console language is not
// SQL (e.g. the MongoDB shell); the API uses it for "run statement at
// cursor", editor markers and confirmation prompts.
type ScriptSplitter interface {
	SplitScript(script string) ([]ScriptStatement, error)
}

// ---- registry --------------------------------------------------------------

var (
	regMu    sync.RWMutex
	registry = map[string]Driver{}
)

// Register makes a driver available. It is called from each driver package's init.
func Register(d Driver) {
	regMu.Lock()
	defer regMu.Unlock()
	id := d.Info().ID
	if _, dup := registry[id]; dup {
		panic("driver: duplicate registration of " + id)
	}
	registry[id] = d
}

func Get(id string) (Driver, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	d, ok := registry[id]
	return d, ok
}

func All() []Info {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Info, 0, len(registry))
	for _, d := range registry {
		out = append(out, d.Info())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Order < out[j].Order || (out[i].Order == out[j].Order && out[i].Name < out[j].Name)
	})
	return out
}

// ---- errors ----------------------------------------------------------------

var (
	ErrNotSupported = errors.New("operation not supported by this database")
	ErrReadOnly     = errors.New("this session is read-only")
	ErrNoRowKey     = errors.New("table has no primary key or unique index; rows cannot be edited safely")
)

// QueryError carries engine error details the editor can point at.
type QueryError struct {
	Message  string `json:"message"`
	Code     string `json:"code,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Position int    `json:"position,omitempty"` // 1-based character offset within the statement
	Line     int    `json:"line,omitempty"`
}

func (e *QueryError) Error() string { return e.Message }

// Timeout applied to catalog calls so a hung server cannot pin a request forever.
const CatalogTimeout = 60 * time.Second
