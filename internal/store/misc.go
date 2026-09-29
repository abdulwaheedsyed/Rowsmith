package store

import (
	"context"
	"encoding/json"
)

// ---- Audit log -------------------------------------------------------------

type AuditEntry struct {
	ID       int64           `json:"id"`
	At       int64           `json:"at"`
	UserID   string          `json:"userId"`
	UserName string          `json:"userName"`
	IP       string          `json:"ip"`
	Action   string          `json:"action"`
	Target   string          `json:"target"`
	Detail   json.RawMessage `json:"detail"`
}

func (s *Store) Audit(ctx context.Context, userID, ip, action, target string, detail any) error {
	d := "{}"
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			d = string(b)
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (at, user_id, ip, action, target, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		now(), userID, ip, action, target, d)
	return err
}

type AuditFilter struct {
	UserID string
	Action string // prefix match, e.g. "auth."
	Before int64  // id cursor
	Limit  int
}

func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q := `SELECT a.id, a.at, a.user_id, COALESCE(u.name, ''), a.ip, a.action, a.target, a.detail
		FROM audit_log a LEFT JOIN users u ON u.id = a.user_id WHERE 1 = 1`
	args := []any{}
	if f.UserID != "" {
		q += ` AND a.user_id = ?`
		args = append(args, f.UserID)
	}
	if f.Action != "" {
		q += ` AND a.action LIKE ? ESCAPE '\'`
		args = append(args, likePrefix(f.Action))
	}
	if f.Before > 0 {
		q += ` AND a.id < ?`
		args = append(args, f.Before)
	}
	q += ` ORDER BY a.id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var d string
		if err := rows.Scan(&e.ID, &e.At, &e.UserID, &e.UserName, &e.IP, &e.Action, &e.Target, &d); err != nil {
			return nil, err
		}
		e.Detail = json.RawMessage(d)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Settings --------------------------------------------------------------

func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if notFound(err) == ErrNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string, secret bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value, secret) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, secret = excluded.secret`, key, value, b2i(secret))
	return err
}

func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

// SecretSettings returns all settings flagged secret (for key rotation).
func (s *Store) SecretSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE secret = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---- Query history ---------------------------------------------------------

type HistoryEntry struct {
	ID           int64  `json:"id"`
	UserID       string `json:"userId"`
	UserName     string `json:"userName,omitempty"`
	ConnectionID string `json:"connectionId"`
	Database     string `json:"database"`
	Body         string `json:"body"`
	StartedAt    int64  `json:"startedAt"`
	DurationMS   int64  `json:"durationMs"`
	RowCount     int64  `json:"rowCount"`
	Status       string `json:"status"`
	Error        string `json:"error"`
}

func (s *Store) AddHistory(ctx context.Context, h *HistoryEntry) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO query_history (user_id, connection_id, database_name, body, started_at, duration_ms, row_count, status, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, h.UserID, h.ConnectionID, h.Database, h.Body, h.StartedAt, h.DurationMS, h.RowCount, h.Status, h.Error)
	if err != nil {
		return err
	}
	h.ID, _ = res.LastInsertId()
	return nil
}

type HistoryFilter struct {
	UserID       string // empty = all users (team history)
	ConnectionID string
	Search       string
	Before       int64
	Limit        int
}

