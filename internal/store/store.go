// Package store persists ansibled's run records in SQLite (DESIGN.md "Storage").
//
// It uses the pure-Go modernc.org/sqlite driver in WAL mode with a busy
// timeout, so the web UI can read while the queue writes. Times are stored as
// unix nanoseconds in UTC, with 0 meaning "not set" (the zero time.Time).
// The run summary is stored as JSON, NULL when there is none.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/timothywashburn/ansibled/internal/model"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store is a handle on the ansibled database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// migrations[i] upgrades the schema from user_version i to i+1.
var migrations = []string{
	`CREATE TABLE runs (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		kind         TEXT    NOT NULL,
		"key"        TEXT    NOT NULL,
		mode         TEXT    NOT NULL,
		sha          TEXT    NOT NULL,
		ref          TEXT    NOT NULL DEFAULT '',
		mr_iid       INTEGER NOT NULL DEFAULT 0,
		"trigger"    TEXT    NOT NULL DEFAULT '',
		triggered_by TEXT    NOT NULL DEFAULT '',
		state        TEXT    NOT NULL,
		description  TEXT    NOT NULL DEFAULT '',
		summary_json TEXT,
		queued_at    INTEGER NOT NULL DEFAULT 0,
		started_at   INTEGER NOT NULL DEFAULT 0,
		finished_at  INTEGER NOT NULL DEFAULT 0,
		logs_expired INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX runs_key_sha ON runs ("key", sha);
	CREATE INDEX runs_state ON runs (state);
	CREATE INDEX runs_finished_at ON runs (finished_at);`,
}

// Open opens (creating if needed) the database at path and brings its schema
// up to date.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this ansibled (%d)", v, len(migrations))
	}
	if v == len(migrations) {
		return nil
	}
	for ; v < len(migrations); v++ {
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
	}
	// PRAGMA does not accept bound parameters; v is an int we control.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const columns = `id, kind, "key", mode, sha, ref, mr_iid, "trigger", triggered_by, state,
	description, summary_json, queued_at, started_at, finished_at, logs_expired`

// CreateRun inserts r and sets r.ID. It fills in defaults: QueuedAt = now if
// zero, Key from Kind and MRIID if empty, Mode = model.ModeCheck if empty.
func (s *Store) CreateRun(ctx context.Context, r *model.Run) error {
	if r.QueuedAt.IsZero() {
		r.QueuedAt = time.Now()
	}
	if r.Key == "" {
		r.Key = model.Key(r.Kind, r.MRIID)
	}
	if r.Mode == "" {
		r.Mode = model.ModeCheck
	}
	sum, err := encodeSummary(r.Summary)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs (kind, "key", mode, sha, ref, mr_iid, "trigger",
		triggered_by, state, description, summary_json, queued_at, started_at, finished_at, logs_expired)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Kind, r.Key, r.Mode, r.SHA, r.Ref, r.MRIID, r.Trigger, r.TriggeredBy, r.State, r.Description,
		sum, toNanos(r.QueuedAt), toNanos(r.StartedAt), toNanos(r.FinishedAt), r.LogsExpired)
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	r.ID = id
	return nil
}

// UpdateRun overwrites every column of the row with ID r.ID.
func (s *Store) UpdateRun(ctx context.Context, r *model.Run) error {
	sum, err := encodeSummary(r.Summary)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET kind = ?, "key" = ?, mode = ?, sha = ?, ref = ?,
		mr_iid = ?, "trigger" = ?, triggered_by = ?, state = ?, description = ?, summary_json = ?,
		queued_at = ?, started_at = ?, finished_at = ?, logs_expired = ? WHERE id = ?`,
		r.Kind, r.Key, r.Mode, r.SHA, r.Ref, r.MRIID, r.Trigger, r.TriggeredBy, r.State, r.Description,
		sum, toNanos(r.QueuedAt), toNanos(r.StartedAt), toNanos(r.FinishedAt), r.LogsExpired, r.ID)
	if err != nil {
		return fmt.Errorf("update run %d: %w", r.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("update run %d: no such run", r.ID)
	}
	return nil
}

// GetRun returns the run with the given ID, or (nil, nil) if there is none.
func (s *Store) GetRun(ctx context.Context, id int64) (*model.Run, error) {
	return s.one(ctx, `SELECT `+columns+` FROM runs WHERE id = ?`, id)
}

