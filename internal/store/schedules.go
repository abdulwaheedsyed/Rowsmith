package store

import "context"

// Schedule is a query that runs on a timetable and delivers its result.
type Schedule struct {
	ID           string `json:"id"`
	OwnerID      string `json:"ownerId"`
	OwnerName    string `json:"ownerName"`
	OwnerEmail   string `json:"ownerEmail"`
	ConnectionID string `json:"connectionId"`
	Connection   string `json:"connectionName"`
	Driver       string `json:"driver"`
	Environment  string `json:"environment"`
	Database     string `json:"database"`
	Schema       string `json:"schema"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Body         string `json:"body"`
	Cron         string `json:"cron"`
	Timezone     string `json:"timezone"`
	Config       string `json:"-"` // JSON, see package schedule
	Webhook      string `json:"-"` // sealed
	Enabled      bool   `json:"enabled"`
	PausedReason string `json:"pausedReason"`
	NextRunAt    int64  `json:"nextRunAt"`
	RunningSince int64  `json:"runningSince"`
	LastRunAt    int64  `json:"lastRunAt"`
	LastStatus   string `json:"lastStatus"`
	LastHash     string `json:"-"`
	LastAlert    bool   `json:"-"`
	Failures     int    `json:"failures"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

// WebhookAAD binds a schedule's sealed webhook URL to the schedule.
func (s *Schedule) WebhookAAD() string { return "schedule:" + s.ID + ":webhook" }

const scheduleCols = `s.id, s.owner_id, u.name, u.email, s.connection_id, c.name, c.driver, c.environment, s.database_name, s.schema_name, s.name, s.description,
	s.body, s.cron, s.timezone, s.config, s.webhook, s.enabled, s.paused_reason, s.next_run_at, s.running_since, s.last_run_at,
	s.last_status, s.last_hash, s.last_alert, s.failures, s.created_at, s.updated_at`

const scheduleFrom = ` FROM schedules s JOIN users u ON u.id = s.owner_id JOIN connections c ON c.id = s.connection_id`

func scanSchedule(row interface{ Scan(...any) error }) (*Schedule, error) {
	var x Schedule
	if err := row.Scan(&x.ID, &x.OwnerID, &x.OwnerName, &x.OwnerEmail, &x.ConnectionID, &x.Connection, &x.Driver, &x.Environment, &x.Database, &x.Schema, &x.Name,
		&x.Description, &x.Body, &x.Cron, &x.Timezone, &x.Config, &x.Webhook, &x.Enabled, &x.PausedReason, &x.NextRunAt,
		&x.RunningSince, &x.LastRunAt, &x.LastStatus, &x.LastHash, &x.LastAlert, &x.Failures, &x.CreatedAt, &x.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	return &x, nil
}

func (s *Store) CreateSchedule(ctx context.Context, x *Schedule) error {
	t := now()
	x.CreatedAt, x.UpdatedAt = t, t
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedules (id, owner_id, connection_id, database_name, schema_name, name, description, body,
		cron, timezone, config, webhook, enabled, paused_reason, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		x.ID, x.OwnerID, x.ConnectionID, x.Database, x.Schema, x.Name, x.Description, x.Body, x.Cron, x.Timezone, x.Config, x.Webhook,
		b2i(x.Enabled), x.PausedReason, x.NextRunAt, t, t)
	return err
}

// UpdateSchedule saves the editable fields and the next run time.
func (s *Store) UpdateSchedule(ctx context.Context, x *Schedule) error {
	x.UpdatedAt = now()
	res, err := s.db.ExecContext(ctx, `UPDATE schedules SET connection_id = ?, database_name = ?, schema_name = ?, name = ?, description = ?,
		body = ?, cron = ?, timezone = ?, config = ?, webhook = ?, enabled = ?, paused_reason = ?, next_run_at = ?, failures = ?,
		last_hash = ?, last_alert = ?, updated_at = ? WHERE id = ?`,
		x.ConnectionID, x.Database, x.Schema, x.Name, x.Description, x.Body, x.Cron, x.Timezone, x.Config, x.Webhook, b2i(x.Enabled),
		x.PausedReason, x.NextRunAt, x.Failures, x.LastHash, b2i(x.LastAlert), x.UpdatedAt, x.ID)
	return affected(res, err)
}

func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	return affected(res, err)
}

func (s *Store) ScheduleByID(ctx context.Context, id string) (*Schedule, error) {
	return scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleCols+scheduleFrom+` WHERE s.id = ?`, id))
}

