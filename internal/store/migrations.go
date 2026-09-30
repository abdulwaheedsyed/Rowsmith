package store

import "context"

// Migration is one run of the migration tool.
type Migration struct {
	ID          string `json:"id"`
	UserID      string `json:"userId"`
	UserName    string `json:"userName"`
	SourceID    string `json:"sourceId"`
	SourceLabel string `json:"sourceLabel"`
	TargetID    string `json:"targetId"`
	TargetLabel string `json:"targetLabel"`
	Status      string `json:"status"`
	Tables      int    `json:"tables"`
	Rows        int64  `json:"rows"`
	Report      string `json:"-"`
	Plan        string `json:"-"`
	StartedAt   int64  `json:"startedAt"`
	FinishedAt  int64  `json:"finishedAt"`
}

const migrationCols = `m.id, m.user_id, u.name, COALESCE(m.source_id, ''), m.source_label, COALESCE(m.target_id, ''), m.target_label,
	m.status, m.tables, m.rows_copied, m.report, m.plan, m.started_at, m.finished_at`

func scanMigration(row interface{ Scan(...any) error }) (*Migration, error) {
	var m Migration
	if err := row.Scan(&m.ID, &m.UserID, &m.UserName, &m.SourceID, &m.SourceLabel, &m.TargetID, &m.TargetLabel, &m.Status,
		&m.Tables, &m.Rows, &m.Report, &m.Plan, &m.StartedAt, &m.FinishedAt); err != nil {
		return nil, notFound(err)
	}
	return &m, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) CreateMigration(ctx context.Context, m *Migration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO migrations (id, user_id, source_id, source_label, target_id, target_label, status, tables, plan, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, m.ID, m.UserID, nullable(m.SourceID), m.SourceLabel, nullable(m.TargetID), m.TargetLabel,
		m.Status, m.Tables, m.Plan, m.StartedAt)
	return err
}

func (s *Store) FinishMigration(ctx context.Context, m *Migration) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET status = ?, rows_copied = ?, report = ?, finished_at = ? WHERE id = ?`,
		m.Status, m.Rows, m.Report, m.FinishedAt, m.ID)
	return err
}

func (s *Store) MigrationByID(ctx context.Context, id string) (*Migration, error) {
	return scanMigration(s.db.QueryRowContext(ctx, `SELECT `+migrationCols+` FROM migrations m JOIN users u ON u.id = m.user_id WHERE m.id = ?`, id))
}

// ListMigrations returns recent runs, one person's or everyone's ("").
func (s *Store) ListMigrations(ctx context.Context, userID string, limit int) ([]*Migration, error) {
	q := `SELECT ` + migrationCols + ` FROM migrations m JOIN users u ON u.id = m.user_id`
	args := []any{}
	if userID != "" {
		q += ` WHERE m.user_id = ?`
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY m.started_at DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Migration{}
	for rows.Next() {
		m, err := scanMigration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InterruptMigrations marks runs cut off by a restart.
func (s *Store) InterruptMigrations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE migrations SET status = 'interrupted', finished_at = ? WHERE status = 'running'`, now())
	return err
}
