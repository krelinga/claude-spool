package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Outbox states.
const (
	OutboxPending   = "pending"
	OutboxDelivered = "delivered"
	// OutboxDead means the retry window expired. Nothing retries it again; it
	// stays in the table as a record, and spool_webhook_dead_total counts it.
	OutboxDead = "dead"
)

// OutboxEntry is one pending delivery: one event to one receiver.
type OutboxEntry struct {
	ID       int64
	Event    string
	Queue    string
	JobID    string
	Payload  json.RawMessage
	URL      string
	Attempts int
	NextAt   time.Time
	State    string
}

// Delivery is an event addressed to a single receiver, ready to enqueue.
type Delivery struct {
	Event   string
	Queue   string
	JobID   string
	URL     string
	Payload json.RawMessage
}

// EnqueueDeliveries adds rows to the outbox.
func (s *Store) EnqueueDeliveries(ctx context.Context, ds []Delivery, now time.Time) error {
	if len(ds) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertDeliveries(ctx, tx, ds, now); err != nil {
		return err
	}
	return tx.Commit()
}

func insertDeliveries(ctx context.Context, tx *sql.Tx, ds []Delivery, now time.Time) error {
	for _, d := range ds {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO webhook_outbox (event, queue, job_id, payload, url, attempts, next_at, state)
			VALUES (?,?,?,?,?,0,?,?)`,
			d.Event, nullString(d.Queue), nullString(d.JobID), string(d.Payload), d.URL,
			toMillis(now), OutboxPending)
		if err != nil {
			return fmt.Errorf("enqueue delivery %s: %w", d.Event, err)
		}
	}
	return nil
}

// FinishWithDeliveries writes a job's terminal result and its notifications in
// one transaction.
//
// This is the transactional-outbox property the design calls for (§3.6): a job
// can never be recorded as finished without its notification also being
// queued, and a notification can never be queued for a result that was rolled
// back. The sender is a separate loop, so a slow receiver cannot hold up the
// executor.
func (s *Store) FinishWithDeliveries(ctx context.Context, id string, r Result, ds []Delivery, now time.Time) error {
	if !r.Status.Terminal() {
		return fmt.Errorf("finish job %s: %q is not terminal", id, r.Status)
	}
	denials, err := marshalOrNil(r.PermissionDenials)
	if err != nil {
		return err
	}
	var outcome any
	if len(r.Outcome) > 0 {
		outcome = string(r.Outcome)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs SET status = ?, finished_at = ?, error_kind = ?, error_message = ?,
			outcome = ?, summary = ?, session_id = ?, num_turns = ?, cost_usd = ?,
			duration_ms = ?, tool_calls = ?, permission_denials = ?
		WHERE id = ?`,
		r.Status, toMillis(now), nullString(string(r.ErrorKind)), nullString(r.ErrorMessage),
		outcome, nullString(r.Summary), nullString(r.SessionID), r.NumTurns, r.CostUSD,
		r.DurationMS, r.ToolCalls, denials, id); err != nil {
		return fmt.Errorf("finish job %s: %w", id, err)
	}
	if err := insertDeliveries(ctx, tx, ds, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimDueDeliveries returns pending deliveries whose next_at has passed.
func (s *Store) ClaimDueDeliveries(ctx context.Context, now time.Time, limit int) ([]OutboxEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, event, queue, job_id, payload, url, attempts, next_at, state
		FROM webhook_outbox
		WHERE state = ? AND next_at <= ?
		ORDER BY id LIMIT ?`, OutboxPending, toMillis(now), limit)
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	defer rows.Close()
	var out []OutboxEntry
	for rows.Next() {
		var (
			e            OutboxEntry
			queue, jobID sql.NullString
			payload      string
			nextAt       int64
		)
		if err := rows.Scan(&e.ID, &e.Event, &queue, &jobID, &payload, &e.URL,
			&e.Attempts, &nextAt, &e.State); err != nil {
			return nil, err
		}
		e.Queue, e.JobID = queue.String, jobID.String
		e.Payload = json.RawMessage(payload)
		e.NextAt = fromMillis(nextAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) MarkDelivered(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webhook_outbox SET state = ?, attempts = attempts + 1 WHERE id = ?`,
		OutboxDelivered, id)
	return err
}

// RescheduleDelivery records a failed attempt and when to try again.
func (s *Store) RescheduleDelivery(ctx context.Context, id int64, nextAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webhook_outbox SET attempts = attempts + 1, next_at = ? WHERE id = ?`,
		toMillis(nextAt), id)
	return err
}

// MarkDead gives up on a delivery whose retry window has expired.
func (s *Store) MarkDead(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webhook_outbox SET state = ?, attempts = attempts + 1 WHERE id = ?`,
		OutboxDead, id)
	return err
}

// OutboxCounts reports rows per state, for /metrics.
func (s *Store) OutboxCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM webhook_outbox GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("outbox counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

// NextDeliveryDue reports when the earliest pending delivery is due, so the
// sender can sleep instead of polling.
func (s *Store) NextDeliveryDue(ctx context.Context) (time.Time, bool, error) {
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(next_at) FROM webhook_outbox WHERE state = ?`, OutboxPending).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) || !at.Valid {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("next delivery due: %w", err)
	}
	return fromMillis(at.Int64), true, nil
}
