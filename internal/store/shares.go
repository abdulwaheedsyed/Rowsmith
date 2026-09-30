package store

import (
	"context"
	"strings"
)

// QueryShare is a query frozen at the moment it was shared, with an optional
// snapshot of its result.
type QueryShare struct {
	ID           string   `json:"id"`
	AuthorID     string   `json:"authorId"`
	AuthorName   string   `json:"authorName"`
	ConnectionID string   `json:"connectionId"`
	Connection   string   `json:"connectionName"`
	Driver       string   `json:"driver"`
	Database     string   `json:"database"`
	Schema       string   `json:"schema"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Body         string   `json:"body"`
	Audience     string   `json:"audience"`
	Result       string   `json:"-"` // sealed
	ResultRows   int64    `json:"resultRows"`
	ResultAt     int64    `json:"resultAt"`
	ExpiresAt    int64    `json:"expiresAt"`
	LastActivity int64    `json:"lastActivity"`
	CreatedAt    int64    `json:"createdAt"`
	UpdatedAt    int64    `json:"updatedAt"`
	People       []string `json:"people"`
	Comments     int      `json:"comments"`
	Open         int      `json:"openThreads"`
}

// ResultAAD binds a share's sealed result to the share.
func (s *QueryShare) ResultAAD() string { return "share:" + s.ID + ":result" }

const shareCols = `q.id, q.author_id, u.name, COALESCE(q.connection_id, ''), q.connection, q.driver, q.database_name, q.schema_name,
	q.title, q.description, q.body, q.audience, q.result, q.result_rows, q.result_at, q.expires_at, q.last_activity, q.created_at, q.updated_at,
	(SELECT COUNT(*) FROM share_comments c WHERE c.share_id = q.id),
	(SELECT COUNT(*) FROM share_comments c WHERE c.share_id = q.id AND c.parent_id = '' AND c.resolved_at = 0 AND c.line > 0)`

func scanShare(row interface{ Scan(...any) error }) (*QueryShare, error) {
	var x QueryShare
	if err := row.Scan(&x.ID, &x.AuthorID, &x.AuthorName, &x.ConnectionID, &x.Connection, &x.Driver, &x.Database, &x.Schema,
		&x.Title, &x.Description, &x.Body, &x.Audience, &x.Result, &x.ResultRows, &x.ResultAt, &x.ExpiresAt, &x.LastActivity,
		&x.CreatedAt, &x.UpdatedAt, &x.Comments, &x.Open); err != nil {
		return nil, notFound(err)
	}
	x.People = []string{}
	return &x, nil
}

func (s *Store) CreateShare(ctx context.Context, x *QueryShare) error {
	t := now()
	x.CreatedAt, x.UpdatedAt, x.LastActivity = t, t, t
	var conn any = x.ConnectionID
	if x.ConnectionID == "" {
		conn = nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO query_shares (id, author_id, connection_id, connection, driver, database_name, schema_name,
		title, description, body, audience, result, result_rows, result_at, expires_at, last_activity, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		x.ID, x.AuthorID, conn, x.Connection, x.Driver, x.Database, x.Schema, x.Title, x.Description, x.Body, x.Audience,
		x.Result, x.ResultRows, x.ResultAt, x.ExpiresAt, t, t, t); err != nil {
		return err
	}
	for _, u := range x.People {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO query_share_people (share_id, user_id) VALUES (?, ?)`, x.ID, u); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateShare saves who may see a share, its wording and its expiry.
func (s *Store) UpdateShare(ctx context.Context, x *QueryShare) error {
	x.UpdatedAt = now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE query_shares SET title = ?, description = ?, audience = ?, expires_at = ?, updated_at = ? WHERE id = ?`,
		x.Title, x.Description, x.Audience, x.ExpiresAt, x.UpdatedAt, x.ID)
	if err := affected(res, err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM query_share_people WHERE share_id = ?`, x.ID); err != nil {
		return err
	}
	for _, u := range x.People {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO query_share_people (share_id, user_id) VALUES (?, ?)`, x.ID, u); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteShare(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM query_shares WHERE id = ?`, id)
	if err := affected(res, err); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM notifications WHERE share_id = ?`, id)
	return err
}

func (s *Store) ShareByID(ctx context.Context, id string) (*QueryShare, error) {
	x, err := scanShare(s.db.QueryRowContext(ctx, `SELECT `+shareCols+` FROM query_shares q JOIN users u ON u.id = q.author_id WHERE q.id = ?`, id))
	if err != nil {
		return nil, err
	}
	people, err := s.SharePeople(ctx, id)
	if err != nil {
		return nil, err
	}
	x.People = people
	return x, nil
}

func (s *Store) SharePeople(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM query_share_people WHERE share_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SharesFor lists shares a person can see: their own, the team's, and
// those shared with them by name ("all" adds everyone's, for admins).
func (s *Store) SharesFor(ctx context.Context, userID string, all bool, limit int) ([]*QueryShare, error) {
	q := `SELECT ` + shareCols + ` FROM query_shares q JOIN users u ON u.id = q.author_id`
	args := []any{}
	if !all {
		q += ` WHERE q.author_id = ? OR q.audience = 'team' OR EXISTS (SELECT 1 FROM query_share_people p WHERE p.share_id = q.id AND p.user_id = ?)`
		args = append(args, userID, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY q.last_activity DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*QueryShare
	for rows.Next() {
		x, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// AllShareResults is used when re-wrapping sealed values.
func (s *Store) AllShareResults(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, result FROM query_shares WHERE result <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, r string
		if err := rows.Scan(&id, &r); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

func (s *Store) SetShareResult(ctx context.Context, id, sealed string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE query_shares SET result = ? WHERE id = ?`, sealed, id)
	return err
}

