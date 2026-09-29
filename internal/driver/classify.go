package driver

// StatementKind classifies a statement for read-only enforcement and
// production safety prompts.
type StatementKind string

const (
	StmtRead    StatementKind = "read"    // SELECT, SHOW, EXPLAIN, DESCRIBE, WITH ... SELECT
	StmtWrite   StatementKind = "write"   // INSERT, UPDATE, DELETE, MERGE, REPLACE, COPY FROM, CALL
	StmtDDL     StatementKind = "ddl"     // CREATE, ALTER, DROP, TRUNCATE, RENAME, COMMENT
	StmtDCL     StatementKind = "dcl"     // GRANT, REVOKE, users and roles
	StmtTCL     StatementKind = "tcl"     // BEGIN, COMMIT, ROLLBACK, SAVEPOINT
	StmtSession StatementKind = "session" // SET, USE, harmless session state
	StmtUnknown StatementKind = "unknown"
)

// Safe reports whether the statement can run in a read-only session.
func (k StatementKind) Safe() bool {
	return k == StmtRead || k == StmtTCL || k == StmtSession
}

// Danger describes why a statement deserves an explicit confirmation.
type Danger struct {
	Level  string `json:"level"` // "", "caution", "destructive"
	Reason string `json:"reason,omitempty"`
}
