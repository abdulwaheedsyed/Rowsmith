package store

import (
	"context"
	"encoding/json"
	"strings"
)

// Access levels a user can hold on a connection, from weakest to strongest.
type Access string

const (
	AccessNone   Access = ""
	AccessRead   Access = "read"   // read-only sessions
	AccessWrite  Access = "write"  // full query access
	AccessManage Access = "manage" // may edit settings, secrets and sharing
)

func (a Access) rank() int {
	switch a {
	case AccessRead:
		return 1
	case AccessWrite:
		return 2
	case AccessManage:
		return 3
	}
	return 0
}

func (a Access) AtLeast(b Access) bool { return a.rank() >= b.rank() }

func MaxAccess(a, b Access) Access {
	if a.rank() >= b.rank() {
		return a
	}
	return b
}

func (a Access) Valid() bool { return a == AccessRead || a == AccessWrite || a == AccessManage }

type Connection struct {
	ID          string          `json:"id"`
	OwnerID     string          `json:"ownerId"`
	Name        string          `json:"name"`
	Driver      string          `json:"driver"`
	Color       string          `json:"color"`
	Environment string          `json:"environment"`
	Folder      string          `json:"folder"`
	Params      json.RawMessage `json:"params"`
	SSH         json.RawMessage `json:"ssh"`
	Secrets     string          `json:"-"`
	ReadOnly    bool            `json:"readOnly"`
	TeamAccess  Access          `json:"teamAccess"`
	Notes       string          `json:"notes"`
	CreatedAt   int64           `json:"createdAt"`
	UpdatedAt   int64           `json:"updatedAt"`
	LastUsedAt  int64           `json:"lastUsedAt"`
}

// SecretsAAD binds a connection's sealed secrets to its identity.
func (c *Connection) SecretsAAD() string { return "connection:" + c.ID + ":secrets" }

const connCols = `id, owner_id, name, driver, color, environment, folder, params, ssh, secrets, read_only, team_access, notes, created_at, updated_at, last_used_at`

func scanConn(row interface{ Scan(...any) error }) (*Connection, error) {
	var c Connection
	var params, ssh string
	err := row.Scan(&c.ID, &c.OwnerID, &c.Name, &c.Driver, &c.Color, &c.Environment, &c.Folder, &params, &ssh, &c.Secrets,
		&c.ReadOnly, &c.TeamAccess, &c.Notes, &c.CreatedAt, &c.UpdatedAt, &c.LastUsedAt)
	if err != nil {
		return nil, notFound(err)
	}
	c.Params, c.SSH = json.RawMessage(params), json.RawMessage(ssh)
	return &c, nil
}

func (s *Store) CreateConnection(ctx context.Context, c *Connection) error {
	t := now()
	c.CreatedAt, c.UpdatedAt = t, t
	_, err := s.db.ExecContext(ctx, `INSERT INTO connections (`+connCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.OwnerID, c.Name, c.Driver, c.Color, c.Environment, c.Folder, rawOr(c.Params), rawOr(c.SSH), c.Secrets,
		b2i(c.ReadOnly), c.TeamAccess, c.Notes, t, t, 0)
	return err
}

func (s *Store) UpdateConnection(ctx context.Context, c *Connection) error {
	c.UpdatedAt = now()
	res, err := s.db.ExecContext(ctx, `UPDATE connections SET name = ?, driver = ?, color = ?, environment = ?, folder = ?, params = ?, ssh = ?,
		secrets = ?, read_only = ?, team_access = ?, notes = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.Driver, c.Color, c.Environment, c.Folder, rawOr(c.Params), rawOr(c.SSH), c.Secrets, b2i(c.ReadOnly),
		c.TeamAccess, c.Notes, c.UpdatedAt, c.ID)
	return affected(res, err)
}

func (s *Store) TouchConnection(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE connections SET last_used_at = ? WHERE id = ?`, now(), id)
}