func (s *Store) touchShare(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE query_shares SET last_activity = ? WHERE id = ?`, now(), id)
}

// Comment is one message in a share's discussion.
type Comment struct {
	ID         string `json:"id"`
	ShareID    string `json:"shareId"`
	AuthorID   string `json:"authorId"`
	AuthorName string `json:"authorName"`
	ParentID   string `json:"parentId"`
	Line       int    `json:"line"`
	Body       string `json:"body"`
	ResolvedAt int64  `json:"resolvedAt"`
	ResolvedBy string `json:"resolvedBy"`
	CreatedAt  int64  `json:"createdAt"`
	EditedAt   int64  `json:"editedAt"`
}

const commentCols = `c.id, c.share_id, c.author_id, u.name, c.parent_id, c.line, c.body, c.resolved_at, c.resolved_by, c.created_at, c.edited_at`

func scanComment(row interface{ Scan(...any) error }) (*Comment, error) {
	var c Comment
	if err := row.Scan(&c.ID, &c.ShareID, &c.AuthorID, &c.AuthorName, &c.ParentID, &c.Line, &c.Body, &c.ResolvedAt, &c.ResolvedBy,
		&c.CreatedAt, &c.EditedAt); err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

func (s *Store) AddComment(ctx context.Context, c *Comment) error {
	c.CreatedAt = now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO share_comments (id, share_id, author_id, parent_id, line, body, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ShareID, c.AuthorID, c.ParentID, c.Line, c.Body, c.CreatedAt)
	if err == nil {
		s.touchShare(ctx, c.ShareID)
	}
	return err
}

func (s *Store) CommentByID(ctx context.Context, id string) (*Comment, error) {
	return scanComment(s.db.QueryRowContext(ctx, `SELECT `+commentCols+` FROM share_comments c JOIN users u ON u.id = c.author_id WHERE c.id = ?`, id))
}

func (s *Store) Comments(ctx context.Context, shareID string) ([]*Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+commentCols+` FROM share_comments c JOIN users u ON u.id = c.author_id
		WHERE c.share_id = ? ORDER BY c.created_at`, shareID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Comment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) EditComment(ctx context.Context, id, body string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE share_comments SET body = ?, edited_at = ? WHERE id = ?`, body, now(), id)
	return affected(res, err)
}

// DeleteComment removes a comment, and its replies when it starts a thread.
func (s *Store) DeleteComment(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM share_comments WHERE parent_id = ?`, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM share_comments WHERE id = ?`, id)
	if err := affected(res, err); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM notifications WHERE comment_id = ?`, id)
	return err
}

func (s *Store) ResolveThread(ctx context.Context, id, by string, resolved bool) error {
	at, who := int64(0), ""
	if resolved {
		at, who = now(), by
	}
	res, err := s.db.ExecContext(ctx, `UPDATE share_comments SET resolved_at = ?, resolved_by = ? WHERE id = ? AND parent_id = ''`, at, who, id)
	return affected(res, err)
}

// Notification tells someone about activity on a share.
type Notification struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	ActorID   string `json:"actorId"`
	ActorName string `json:"actorName"`
	ShareID   string `json:"shareId"`
	Title     string `json:"title"`
	CommentID string `json:"commentId"`
	Excerpt   string `json:"excerpt"`
	CreatedAt int64  `json:"createdAt"`
	ReadAt    int64  `json:"readAt"`
}

func (s *Store) Notify(ctx context.Context, n *Notification, userID string) error {
	n.CreatedAt = now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO notifications (id, user_id, kind, actor_id, share_id, comment_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		n.ID, userID, n.Kind, n.ActorID, n.ShareID, n.CommentID, n.CreatedAt)
	return err
}

// Notifications returns someone's newest notifications and how many are unread.
func (s *Store) Notifications(ctx context.Context, userID string, limit int) ([]*Notification, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, n.kind, n.actor_id, COALESCE(a.name, ''), n.share_id, COALESCE(q.title, ''), n.comment_id,
		COALESCE(c.body, ''), n.created_at, n.read_at
		FROM notifications n
		LEFT JOIN users a ON a.id = n.actor_id
		LEFT JOIN query_shares q ON q.id = n.share_id
		LEFT JOIN share_comments c ON c.id = n.comment_id
		WHERE n.user_id = ? ORDER BY n.created_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.ActorID, &n.ActorName, &n.ShareID, &n.Title, &n.CommentID, &n.Excerpt, &n.CreatedAt, &n.ReadAt); err != nil {
			return nil, 0, err
		}
		if len(n.Excerpt) > 240 {
			n.Excerpt = n.Excerpt[:240] + "…"
		}
		out = append(out, &n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var unread int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at = 0`, userID).Scan(&unread)
	return out, unread, err
}

// MarkRead marks the given notifications (or all, for none given) read.
func (s *Store) MarkRead(ctx context.Context, userID string, ids []string) error {
	if len(ids) == 0 {
		_, err := s.db.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at = 0`, now(), userID)
		return err
	}
	args := []any{now(), userID}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE user_id = ? AND id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	return err
}

// MarkShareRead marks everything about one share read, e.g. on opening it.
func (s *Store) MarkShareRead(ctx context.Context, userID, shareID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE user_id = ? AND share_id = ? AND read_at = 0`, now(), userID, shareID)
	return err
}

// PruneNotifications keeps notifications for 90 days.
func (s *Store) PruneNotifications(ctx context.Context, before int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notifications WHERE created_at < ?`, before)
	return err
}
