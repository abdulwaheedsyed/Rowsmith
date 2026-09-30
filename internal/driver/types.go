package driver

import (
	"encoding/json"
	"io"
	"time"
)

// ---- self-description --------------------------------------------------------

type FieldType string

const (
	FieldText     FieldType = "text"
	FieldPassword FieldType = "password"
	FieldNumber   FieldType = "number"
	FieldSelect   FieldType = "select"
	FieldTextarea FieldType = "textarea"
	FieldFile     FieldType = "file" // file contents are read client-side and stored as a secret/param
	FieldBool     FieldType = "bool"
)

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Field describes one input of a driver's connection form.
type Field struct {
	Key         string              `json:"key"`
	Label       string              `json:"label"`
	Type        FieldType           `json:"type"`
	Required    bool                `json:"required,omitempty"`
	Secret      bool                `json:"secret,omitempty"`
	Default     any                 `json:"default,omitempty"`
	Placeholder string              `json:"placeholder,omitempty"`
	Help        string              `json:"help,omitempty"`
	Options     []Option            `json:"options,omitempty"`
	Section     string              `json:"section,omitempty"` // "", "auth", "tls", "advanced"
	Span        int                 `json:"span,omitempty"`    // grid columns out of 6 (default 6)
	ShowIf      map[string][]string `json:"showIf,omitempty"`  // show when another field has one of these values
}

type Caps struct {
	Databases      bool `json:"databases"`      // server hosts several databases
	Schemas        bool `json:"schemas"`        // namespaces inside a database
	SQL            bool `json:"sql"`            // console speaks SQL (vs. an engine-specific language)
	Transactions   bool `json:"transactions"`   //
	EditRows       bool `json:"editRows"`       //
	DDL            bool `json:"ddl"`            // structure editing
	CreateDatabase bool `json:"createDatabase"` //
	ForeignKeys    bool `json:"foreignKeys"`    //
	Explain        bool `json:"explain"`        //
	Processes      bool `json:"processes"`      //
	Variables      bool `json:"variables"`      //
	Users          bool `json:"users"`          //
	Geometry       bool `json:"geometry"`       // spatial columns can be rendered on a map
	Documents      bool `json:"documents"`      // rows are JSON documents
	Dump           bool `json:"dump"`           //
	CostEstimate   bool `json:"costEstimate"`   // dry-run reports bytes processed (BigQuery)
}

type KindInfo struct {
	Kind   string `json:"kind"`
	Label  string `json:"label"`  // plural, e.g. "Tables"
	Icon   string `json:"icon"`   // hint for the UI icon set
	Browse bool   `json:"browse"` // has rows
}

type Info struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Order       int        `json:"order"`
	Dialect     string     `json:"dialect"` // editor language: mysql, postgresql, mssql, plsql, sqlite, bigquery, mongodb
	DefaultPort int        `json:"defaultPort,omitempty"`
	Fields      []Field    `json:"fields"`
	SSH         bool       `json:"ssh"`
	Caps        Caps       `json:"caps"`
	Kinds       []KindInfo `json:"kinds"`
	Types       []string   `json:"types"`      // data types offered by the column editor
	URLSchemes  []string   `json:"urlSchemes"` // connection URLs the form can parse
	QuoteChar   string     `json:"quoteChar"`  // identifier quote for client-side snippets
	// Design describes the structure editor for this engine; nil hides it.
	Design *TableDesign `json:"design,omitempty"`
}

// TableDesign tells the structure editor which parts of a TableDef an engine
// can create and change. The editor only offers what is listed here, and the
// engine's DDLGenerator must accept everything it offers.
type TableDesign struct {
	Columns          bool `json:"columns"`        // columns can be designed (false for document stores: indexes only)
	ReorderColumns   bool `json:"reorderColumns"` // existing columns can be moved in place (new tables are always free-form)
	AutoIncrement    bool `json:"autoIncrement"`  // Column.AutoIncrement: engine-native auto-numbering
	ColumnComments   bool `json:"columnComments"`
	TableComment     bool `json:"tableComment"`
	Collation        bool `json:"collation"`        // Column.Collation
	Generated        bool `json:"generated"`        // computed columns (Column.Generated)
	GeneratedVirtual bool `json:"generatedVirtual"` // VIRTUAL computed columns (Column.GeneratedStored=false)
	GeneratedStored  bool `json:"generatedStored"`  // STORED/PERSISTED computed columns
	OnUpdate         bool `json:"onUpdate"`         // Column.OnUpdate (MySQL)
	Checks           bool `json:"checks"`
	PrimaryKey       bool `json:"primaryKey"`
	ForeignKeys      bool `json:"foreignKeys"`
	Indexes          bool `json:"indexes"`
	PartialIndexes   bool `json:"partialIndexes"` // Index.Where
	IndexLengths     bool `json:"indexLengths"`   // Index.Lengths (prefix indexes)
	// IndexTypes are the methods offered for Index.Type; the first is the default.
	IndexTypes []string `json:"indexTypes,omitempty"`
	// FKActions are the rules offered for ON DELETE / ON UPDATE.
	FKActions []string `json:"fkActions,omitempty"`
	// Options are table-level settings stored in TableDef.Options (e.g. engine).
	Options []Field `json:"options,omitempty"`
	// Note is shown in the editor, e.g. how the engine applies changes.
	Note string `json:"note,omitempty"`
}

