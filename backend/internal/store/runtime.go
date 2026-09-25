package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ExecutorState is the single shared gate in front of every queue. Auth and
// usage limits belong to the one login and the one subscription, so they block
// everything at once — which is exactly the part that should be shared (§3.3).
type ExecutorState string

const (
	ExecReady        ExecutorState = "ready"
	ExecBlockedAuth  ExecutorState = "blocked_auth"
	ExecBlockedUsage ExecutorState = "blocked_usage"
	ExecPaused       ExecutorState = "paused"
)

type Executor struct {
	State        ExecutorState `json:"state"`
	Reason       string        `json:"reason,omitempty"`
	BlockedUntil *time.Time    `json:"blocked_until,omitempty"`
}

// Runnable reports whether the executor may start a job now. A usage block
// expires by itself once the reset time passes.
func (e Executor) Runnable(now time.Time) bool {
	if e.State == ExecReady {
		return true
	}
	if e.State == ExecBlockedUsage && e.BlockedUntil != nil && now.After(*e.BlockedUntil) {
		return true
	}
	return false
}

func (s *Store) ExecutorState(ctx context.Context) (Executor, error) {
	var (
		e       Executor
		reason  sql.NullString
		blocked sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT state, blocked_until, reason FROM executor_state WHERE id = 1`).
		Scan(&e.State, &blocked, &reason)
	if err != nil {
		return e, fmt.Errorf("read executor state: %w", err)
	}
	e.Reason = reason.String
	e.BlockedUntil = millisPtr(blocked)
	return e, nil
}

func (s *Store) SetExecutorState(ctx context.Context, state ExecutorState, reason string, until *time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE executor_state SET state = ?, reason = ?, blocked_until = ? WHERE id = 1`,
		string(state), nullString(reason), nullMillis(until))
	if err != nil {
		return fmt.Errorf("set executor state: %w", err)
	}
	return nil
}

// QueueRuntime is the mutable per-queue state that config does not own.
type QueueRuntime struct {
	Queue        string  `json:"queue"`
	Paused       bool    `json:"paused"`
	PausedReason string  `json:"paused_reason,omitempty"`
	RRCredit     float64 `json:"-"`
}

func (s *Store) QueueRuntime(ctx context.Context, queue string) (QueueRuntime, error) {
	var (
		r      QueueRuntime
		paused int
		reason sql.NullString
	)
	r.Queue = queue
	err := s.db.QueryRowContext(ctx,
		`SELECT paused, paused_reason, rr_credit FROM queue_runtime WHERE queue = ?`, queue).
		Scan(&paused, &reason, &r.RRCredit)
	if errors.Is(err, sql.ErrNoRows) {
		// A queue with no runtime row has never been touched: unpaused, no credit.
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("read queue runtime %s: %w", queue, err)
	}
	r.Paused = paused != 0
	r.PausedReason = reason.String
	return r, nil
}

func (s *Store) AllQueueRuntime(ctx context.Context) (map[string]QueueRuntime, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT queue, paused, paused_reason, rr_credit FROM queue_runtime`)
	if err != nil {
		return nil, fmt.Errorf("read queue runtime: %w", err)
	}
	defer rows.Close()
	out := map[string]QueueRuntime{}
	for rows.Next() {
		var (
			r      QueueRuntime
			paused int
			reason sql.NullString
		)
		if err := rows.Scan(&r.Queue, &paused, &reason, &r.RRCredit); err != nil {
			return nil, err
		}
		r.Paused = paused != 0
		r.PausedReason = reason.String
		out[r.Queue] = r
	}
	return out, rows.Err()
}

func (s *Store) SetQueuePaused(ctx context.Context, queue string, paused bool, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO queue_runtime (queue, paused, paused_reason) VALUES (?,?,?)
		ON CONFLICT(queue) DO UPDATE SET paused = excluded.paused, paused_reason = excluded.paused_reason`,
		queue, boolInt(paused), nullString(reason))
	if err != nil {
		return fmt.Errorf("set queue %s paused: %w", queue, err)
	}
	return nil
}