// ListRuns returns up to limit runs, newest ID first, skipping offset runs.
// A limit <= 0 means no limit.
func (s *Store) ListRuns(ctx context.Context, limit, offset int) ([]*model.Run, error) {
	if limit <= 0 {
		limit = -1
	}
	if offset < 0 {
		offset = 0
	}
	return s.many(ctx, `SELECT `+columns+` FROM runs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
}

// FilterRuns is ListRuns restricted to the runs matching f.
func (s *Store) FilterRuns(ctx context.Context, f model.RunFilter, limit, offset int) ([]*model.Run, error) {
	if limit <= 0 {
		limit = -1
	}
	if offset < 0 {
		offset = 0
	}
	q := `SELECT ` + columns + ` FROM runs WHERE 1 = 1`
	var args []any
	if len(f.Kinds) > 0 {
		in, a := inList(f.Kinds)
		q += ` AND kind IN (` + in + `)`
		args = append(args, a...)
	}
	if len(f.States) > 0 {
		in, a := inList(f.States)
		q += ` AND state IN (` + in + `)`
		args = append(args, a...)
	}
	if f.MRIID > 0 {
		q += ` AND mr_iid = ?`
		args = append(args, f.MRIID)
	}
	q += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	return s.many(ctx, q, append(args, limit, offset)...)
}

// ActiveByKey returns the queued and running runs with the given key, ascending ID.
func (s *Store) ActiveByKey(ctx context.Context, key string) ([]*model.Run, error) {
	return s.many(ctx, `SELECT `+columns+` FROM runs WHERE "key" = ? AND state IN (?, ?) ORDER BY id`,
		key, model.StateQueued, model.StateRunning)
}

// Exists reports whether a run for key and sha exists in any state but canceled.
func (s *Store) Exists(ctx context.Context, key, sha string) (bool, error) {
	return s.ExistsExcept(ctx, key, sha, 0)
}

// ExistsExcept is Exists ignoring the run with ID exceptID (used by the queue
// to ignore a running run that is already being cancelled).
func (s *Store) ExistsExcept(ctx context.Context, key, sha string, exceptID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE "key" = ? AND sha = ? AND state != ? AND id != ? LIMIT 1`,
		key, sha, model.StateCanceled, exceptID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("exists %s@%s: %w", key, sha, err)
	}
	return true, nil
}

// NextQueued returns the queued run with the lowest ID, or (nil, nil) if none.
func (s *Store) NextQueued(ctx context.Context) (*model.Run, error) {
	return s.one(ctx, `SELECT `+columns+` FROM runs WHERE state = ? ORDER BY id LIMIT 1`, model.StateQueued)
}

// ListByState returns every run in one of states, ascending ID. No states, no runs.
func (s *Store) ListByState(ctx context.Context, states ...model.State) ([]*model.Run, error) {
	if len(states) == 0 {
		return nil, nil
	}
	in, args := inList(states)
	return s.many(ctx, `SELECT `+columns+` FROM runs WHERE state IN (`+in+`) ORDER BY id`, args...)
}

// CountByState counts the runs in one of states. No states counts nothing.
func (s *Store) CountByState(ctx context.Context, states ...model.State) (int, error) {
	if len(states) == 0 {
		return 0, nil
	}
	in, args := inList(states)
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE state IN (`+in+`)`, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count runs: %w", err)
	}
	return n, nil
}

// LatestFinished returns the run of one of kinds (any kind if none are given)
// that finished last with state success, failed or error, or (nil, nil).
func (s *Store) LatestFinished(ctx context.Context, kinds ...model.Kind) (*model.Run, error) {
	q := `SELECT ` + columns + ` FROM runs WHERE state IN (?, ?, ?) AND finished_at != 0`
	args := []any{model.StateSuccess, model.StateFailed, model.StateError}
	if len(kinds) > 0 {
		in, kargs := inList(kinds)
		q += ` AND kind IN (` + in + `)`
		args = append(args, kargs...)
	}
	q += ` ORDER BY finished_at DESC, id DESC LIMIT 1`
	return s.one(ctx, q, args...)
}

// LogsOlderThan returns the IDs, ascending, of runs that finished before t and
// whose logs have not been marked expired yet.
func (s *Store) LogsOlderThan(ctx context.Context, t time.Time) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs
		WHERE finished_at != 0 AND finished_at < ? AND logs_expired = 0 ORDER BY id`, toNanos(t))
	if err != nil {
		return nil, fmt.Errorf("logs older than: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkLogsExpired records that the log files of run id have been deleted.
func (s *Store) MarkLogsExpired(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET logs_expired = 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("mark logs expired %d: %w", id, err)
	}
	return nil
}

func (s *Store) one(ctx context.Context, q string, args ...any) (*model.Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) many(ctx context.Context, q string, args ...any) ([]*model.Run, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanRun(sc scanner) (*model.Run, error) {
	var (
		r                   model.Run
		sum                 sql.NullString
		queued, started, fn int64
	)
	err := sc.Scan(&r.ID, &r.Kind, &r.Key, &r.Mode, &r.SHA, &r.Ref, &r.MRIID, &r.Trigger, &r.TriggeredBy,
		&r.State, &r.Description, &sum, &queued, &started, &fn, &r.LogsExpired)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan run: %w", err)
	}
	r.QueuedAt, r.StartedAt, r.FinishedAt = fromNanos(queued), fromNanos(started), fromNanos(fn)
	if sum.Valid && sum.String != "" {
		r.Summary = new(model.Summary)
		if err := json.Unmarshal([]byte(sum.String), r.Summary); err != nil {
			return nil, fmt.Errorf("run %d: decode summary: %w", r.ID, err)
		}
	}
	return &r, nil
}

func encodeSummary(s *model.Summary) (sql.NullString, error) {
	if s == nil {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode summary: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// toNanos maps the zero time to 0, everything else to unix nanoseconds.
func toNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func inList[T ~string](vs []T) (string, []any) {
	args := make([]any, len(vs))
	for i, v := range vs {
		args[i] = string(v)
	}
	return strings.TrimSuffix(strings.Repeat("?, ", len(vs)), ", "), args
}