// ListSchedules returns one person's schedules, or everyone's for "".
func (s *Store) ListSchedules(ctx context.Context, ownerID string) ([]*Schedule, error) {
	q := `SELECT ` + scheduleCols + scheduleFrom
	args := []any{}
	if ownerID != "" {
		q += ` WHERE s.owner_id = ?`
		args = append(args, ownerID)
	}
	return s.schedules(ctx, q+` ORDER BY s.enabled DESC, s.name COLLATE NOCASE`, args...)
}

// DueSchedules returns enabled schedules whose time has come and that are
// not already running.
func (s *Store) DueSchedules(ctx context.Context, at int64, limit int) ([]*Schedule, error) {
	return s.schedules(ctx, `SELECT `+scheduleCols+scheduleFrom+` WHERE s.enabled = 1 AND s.running_since = 0 AND s.next_run_at > 0
		AND s.next_run_at <= ? ORDER BY s.next_run_at LIMIT ?`, at, limit)
}

func (s *Store) schedules(ctx context.Context, q string, args ...any) ([]*Schedule, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Schedule
	for rows.Next() {
		x, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ClaimSchedule marks a schedule as running and sets its next run time. It
// fails (false) when another run holds it.
func (s *Store) ClaimSchedule(ctx context.Context, id string, at, next int64, advance bool) (bool, error) {
	q := `UPDATE schedules SET running_since = ? WHERE id = ? AND running_since = 0`
	args := []any{at, id}
	if advance {
		q = `UPDATE schedules SET running_since = ?, next_run_at = ? WHERE id = ? AND running_since = 0`
		args = []any{at, next, id}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ScheduleOutcome is what a finished run records on its schedule.
type ScheduleOutcome struct {
	At        int64
	Status    string
	Hash      string // kept when empty
	Alert     bool
	Failures  int
	Disable   bool
	Reason    string
	KeepState bool // a failed run leaves the hash and alert state alone
}

func (s *Store) FinishSchedule(ctx context.Context, id string, o ScheduleOutcome) error {
	q := `UPDATE schedules SET running_since = 0, last_run_at = ?, last_status = ?, failures = ?`
	args := []any{o.At, o.Status, o.Failures}
	if !o.KeepState {
		q += `, last_hash = ?, last_alert = ?`
		args = append(args, o.Hash, b2i(o.Alert))
	}
	if o.Disable {
		q += `, enabled = 0, paused_reason = ?`
		args = append(args, o.Reason)
	}
	_, err := s.db.ExecContext(ctx, q+` WHERE id = ?`, append(args, id)...)
	return err
}

// PauseSchedule turns a schedule off, with a reason shown to its owner.
func (s *Store) PauseSchedule(ctx context.Context, id, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET enabled = 0, paused_reason = ?, running_since = 0, updated_at = ? WHERE id = ?`, reason, now(), id)
	return err
}

// RecoverSchedules clears runs left unfinished by a restart.
func (s *Store) RecoverSchedules(ctx context.Context) (int64, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `UPDATE schedule_runs SET status = 'failed', finished_at = ?, error = 'Interrupted: Rowsmith restarted while this was running.'
		WHERE status = 'running'`, t)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	_, err = s.db.ExecContext(ctx, `UPDATE schedules SET running_since = 0 WHERE running_since <> 0`)
	return n, err
}

// ScheduleRun is one execution of a schedule.
type ScheduleRun struct {
	ID          string `json:"id"`
	ScheduleID  string `json:"scheduleId"`
	Trigger     string `json:"trigger"`
	TriggeredBy string `json:"triggeredBy"`
	StartedAt   int64  `json:"startedAt"`
	FinishedAt  int64  `json:"finishedAt"`
	Status      string `json:"status"`
	RowCount    int64  `json:"rowCount"`
	Truncated   bool   `json:"truncated"`
	Observed    string `json:"observed"`
	FileName    string `json:"fileName"`
	FileType    string `json:"fileType"`
	FileSize    int64  `json:"fileSize"`
	FileKey     string `json:"-"`
	FileExpires int64  `json:"fileExpires"`
	Delivery    string `json:"-"`
	Error       string `json:"error"`
}

// FileAAD binds a run's sealed file key to the run.
func (r *ScheduleRun) FileAAD() string { return "schedule-run:" + r.ID + ":file" }

const runCols = `id, schedule_id, trigger, triggered_by, started_at, finished_at, status, row_count, truncated, observed,
	file_name, file_type, file_size, file_key, file_expires, delivery, error`

func scanRun(row interface{ Scan(...any) error }) (*ScheduleRun, error) {
	var r ScheduleRun
	if err := row.Scan(&r.ID, &r.ScheduleID, &r.Trigger, &r.TriggeredBy, &r.StartedAt, &r.FinishedAt, &r.Status, &r.RowCount, &r.Truncated,
		&r.Observed, &r.FileName, &r.FileType, &r.FileSize, &r.FileKey, &r.FileExpires, &r.Delivery, &r.Error); err != nil {
		return nil, notFound(err)
	}
	return &r, nil
}

func (s *Store) CreateRun(ctx context.Context, r *ScheduleRun) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedule_runs (id, schedule_id, trigger, triggered_by, started_at, status) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.ScheduleID, r.Trigger, r.TriggeredBy, r.StartedAt, r.Status)
	return err
}

func (s *Store) FinishRun(ctx context.Context, r *ScheduleRun) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedule_runs SET finished_at = ?, status = ?, row_count = ?, truncated = ?, observed = ?,
		file_name = ?, file_type = ?, file_size = ?, file_key = ?, file_expires = ?, delivery = ?, error = ? WHERE id = ?`,
		r.FinishedAt, r.Status, r.RowCount, b2i(r.Truncated), r.Observed, r.FileName, r.FileType, r.FileSize, r.FileKey, r.FileExpires,
		r.Delivery, r.Error, r.ID)
	return err
}

func (s *Store) RunByID(ctx context.Context, id string) (*ScheduleRun, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM schedule_runs WHERE id = ?`, id))
}

func (s *Store) ListRuns(ctx context.Context, scheduleID string, limit int) ([]*ScheduleRun, error) {
	return s.runs(ctx, `SELECT `+runCols+` FROM schedule_runs WHERE schedule_id = ? ORDER BY started_at DESC LIMIT ?`, scheduleID, limit)
}

// RunsWithFiles lists runs whose files expire at or before the given time.
func (s *Store) RunsWithFiles(ctx context.Context, before int64) ([]*ScheduleRun, error) {
	return s.runs(ctx, `SELECT `+runCols+` FROM schedule_runs WHERE file_key <> '' AND file_expires <= ?`, before)
}

// AllRunKeys lists every run that still has a sealed file key.
func (s *Store) AllRunKeys(ctx context.Context) ([]*ScheduleRun, error) {
	return s.runs(ctx, `SELECT `+runCols+` FROM schedule_runs WHERE file_key <> ''`)
}

func (s *Store) SetRunFileKey(ctx context.Context, id, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedule_runs SET file_key = ? WHERE id = ?`, key, id)
	return err
}

// ForgetRunFile records that a run's file was deleted.
func (s *Store) ForgetRunFile(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedule_runs SET file_key = '', file_expires = 0 WHERE id = ?`, id)
	return err
}

// PruneRuns keeps the newest keep runs of a schedule and returns the IDs of
// deleted runs that still had files.
func (s *Store) PruneRuns(ctx context.Context, scheduleID string, keep int) ([]string, error) {
	old, err := s.runs(ctx, `SELECT `+runCols+` FROM schedule_runs WHERE schedule_id = ? ORDER BY started_at DESC LIMIT -1 OFFSET ?`, scheduleID, keep)
	if err != nil || len(old) == 0 {
		return nil, err
	}
	var files []string
	for _, r := range old {
		if r.FileKey != "" {
			files = append(files, r.ID)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM schedule_runs WHERE id = ?`, r.ID); err != nil {
			return files, err
		}
	}
	return files, nil
}

// RunIDsWithFiles returns the runs of a schedule that still have files, so
// they can be removed with the schedule.
func (s *Store) RunIDsWithFiles(ctx context.Context, scheduleID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM schedule_runs WHERE schedule_id = ? AND file_key <> ''`, scheduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) runs(ctx context.Context, q string, args ...any) ([]*ScheduleRun, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ScheduleRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AllSchedules is used when re-wrapping sealed values.
func (s *Store) AllSchedules(ctx context.Context) ([]*Schedule, error) {
	return s.ListSchedules(ctx, "")
}

// SetScheduleWebhook stores a re-wrapped webhook.
func (s *Store) SetScheduleWebhook(ctx context.Context, id, sealed string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET webhook = ? WHERE id = ?`, sealed, id)
	return err
}