// SetRRCredits persists round-robin bookkeeping so fairness survives a restart.
func (s *Store) SetRRCredits(ctx context.Context, credits map[string]float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for queue, credit := range credits {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO queue_runtime (queue, rr_credit) VALUES (?,?)
			ON CONFLICT(queue) DO UPDATE SET rr_credit = excluded.rr_credit`, queue, credit); err != nil {
			return fmt.Errorf("save rr credit for %s: %w", queue, err)
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Depths returns the number of queued jobs per queue.
func (s *Store) Depths(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT queue, COUNT(*) FROM jobs WHERE status = 'queued' GROUP BY queue`)
	if err != nil {
		return nil, fmt.Errorf("queue depths: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var q string
		var n int
		if err := rows.Scan(&q, &n); err != nil {
			return nil, err
		}
		out[q] = n
	}
	return out, rows.Err()
}

// QueueStats is what GET /v1/queues/{q} reports.
type QueueStats struct {
	Depth           int        `json:"depth"`
	Running         string     `json:"running_job,omitempty"`
	LastSuccessAt   *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt   *time.Time `json:"last_failure_at,omitempty"`
	SuccessRate7d   *float64   `json:"success_rate_7d,omitempty"`
	CompletedJobs7d int        `json:"completed_jobs_7d"`
}

func (s *Store) Stats(ctx context.Context, queue string, now time.Time) (QueueStats, error) {
	var st QueueStats
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE queue = ? AND status = 'queued'`, queue).Scan(&st.Depth)
	if err != nil {
		return st, fmt.Errorf("stats depth: %w", err)
	}

	var running sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM jobs WHERE queue = ? AND status = 'running' LIMIT 1`, queue).Scan(&running); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("stats running: %w", err)
	}
	st.Running = running.String

	for _, q := range []struct {
		status string
		dst    **time.Time
	}{
		{"succeeded", &st.LastSuccessAt},
		{"failed", &st.LastFailureAt},
	} {
		var at sql.NullInt64
		err := s.db.QueryRowContext(ctx,
			`SELECT MAX(finished_at) FROM jobs WHERE queue = ? AND status = ?`, queue, q.status).Scan(&at)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return st, fmt.Errorf("stats %s: %w", q.status, err)
		}
		*q.dst = millisPtr(at)
	}

	// Success rate counts only runs that reached a verdict. Cancelled and
	// interrupted jobs say nothing about whether the queue works.
	since := toMillis(now.Add(-7 * 24 * time.Hour))
	var succeeded, decided int
	err = s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM jobs
		WHERE queue = ? AND finished_at >= ? AND status IN ('succeeded','failed','needs_input')`,
		queue, since).Scan(&succeeded, &decided)
	if err != nil {
		return st, fmt.Errorf("stats success rate: %w", err)
	}
	st.CompletedJobs7d = decided
	if decided > 0 {
		rate := float64(succeeded) / float64(decided)
		st.SuccessRate7d = &rate
	}
	return st, nil
}

// JobCount is one row of the jobs-by-outcome breakdown behind
// spool_jobs_total.
type JobCount struct {
	Queue     string
	Status    JobStatus
	ErrorKind ErrorKind
	Count     int
}

func (s *Store) JobCounts(ctx context.Context) ([]JobCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT queue, status, COALESCE(error_kind, ''), COUNT(*)
		FROM jobs GROUP BY queue, status, error_kind
		ORDER BY queue, status, error_kind`)
	if err != nil {
		return nil, fmt.Errorf("job counts: %w", err)
	}
	defer rows.Close()
	var out []JobCount
	for rows.Next() {
		var c JobCount
		if err := rows.Scan(&c.Queue, &c.Status, &c.ErrorKind, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