func (s *Store) DeleteConnection(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE id = ?`, id)
	return affected(res, err)
}

func (s *Store) ConnectionByID(ctx context.Context, id string) (*Connection, error) {
	return scanConn(s.db.QueryRowContext(ctx, `SELECT `+connCols+` FROM connections WHERE id = ?`, id))
}

// VisibleConnection pairs a connection with the caller's effective access.
type VisibleConnection struct {
	*Connection
	Access    Access `json:"access"`
	OwnerName string `json:"ownerName"`
}

// ConnectionsFor lists connections the user owns, has been shared, or that are team-shared.
func (s *Store) ConnectionsFor(ctx context.Context, userID string) ([]VisibleConnection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixCols("c.", connCols)+`, COALESCE(sh.access, ''), u.name
		FROM connections c
		JOIN users u ON u.id = c.owner_id
		LEFT JOIN connection_shares sh ON sh.connection_id = c.id AND sh.user_id = ?
		WHERE c.owner_id = ? OR sh.user_id IS NOT NULL OR c.team_access != ''
		ORDER BY c.folder COLLATE NOCASE, c.name COLLATE NOCASE`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VisibleConnection
	for rows.Next() {
		var c Connection
		var params, ssh, share, owner string
		if err := rows.Scan(&c.ID, &c.OwnerID, &c.Name, &c.Driver, &c.Color, &c.Environment, &c.Folder, &params, &ssh, &c.Secrets,
			&c.ReadOnly, &c.TeamAccess, &c.Notes, &c.CreatedAt, &c.UpdatedAt, &c.LastUsedAt, &share, &owner); err != nil {
			return nil, err
		}
		c.Params, c.SSH = json.RawMessage(params), json.RawMessage(ssh)
		acc := MaxAccess(Access(share), c.TeamAccess)
		if c.OwnerID == userID {
			acc = AccessManage
		}
		out = append(out, VisibleConnection{Connection: &c, Access: acc, OwnerName: owner})
	}
	return out, rows.Err()
}

// AccessFor returns the user's effective access to a connection (AccessNone if none).
func (s *Store) AccessFor(ctx context.Context, c *Connection, userID string) (Access, error) {
	if c.OwnerID == userID {
		return AccessManage, nil
	}
	var share string
	err := s.db.QueryRowContext(ctx, `SELECT access FROM connection_shares WHERE connection_id = ? AND user_id = ?`, c.ID, userID).Scan(&share)
	if err != nil && notFound(err) != ErrNotFound {
		return AccessNone, err
	}
	return MaxAccess(Access(share), c.TeamAccess), nil
}

type Share struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Access Access `json:"access"`
}

func (s *Store) Shares(ctx context.Context, connID string) ([]Share, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sh.user_id, u.name, u.email, sh.access FROM connection_shares sh
		JOIN users u ON u.id = sh.user_id WHERE sh.connection_id = ? ORDER BY u.name`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		var x Share
		if err := rows.Scan(&x.UserID, &x.Name, &x.Email, &x.Access); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) ReplaceShares(ctx context.Context, connID string, shares []Share) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM connection_shares WHERE connection_id = ?`, connID); err != nil {
		return err
	}
	for _, sh := range shares {
		if _, err := tx.ExecContext(ctx, `INSERT INTO connection_shares (connection_id, user_id, access) VALUES (?, ?, ?)`, connID, sh.UserID, sh.Access); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TransferConnections moves ownership, used when a user is removed.
func (s *Store) TransferConnections(ctx context.Context, fromUser, toUser string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connections SET owner_id = ?, updated_at = ? WHERE owner_id = ?`, toUser, now(), fromUser)
	return err
}

func (s *Store) AllConnections(ctx context.Context) ([]*Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connCols+` FROM connections`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Connection
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- SSH known hosts -------------------------------------------------------

type KnownHost struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"-"`
	AddedBy     string `json:"addedBy"`
	AddedAt     int64  `json:"addedAt"`
}

func (s *Store) KnownHostKeys(ctx context.Context, host string, port int) ([]KnownHost, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host, port, key_type, fingerprint, public_key, added_by, added_at
		FROM ssh_known_hosts WHERE host = ? AND port = ?`, strings.ToLower(host), port)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KnownHost
	for rows.Next() {
		var k KnownHost
		if err := rows.Scan(&k.Host, &k.Port, &k.KeyType, &k.Fingerprint, &k.PublicKey, &k.AddedBy, &k.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) TrustHostKey(ctx context.Context, k KnownHost) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO ssh_known_hosts (host, port, key_type, fingerprint, public_key, added_by, added_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (host, port, key_type) DO UPDATE SET fingerprint = excluded.fingerprint,
		public_key = excluded.public_key, added_by = excluded.added_by, added_at = excluded.added_at`,
		strings.ToLower(k.Host), k.Port, k.KeyType, k.Fingerprint, k.PublicKey, k.AddedBy, now())
	return err
}

func (s *Store) ListKnownHosts(ctx context.Context) ([]KnownHost, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host, port, key_type, fingerprint, public_key, added_by, added_at FROM ssh_known_hosts ORDER BY host, port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KnownHost
	for rows.Next() {
		var k KnownHost
		if err := rows.Scan(&k.Host, &k.Port, &k.KeyType, &k.Fingerprint, &k.PublicKey, &k.AddedBy, &k.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) ForgetHostKey(ctx context.Context, host string, port int, keyType string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ssh_known_hosts WHERE host = ? AND port = ? AND key_type = ?`, strings.ToLower(host), port, keyType)
	return affected(res, err)
}

func rawOr(r json.RawMessage) string {
	if len(r) == 0 {
		return "{}"
	}
	return string(r)
}

func prefixCols(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