// ---- catalog -----------------------------------------------------------------

type ServerInfo struct {
	Product  string            `json:"product"`
	Version  string            `json:"version"`
	User     string            `json:"user"`
	Database string            `json:"database,omitempty"`
	Extras   map[string]string `json:"extras,omitempty"` // e.g. "PostGIS": "3.5"
}

type Scope struct {
	Database string `json:"database,omitempty"`
	Schema   string `json:"schema,omitempty"`
}

type ObjectRef struct {
	Database string `json:"database,omitempty"`
	Schema   string `json:"schema,omitempty"`
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
}

func (r ObjectRef) Scope() Scope { return Scope{Database: r.Database, Schema: r.Schema} }

type Database struct {
	Name      string `json:"name"`
	Size      *int64 `json:"size,omitempty"`
	Collation string `json:"collation,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Tables    *int64 `json:"tables,omitempty"`
	System    bool   `json:"system,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

type Schema struct {
	Name   string `json:"name"`
	Owner  string `json:"owner,omitempty"`
	System bool   `json:"system,omitempty"`
}

type Object struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Rows      *int64 `json:"rows,omitempty"` // estimate
	Size      *int64 `json:"size,omitempty"` // bytes, data + indexes
	Engine    string `json:"engine,omitempty"`
	Comment   string `json:"comment,omitempty"`
	Collation string `json:"collation,omitempty"`
	Updated   string `json:"updated,omitempty"`
	Extra     string `json:"extra,omitempty"` // e.g. routine signature
	// Extension names the extension that owns the object (PostgreSQL);
	// dumps leave such objects to CREATE EXTENSION.
	Extension string `json:"extension,omitempty"`
	// OwnedBy is set for sequences created by an identity or serial column
	// ("identity" or "serial"), which the column recreates.
	OwnedBy string `json:"ownedBy,omitempty"`
}

type Column struct {
	Name            string    `json:"name"`
	Type            string    `json:"type"`     // full declared type, e.g. varchar(255)
	BaseType        string    `json:"baseType"` // lowercased base, e.g. varchar
	Kind            ValueKind `json:"kind"`
	Nullable        bool      `json:"nullable"`
	Default         *string   `json:"default,omitempty"`
	AutoIncrement   bool      `json:"autoIncrement,omitempty"`
	Generated       string    `json:"generated,omitempty"` // expression for computed columns
	GeneratedStored bool      `json:"generatedStored,omitempty"`
	PrimaryKey      bool      `json:"primaryKey,omitempty"`
	Comment         string    `json:"comment,omitempty"`
	Collation       string    `json:"collation,omitempty"`
	Enum            []string  `json:"enum,omitempty"`
	Unsigned        bool      `json:"unsigned,omitempty"`
	Length          *int64    `json:"length,omitempty"`
	Precision       *int64    `json:"precision,omitempty"`
	Scale           *int64    `json:"scale,omitempty"`
	SRID            int       `json:"srid,omitempty"`
	GeometryType    string    `json:"geometryType,omitempty"`
	OnUpdate        string    `json:"onUpdate,omitempty"`
}

type Index struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	Unique     bool     `json:"unique"`
	Primary    bool     `json:"primary"`
	Type       string   `json:"type,omitempty"` // btree, hash, gist, fulltext, spatial...
	Where      string   `json:"where,omitempty"`
	Lengths    []int    `json:"lengths,omitempty"`
	Desc       []bool   `json:"desc,omitempty"`
	Comment    string   `json:"comment,omitempty"`
	Definition string   `json:"definition,omitempty"`
}

