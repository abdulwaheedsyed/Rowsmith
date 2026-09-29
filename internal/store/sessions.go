package store

import "context"

type SessionStage string

const (
	StageMFA    SessionStage = "mfa"    // password verified, second factor pending
	StageEnroll SessionStage = "enroll" // must enroll MFA before doing anything else
	StageFull   SessionStage = "full"
)

type Session struct {
	IDHash     string
	UserID     string
	CSRF       string
	Stage      SessionStage
	CreatedAt  int64
	LastSeenAt int64
	ExpiresAt  int64
	IP         string
	UserAgent  string
}

func (s *Store) CreateSession(ctx context.Context, x *Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id_hash, user_id, csrf, stage, created_at, last_seen_at, expires_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, x.IDHash, x.UserID, x.CSRF, x.Stage, x.CreatedAt, x.LastSeenAt, x.ExpiresAt, x.IP, x.UserAgent)
	return err
}

func (s *Store) SessionByHash(ctx context.Context, h string) (*Session, error) {
	var x Session
	err := s.db.QueryRowContext(ctx, `SELECT id_hash, user_id, csrf, stage, created_at, last_seen_at, expires_at, ip, user_agent
		FROM sessions WHERE id_hash = ?`, h).Scan(&x.IDHash, &x.UserID, &x.CSRF, &x.Stage, &x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt, &x.IP, &x.UserAgent)
	if err != nil {
		return nil, notFound(err)
	}
	return &x, nil
}

func (s *Store) TouchSession(ctx context.Context, h string, t int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?`, t, h)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, h string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, h)
	return err
}

// DeleteUserSessions revokes every session of a user except keepHash (may be empty).
func (s *Store) DeleteUserSessions(ctx context.Context, userID, keepHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id_hash != ?`, userID, keepHash)
	return err
}

func (s *Store) PurgeExpiredSessions(ctx context.Context, idleCutoff int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ? OR last_seen_at < ?`, now(), idleCutoff)
	return err
}

type SessionInfo struct {
	IDHash     string `json:"id"`
	CreatedAt  int64  `json:"createdAt"`
	LastSeenAt int64  `json:"lastSeenAt"`
	IP         string `json:"ip"`
	UserAgent  string `json:"userAgent"`
}

func (s *Store) ListUserSessions(ctx context.Context, userID string) ([]SessionInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id_hash, created_at, last_seen_at, ip, user_agent FROM sessions
		WHERE user_id = ? AND stage = 'full' ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionInfo
	for rows.Next() {
		var x SessionInfo
		if err := rows.Scan(&x.IDHash, &x.CreatedAt, &x.LastSeenAt, &x.IP, &x.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