func (s *Store) ListHistory(ctx context.Context, f HistoryFilter) ([]HistoryEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q := `SELECT h.id, h.user_id, COALESCE(u.name, ''), h.connection_id, h.database_name, h.body, h.started_at, h.duration_ms, h.row_count, h.status, h.error
		FROM query_history h LEFT JOIN users u ON u.id = h.user_id WHERE 1 = 1`
	args := []any{}
	if f.UserID != "" {
		q += ` AND h.user_id = ?`
		args = append(args, f.UserID)
	}
	if f.ConnectionID != "" {
		q += ` AND h.connection_id = ?`
		args = append(args, f.ConnectionID)
	}
	if f.Search != "" {
		q += ` AND h.body LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(f.Search)+"%")
	}
	if f.Before > 0 {
		q += ` AND h.id < ?`
		args = append(args, f.Before)
	}
	q += ` ORDER BY h.id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryEntry
	for rows.Next() {
		var h HistoryEntry
		if err := rows.Scan(&h.ID, &h.UserID, &h.UserName, &h.ConnectionID, &h.Database, &h.Body, &h.StartedAt, &h.DurationMS, &h.RowCount, &h.Status, &h.Error); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- Saved queries ---------------------------------------------------------

type SavedQuery struct {
	ID           string   `json:"id"`
	OwnerID      string   `json:"ownerId"`
	OwnerName    string   `json:"ownerName"`
	ConnectionID string   `json:"connectionId"`
	Database     string   `json:"database"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Body         string   `json:"body"`
	Tags         []string `json:"tags"`
	Visibility   string   `json:"visibility"`
	CreatedAt    int64    `json:"createdAt"`
	UpdatedAt    int64    `json:"updatedAt"`
}

func (s *Store) SaveQuery(ctx context.Context, q *SavedQuery) error {
	tags, _ := json.Marshal(q.Tags)
	if q.Tags == nil {
		tags = []byte("[]")
	}
	t := now()
	q.UpdatedAt = t
	var conn any = q.ConnectionID
	if q.ConnectionID == "" {
		conn = nil
	}
	if q.CreatedAt == 0 {
		q.CreatedAt = t
		_, err := s.db.ExecContext(ctx, `INSERT INTO saved_queries (id, owner_id, connection_id, database_name, name, description, body, tags, visibility, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, q.ID, q.OwnerID, conn, q.Database, q.Name, q.Description, q.Body, string(tags), q.Visibility, t, t)
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE saved_queries SET connection_id = ?, database_name = ?, name = ?, description = ?, body = ?, tags = ?,
		visibility = ?, updated_at = ? WHERE id = ?`, conn, q.Database, q.Name, q.Description, q.Body, string(tags), q.Visibility, t, q.ID)
	return affected(res, err)
}

const savedCols = `q.id, q.owner_id, u.name, COALESCE(q.connection_id, ''), q.database_name, q.name, q.description, q.body, q.tags, q.visibility, q.created_at, q.updated_at`

func scanSaved(row interface{ Scan(...any) error }) (*SavedQuery, error) {
	var q SavedQuery
	var tags string
	if err := row.Scan(&q.ID, &q.OwnerID, &q.OwnerName, &q.ConnectionID, &q.Database, &q.Name, &q.Description, &q.Body, &tags, &q.Visibility, &q.CreatedAt, &q.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	_ = json.Unmarshal([]byte(tags), &q.Tags)
	return &q, nil
}

func (s *Store) SavedQueryByID(ctx context.Context, id string) (*SavedQuery, error) {
	return scanSaved(s.db.QueryRowContext(ctx, `SELECT `+savedCols+` FROM saved_queries q JOIN users u ON u.id = q.owner_id WHERE q.id = ?`, id))
}

// SavedQueriesFor lists the user's own queries plus team-visible ones.
func (s *Store) SavedQueriesFor(ctx context.Context, userID string) ([]*SavedQuery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+savedCols+` FROM saved_queries q JOIN users u ON u.id = q.owner_id
		WHERE q.owner_id = ? OR q.visibility = 'team' ORDER BY q.updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SavedQuery
	for rows.Next() {
		q, err := scanSaved(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSavedQuery(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM saved_queries WHERE id = ?`, id)
	return affected(res, err)
}

// ---- Object notes ----------------------------------------------------------

type Note struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connectionId"`
	ObjectPath   string `json:"objectPath"`
	AuthorID     string `json:"authorId"`
	AuthorName   string `json:"authorName"`
	Body         string `json:"body"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

func (s *Store) AddNote(ctx context.Context, n *Note) error {
	t := now()
	n.CreatedAt, n.UpdatedAt = t, t
	_, err := s.db.ExecContext(ctx, `INSERT INTO object_notes (id, connection_id, object_path, author_id, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, n.ID, n.ConnectionID, n.ObjectPath, n.AuthorID, n.Body, t, t)
	return err
}

func (s *Store) Notes(ctx context.Context, connID, path string) ([]Note, error) {
	q := `SELECT n.id, n.connection_id, n.object_path, n.author_id, u.name, n.body, n.created_at, n.updated_at
		FROM object_notes n JOIN users u ON u.id = n.author_id WHERE n.connection_id = ?`
	args := []any{connID}
	if path != "*" {
		q += ` AND n.object_path = ?`
		args = append(args, path)
	}
	q += ` ORDER BY n.created_at`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.ConnectionID, &n.ObjectPath, &n.AuthorID, &n.AuthorName, &n.Body, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) NoteByID(ctx context.Context, id string) (*Note, error) {
	var n Note
	err := s.db.QueryRowContext(ctx, `SELECT id, connection_id, object_path, author_id, body, created_at, updated_at FROM object_notes WHERE id = ?`, id).
		Scan(&n.ID, &n.ConnectionID, &n.ObjectPath, &n.AuthorID, &n.Body, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &n, nil
}

func (s *Store) DeleteNote(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM object_notes WHERE id = ?`, id)
	return affected(res, err)
}

func likePrefix(s string) string { return escapeLike(s) + "%" }

func escapeLike(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '%' || r == '_' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}