type ForeignKey struct {
	Name       string    `json:"name"`
	Columns    []string  `json:"columns"`
	RefTable   ObjectRef `json:"refTable"`
	RefColumns []string  `json:"refColumns"`
	OnUpdate   string    `json:"onUpdate,omitempty"`
	OnDelete   string    `json:"onDelete,omitempty"`
	// For incoming references the source table is in Table.
	Table *ObjectRef `json:"table,omitempty"`
}

type Check struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

type Trigger struct {
	Name      string `json:"name"`
	Timing    string `json:"timing"` // BEFORE / AFTER / INSTEAD OF
	Event     string `json:"event"`  // INSERT / UPDATE / DELETE
	Statement string `json:"statement,omitempty"`
}

type Table struct {
	Ref         ObjectRef         `json:"ref"`
	Kind        string            `json:"kind"`
	Columns     []Column          `json:"columns"`
	Indexes     []Index           `json:"indexes"`
	ForeignKeys []ForeignKey      `json:"foreignKeys"`
	Referenced  []ForeignKey      `json:"referenced"` // incoming foreign keys
	Checks      []Check           `json:"checks"`
	Triggers    []Trigger         `json:"triggers"`
	PrimaryKey  []string          `json:"primaryKey"`
	RowKey      []string          `json:"rowKey"`               // columns that identify a row for editing
	RowKeyKind  string            `json:"rowKeyKind,omitempty"` // "primary", "unique", "rowid", "ctid", "_id"
	Comment     string            `json:"comment,omitempty"`
	Options     map[string]string `json:"options,omitempty"` // engine, collation, tablespace...
	RowEstimate *int64            `json:"rowEstimate,omitempty"`
	Size        *int64            `json:"size,omitempty"`
	DDL         string            `json:"ddl,omitempty"`
	Definition  string            `json:"definition,omitempty"` // view / routine body
	Editable    bool              `json:"editable"`
}

// TableDef is the desired state submitted by the structure editor.
type TableDef struct {
	Ref         ObjectRef         `json:"ref"`
	Columns     []ColumnDef       `json:"columns"`
	Indexes     []Index           `json:"indexes"`
	ForeignKeys []ForeignKey      `json:"foreignKeys"`
	Checks      []Check           `json:"checks"`
	PrimaryKey  []string          `json:"primaryKey"`
	Comment     string            `json:"comment"`
	Options     map[string]string `json:"options"`
}

type ColumnDef struct {
	Column
	// OriginalName links an edited column to the existing one (empty = new column).
	OriginalName string `json:"originalName,omitempty"`
	After        string `json:"after,omitempty"` // column ordering hint (MySQL)
}

type CatalogTable struct {
	Schema  string          `json:"schema,omitempty"`
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Columns []CatalogColumn `json:"columns"`
	FKs     []ForeignKey    `json:"fks,omitempty"`
}

type CatalogColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
	PK   bool   `json:"pk,omitempty"`
}

// ---- data --------------------------------------------------------------------

// ValueKind is a normalized type family the UI uses to render and edit cells.
type ValueKind string

const (
	KindInt       ValueKind = "int"
	KindFloat     ValueKind = "float"
	KindDecimal   ValueKind = "decimal"
	KindBool      ValueKind = "bool"
	KindString    ValueKind = "string"
	KindText      ValueKind = "text"
	KindBinary    ValueKind = "binary"
	KindDate      ValueKind = "date"
	KindTime      ValueKind = "time"
	KindDateTime  ValueKind = "datetime"
	KindTimestamp ValueKind = "timestamp" // with time zone
	KindInterval  ValueKind = "interval"
	KindJSON      ValueKind = "json"
	KindUUID      ValueKind = "uuid"
	KindGeometry  ValueKind = "geometry"
	KindArray     ValueKind = "array"
	KindObject    ValueKind = "object" // documents, structs
	KindEnum      ValueKind = "enum"
	KindOther     ValueKind = "other"
)

type ResultColumn struct {
	Name     string    `json:"name"`
	Type     string    `json:"type"` // engine type name
	Kind     ValueKind `json:"kind"`
	Nullable *bool     `json:"nullable,omitempty"`
	Table    string    `json:"table,omitempty"`
}

// Result is a materialized result set. Cell values are JSON-ready: see Encode.
type Result struct {
	Columns   []ResultColumn `json:"columns"`
	Rows      [][]any        `json:"rows"`
	Truncated bool           `json:"truncated"` // more rows existed than were returned
	// Clipped means the page stopped early because its values were large
	// (see BrowseBudget); Truncated is set too and fewer rows came back.
	Clipped      bool          `json:"clipped,omitempty"`
	RowsAffected *int64        `json:"rowsAffected,omitempty"`
	Duration     time.Duration `json:"-"`
	DurationMS   float64       `json:"durationMs"`
	SQL          string        `json:"sql,omitempty"` // the statement Rowsmith generated, shown to the user
}

