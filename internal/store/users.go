package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleMember, RoleViewer:
		return true
	}
	return false
}

// AtLeast reports whether r grants at least the privileges of other.
func (r Role) AtLeast(other Role) bool { return r.rank() >= other.rank() }

func (r Role) rank() int {
	switch r {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleMember:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

type User struct {
	ID                 string          `json:"id"`
	Email              string          `json:"email"`
	Name               string          `json:"name"`
	PasswordHash       string          `json:"-"`
	Role               Role            `json:"role"`
	MFASecret          string          `json:"-"`
	MFAEnabled         bool            `json:"mfaEnabled"`
	MFALastStep        int64           `json:"-"`
	FailedLogins       int             `json:"-"`
	LockedUntil        int64           `json:"-"`
	Disabled           bool            `json:"disabled"`
	MustChangePassword bool            `json:"mustChangePassword"`
	Prefs              json.RawMessage `json:"prefs"`
	CreatedAt          int64           `json:"createdAt"`
	UpdatedAt          int64           `json:"updatedAt"`
	LastLoginAt        int64           `json:"lastLoginAt"`
}

const userCols = `id, email, name, password_hash, role, COALESCE(mfa_secret, ''), mfa_enabled, mfa_last_step,
	failed_logins, locked_until, disabled, must_change_password, prefs, created_at, updated_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var prefs string
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.MFASecret, &u.MFAEnabled, &u.MFALastStep,
		&u.FailedLogins, &u.LockedUntil, &u.Disabled, &u.MustChangePassword, &prefs, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt)
	if err != nil {
		return nil, notFound(err)
	}
	u.Prefs = json.RawMessage(prefs)
	return &u, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, u *User) error {
	t := now()
	u.CreatedAt, u.UpdatedAt = t, t
	if len(u.Prefs) == 0 {
		u.Prefs = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO users (id, email, name, password_hash, role, must_change_password, prefs, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, u.ID, u.Email, u.Name, u.PasswordHash, u.Role, b2i(u.MustChangePassword), string(u.Prefs), t, t)
	return err
}

func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

type UserPatch struct {
	Name               *string
	Email              *string
	Role               *Role
	Disabled           *bool
	PasswordHash       *string
	MustChangePassword *bool
	Prefs              *json.RawMessage
}

func (s *Store) UpdateUser(ctx context.Context, id string, p UserPatch) error {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) { sets = append(sets, col+" = ?"); args = append(args, v) }
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.Email != nil {
		add("email", *p.Email)
	}
	if p.Role != nil {
		add("role", string(*p.Role))
	}
	if p.Disabled != nil {
		add("disabled", b2i(*p.Disabled))
	}
	if p.PasswordHash != nil {
		add("password_hash", *p.PasswordHash)
	}
	if p.MustChangePassword != nil {
		add("must_change_password", b2i(*p.MustChangePassword))
	}
	if p.Prefs != nil {
		add("prefs", string(*p.Prefs))
	}
	if len(sets) == 0 {
		return nil
	}
	add("updated_at", now())
	args = append(args, id)
	q := "UPDATE users SET " + join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, q, args...)
	return affected(res, err)
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return affected(res, err)
}

func (s *Store) CountRole(ctx context.Context, role Role) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ? AND disabled = 0`, role).Scan(&n)
	return n, err
}

// RecordLoginFailure increments the failure counter and locks the account
// for lockFor milliseconds once threshold consecutive failures are reached.
func (s *Store) RecordLoginFailure(ctx context.Context, id string, threshold int, lockFor int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET failed_logins = failed_logins + 1,
		locked_until = CASE WHEN failed_logins + 1 >= ? THEN ? ELSE locked_until END WHERE id = ?`,
		threshold, now()+lockFor, id)
	return err
}

func (s *Store) RecordLoginSuccess(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET failed_logins = 0, locked_until = 0, last_login_at = ? WHERE id = ?`, now(), id)
	return err
}

func (s *Store) SetMFA(ctx context.Context, id, sealedSecret string, enabled bool) error {
	var secret any = sealedSecret
	if sealedSecret == "" {
		secret = nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET mfa_secret = ?, mfa_enabled = ?, mfa_last_step = 0, updated_at = ? WHERE id = ?`,
		secret, b2i(enabled), now(), id)
	return err
}

// ConsumeTOTPStep atomically advances the last-used TOTP step, rejecting replays.
func (s *Store) ConsumeTOTPStep(ctx context.Context, id string, step int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET mfa_last_step = ? WHERE id = ? AND mfa_last_step < ?`, step, id, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID string, hashes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)`, userID, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UseRecoveryCode marks a matching unused code as used; returns false if none matched.
func (s *Store) UseRecoveryCode(ctx context.Context, userID, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at = 0`, now(), userID, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) RemainingRecoveryCodes(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at = 0`, userID).Scan(&n)
	return n, err
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
