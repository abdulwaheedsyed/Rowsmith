package driver

import "context"

// Access management: accounts, role memberships and privileges. Reads go to
// the server; changes come back as statements the UI previews before
// Rowsmith runs them.

// AccessManager is implemented by engines whose accounts and privileges
// Rowsmith can manage.
type AccessManager interface {
	AccessModel() AccessModel
	// Principals lists the accounts and roles access can be granted to.
	Principals(ctx context.Context) ([]Principal, error)
	// Access lists what a principal is granted directly: server-wide, in the
	// scope's database, in its schema and on the objects there.
	Access(ctx context.Context, principal string, scope Scope) ([]Grant, error)
	// ObjectAccess lists who is granted what on one object, including access
	// that comes from a broader level (shown with Via).
	ObjectAccess(ctx context.Context, ref ObjectRef) ([]Grant, error)
	// GrantSQL turns grant and revoke changes into statements.
	GrantSQL(changes []GrantChange) ([]string, error)
	// AccountSQL creates, changes or drops an account or a role membership.
	AccountSQL(op AccountOp) ([]string, error)
}

// Access levels, broadest first. An engine offers the ones it has.
const (
	LevelServer   = "server"   // server-wide privileges (MySQL *.*, Oracle system privileges)
	LevelDatabase = "database" // one database
	LevelSchema   = "schema"   // one schema
	LevelTables   = "tables"   // every table now in the schema
	LevelFuture   = "future"   // tables created in the schema later
	LevelTable    = "table"    // one table or view

	LevelSequences       = "sequences"        // every sequence now in the schema (PostgreSQL)
	LevelFutureSequences = "future_sequences" // sequences created in the schema later (PostgreSQL)
)

type AccessModel struct {
	Levels     []AccessLevel  `json:"levels"`
	Attributes []AccountAttr  `json:"attributes"` // account settings, edited in place
	Presets    []AccessPreset `json:"presets"`
	Roles      bool           `json:"roles"`    // accounts can be members of roles
	HostPart   bool           `json:"hostPart"` // accounts are user@host
	// What the part after @ means, for the form that creates an account.
	HostLabel   string   `json:"hostLabel,omitempty"`
	HostDefault string   `json:"hostDefault,omitempty"`
	HostHelp    string   `json:"hostHelp,omitempty"`
	CanCreate   []string `json:"canCreate"` // principal kinds that can be created: user, role
	Password    bool     `json:"password"`  // accounts can have a password set
	// Covers lists, for a level, the broader levels whose grants apply to it
	// too, e.g. a MySQL database-level SELECT covers every table in it.
	Covers map[string][]string `json:"covers,omitempty"`
	// DropOptions are choices offered when dropping an account, passed back
	// in AccountOp.Attrs.
	DropOptions []AccountAttr `json:"dropOptions,omitempty"`
	Note        string        `json:"note,omitempty"`
}

// AccessLevel is a place privileges are granted.
type AccessLevel struct {
	Level      string   `json:"level"`
	Label      string   `json:"label"`
	Privileges []string `json:"privileges"`
	Help       string   `json:"help,omitempty"`
}

// AccountAttr is an account setting, such as "can log in" or a connection limit.
type AccountAttr struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Type    string `json:"type"` // bool, int, date, text
	Help    string `json:"help,omitempty"`
	Default string `json:"default,omitempty"` // for drop options: "true" starts ticked
}

// AccessPreset is a named bundle of privileges for a database or schema.
type AccessPreset struct {
	ID          string        `json:"id"`
	Label       string        `json:"label"`
	Description string        `json:"description"`
	Grants      []PresetGrant `json:"grants"`
}

type PresetGrant struct {
	Level      string   `json:"level"`
	Privileges []string `json:"privileges"`
}

type Principal struct {
	Name       string         `json:"name"` // as Access and AccountSQL expect it (user@host on MySQL)
	Kind       string         `json:"kind"` // user, role
	Login      bool           `json:"login"`
	MemberOf   []string       `json:"memberOf"`
	Attributes map[string]any `json:"attributes,omitempty"`
	System     bool           `json:"system,omitempty"` // built in; shown but not offered for changes
	// ServerOnly principals hold server-wide privileges only, such as a SQL
	// Server server role: nothing in a database can be granted to them.
	ServerOnly bool `json:"serverOnly,omitempty"`
}

// Grant is one privilege a principal holds.
type Grant struct {
	Principal string `json:"principal"`
	Level     string `json:"level"`
	Database  string `json:"database,omitempty"`
	Schema    string `json:"schema,omitempty"`
	Object    string `json:"object,omitempty"`
	Privilege string `json:"privilege"`
	WithGrant bool   `json:"withGrant,omitempty"`
	Via       string `json:"via,omitempty"` // held through a broader level or a role; change it there
}

type GrantChange struct {
	Grant
	Revoke bool `json:"revoke,omitempty"`
	// Kind is the principal's, user or role, for engines that grant to
	// each differently (MongoDB).
	Kind string `json:"kind,omitempty"`
}

// AccountOp creates, changes or drops an account, or changes a role membership.
type AccountOp struct {
	Op       string            `json:"op"` // create, alter, password, drop, add_role, remove_role
	Name     string            `json:"name"`
	Kind     string            `json:"kind,omitempty"` // create: user or role
	Password string            `json:"password,omitempty"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Role     string            `json:"role,omitempty"`
}

// Redacted is what a password becomes in previews and the audit log.
const Redacted = "••••••••"