type Count struct {
	Rows  int64 `json:"rows"`
	Exact bool  `json:"exact"`
}

type Filter struct {
	Column string `json:"column"`
	Op     string `json:"op"` // = != < <= > >= like notlike contains startswith endswith in notin null notnull regexp between
	Value  any    `json:"value,omitempty"`
	Values []any  `json:"values,omitempty"`
}

type Sort struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

type BrowseRequest struct {
	Ref     ObjectRef `json:"ref"`
	Columns []string  `json:"columns,omitempty"`
	Filters []Filter  `json:"filters,omitempty"`
	Where   string    `json:"where,omitempty"` // raw condition (SQL) or JSON filter (documents)
	Search  string    `json:"search,omitempty"`
	Sort    []Sort    `json:"sort,omitempty"`
	Offset  int64     `json:"offset"`
	Limit   int       `json:"limit"`
}

// RowEdit is one change from the data grid. Values use the cell encoding plus
// the markers {"$null":true}, {"$default":true} and {"$expr":"NOW()"}.
type RowEdit struct {
	Op     string         `json:"op"` // insert | update | delete
	Key    map[string]any `json:"key,omitempty"`
	Values map[string]any `json:"values,omitempty"`
}

type EditResult struct {
	Applied    int      `json:"applied"`
	Statements []string `json:"statements"`
	Inserted   [][]any  `json:"inserted,omitempty"` // row keys of inserted rows, when available
}

// ---- execution ---------------------------------------------------------------

type ExecOptions struct {
	MaxRows        int  // per result set; further rows are counted but not sent
	StopOnError    bool //
	ReadOnly       bool // reject statements that are not read-only
	DryRun         bool // engines with CostEstimate: estimate only
	MaxBytesBilled int64
	Params         []any
}

type StatementInfo struct {
	Index int           `json:"index"`
	SQL   string        `json:"sql"`
	Line  int           `json:"line"` // 1-based line of the statement within the script
	Kind  StatementKind `json:"kind"`
}

type ResultSummary struct {
	RowCount       int64   `json:"rowCount"`
	RowsAffected   *int64  `json:"rowsAffected,omitempty"`
	Truncated      bool    `json:"truncated"`
	Clipped        bool    `json:"clipped,omitempty"` // stopped at StreamBudget rather than MaxRows
	DurationMS     float64 `json:"durationMs"`
	BytesProcessed *int64  `json:"bytesProcessed,omitempty"`
	CacheHit       *bool   `json:"cacheHit,omitempty"`
}

// Sink receives streamed execution output. Engines call BeginStatement once
// per statement, then zero or more result sets (Columns → Rows... → EndResult),
// then EndStatement. Implementations serialize to the client.
type Sink interface {
	BeginStatement(StatementInfo) error
	Columns([]ResultColumn) error
	Rows([][]any) error
	EndResult(ResultSummary) error
	Notice(level, text string) error
	EndStatement(err error) error
}

// ---- explain -----------------------------------------------------------------

type PlanNode struct {
	Operation  string            `json:"operation"`
	Object     string            `json:"object,omitempty"`
	Detail     string            `json:"detail,omitempty"`
	Cost       *float64          `json:"cost,omitempty"`
	Rows       *float64          `json:"rows,omitempty"`
	ActualRows *float64          `json:"actualRows,omitempty"`
	TimeMS     *float64          `json:"timeMs,omitempty"`
	Loops      *float64          `json:"loops,omitempty"`
	Props      map[string]string `json:"props,omitempty"`
	Children   []*PlanNode       `json:"children,omitempty"`
}

type Plan struct {
	Root   *PlanNode       `json:"root"`
	Raw    string          `json:"raw"`
	Format string          `json:"format"` // json, text, xml
	Totals map[string]any  `json:"totals,omitempty"`
	Extra  json.RawMessage `json:"extra,omitempty"`
}

// ---- dump --------------------------------------------------------------------

type DumpRequest struct {
	Scope       Scope       `json:"scope"`
	Objects     []ObjectRef `json:"objects"` // empty = whole scope
	Structure   bool        `json:"structure"`
	Data        bool        `json:"data"`
	DropFirst   bool        `json:"dropFirst"`
	IfNotExists bool        `json:"ifNotExists"`
	BatchSize   int         `json:"batchSize"` // rows per INSERT
}

type DumpWriter interface {
	io.Writer
	Progress(object string, rows int64)
}
