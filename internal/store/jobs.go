package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

const jobColumns = `id, queue, queue_config_hash, status, priority, input, args,
	rendered_prompt, model, labels, client_ref, submitted_by, idempotency_key,
	parent_job_id, resume_session, run_attempts, created_at, started_at, finished_at,
	error_kind, error_message, outcome, summary, session_id, num_turns, cost_usd,
	duration_ms, tool_calls, permission_denials`

type scanner interface {
	Scan(dest ...any) error
}

func scanJob(sc scanner) (*Job, error) {
	var (
		j                                               Job
		input, args, rendered, model, labels, clientRef sql.NullString
		idem, parent, resume                            sql.NullString
		startedAt, finishedAt                           sql.NullInt64
		errKind, errMsg, outcome, summary, sessionID    sql.NullString
		numTurns, durationMS                            sql.NullInt64
		costUSD                                         sql.NullFloat64
		denials                                         sql.NullString
		createdAt                                       int64
	)
	err := sc.Scan(&j.ID, &j.Queue, &j.QueueConfigHash, &j.Status, &j.Priority,
		&input, &args, &rendered, &model, &labels, &clientRef, &j.SubmittedBy,
		&idem, &parent, &resume, &j.RunAttempts, &createdAt, &startedAt, &finishedAt,
		&errKind, &errMsg, &outcome, &summary, &sessionID, &numTurns, &costUSD,
		&durationMS, &j.ToolCalls, &denials)
	if err != nil {
		return nil, err
	}
	j.Input = input.String
	j.RenderedPrompt = rendered.String
	j.Model = model.String
	j.ClientRef = clientRef.String
	j.IdempotencyKey = idem.String
	j.ParentJobID = parent.String
	j.ResumeSession = resume.String
	j.CreatedAt = fromMillis(createdAt)
	j.StartedAt = millisPtr(startedAt)
	j.FinishedAt = millisPtr(finishedAt)
	j.ErrorKind = ErrorKind(errKind.String)
	j.ErrorMessage = errMsg.String
	j.Summary = summary.String
	j.SessionID = sessionID.String
	j.NumTurns = int(numTurns.Int64)
	j.CostUSD = costUSD.Float64
	j.DurationMS = durationMS.Int64
	if args.Valid && args.String != "" {
		if err := json.Unmarshal([]byte(args.String), &j.Args); err != nil {
			return nil, fmt.Errorf("job %s: decode args: %w", j.ID, err)
		}
	}
	if labels.Valid && labels.String != "" {
		if err := json.Unmarshal([]byte(labels.String), &j.Labels); err != nil {
			return nil, fmt.Errorf("job %s: decode labels: %w", j.ID, err)
		}
	}
	if denials.Valid && denials.String != "" {
		if err := json.Unmarshal([]byte(denials.String), &j.PermissionDenials); err != nil {
			return nil, fmt.Errorf("job %s: decode permission_denials: %w", j.ID, err)
		}
	}
	if outcome.Valid && outcome.String != "" {
		j.Outcome = json.RawMessage(outcome.String)
	}
	return &j, nil
}

// Insert stores a newly submitted job. It returns the existing job, and false,
// when the queue+idempotency key has already been used: a resubmitted share
// sheet must not create a second Notion page.
func (s *Store) Insert(ctx context.Context, j *Job) (*Job, bool, error) {
	argsJSON, err := marshalOrNil(j.Args)
	if err != nil {
		return nil, false, err
	}
	labelsJSON, err := marshalOrNil(j.Labels)
	if err != nil {
		return nil, false, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO jobs (id, queue, queue_config_hash, status, priority, input, args,
			model, labels, client_ref, submitted_by, idempotency_key, parent_job_id,
			resume_session, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Queue, j.QueueConfigHash, j.Status, j.Priority, nullString(j.Input), argsJSON,
		nullString(j.Model), labelsJSON, nullString(j.ClientRef), j.SubmittedBy,
		nullString(j.IdempotencyKey), nullString(j.ParentJobID), nullString(j.ResumeSession),
		toMillis(j.CreatedAt))
	if err != nil {
		if isUniqueViolation(err) && j.IdempotencyKey != "" {
			existing, getErr := s.jobByIdempotencyKey(ctx, j.Queue, j.IdempotencyKey)
			if getErr != nil {
				return nil, false, getErr
			}
			return existing, false, nil
		}
		return nil, false, fmt.Errorf("insert job: %w", err)
	}
	return j, true, nil
}

func isUniqueViolation(err error) bool {
	// modernc's driver reports constraint failures in the message; there is no
	// exported sentinel to match on.
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func marshalOrNil(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return nil, nil
		}
	case []string:
		if len(t) == 0 {
			return nil, nil
		}
	case nil:
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return string(b), nil
}

func (s *Store) jobByIdempotencyKey(ctx context.Context, queue, key string) (*Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE queue = ? AND idempotency_key = ?`, queue, key)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// ListFilter selects a page of jobs. An empty Queue or Status means "any".
type ListFilter struct {
	Queue  string
	Status JobStatus
	Since  *time.Time
	// Cursor is the ID to page from; results are newest-first, so the next page
	// is the jobs with an ID lexically below the cursor (ULIDs sort by time).
	Cursor string
	Limit  int
}

func (s *Store) List(ctx context.Context, f ListFilter) ([]*Job, error) {
	var where []string
	var args []any
	if f.Queue != "" {
		where = append(where, "queue = ?")
		args = append(args, f.Queue)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.Since != nil {
		where = append(where, "created_at >= ?")
		args = append(args, toMillis(*f.Since))
	}
	if f.Cursor != "" {
		where = append(where, "id < ?")
		args = append(args, f.Cursor)
	}
	q := `SELECT ` + jobColumns + ` FROM jobs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Claim atomically moves the head of a queue to running. It returns
// ErrNotFound when the queue has nothing runnable, which is not exceptional:
// the scheduler races nothing, but a job may have been cancelled meanwhile.
func (s *Store) Claim(ctx context.Context, queue string, now time.Time) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE jobs SET status = 'running', started_at = ?, run_attempts = run_attempts + 1
		WHERE id = (
			SELECT id FROM jobs WHERE queue = ? AND status = 'queued'
			ORDER BY priority DESC, id ASC LIMIT 1
		)
		RETURNING `+jobColumns, toMillis(now), queue)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// SetRenderedPrompt records the exact prompt a run used, so history stays
// readable after the template changes.
func (s *Store) SetRenderedPrompt(ctx context.Context, id, prompt string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET rendered_prompt = ? WHERE id = ?`, prompt, id)
	return err
}

// Finish writes a terminal result.
func (s *Store) Finish(ctx context.Context, id string, r Result, now time.Time) error {
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
	_, err = s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, finished_at = ?, error_kind = ?, error_message = ?,
			outcome = ?, summary = ?, session_id = ?, num_turns = ?, cost_usd = ?,
			duration_ms = ?, tool_calls = ?, permission_denials = ?
		WHERE id = ?`,
		r.Status, toMillis(now), nullString(string(r.ErrorKind)), nullString(r.ErrorMessage),
		outcome, nullString(r.Summary), nullString(r.SessionID), r.NumTurns, r.CostUSD,
		r.DurationMS, r.ToolCalls, denials, id)
	if err != nil {
		return fmt.Errorf("finish job %s: %w", id, err)
	}
	return nil
}

// Requeue returns a running job to the queue. It is used only for auth and
// usage failures with zero tool calls: anything that touched a tool may have
// had side effects and waits for a human instead (§3.5).
func (s *Store) Requeue(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'queued', started_at = NULL WHERE id = ? AND status = 'running'`, id)
	if err != nil {
		return fmt.Errorf("requeue %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Cancel marks a queued job cancelled. Cancelling a *running* job is an
// ergonomics-phase feature (§7 step 5); until then a running job is refused.
func (s *Store) Cancel(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'cancelled', finished_at = ? WHERE id = ? AND status = 'queued'`,
		toMillis(now), id)
	if err != nil {
		return fmt.Errorf("cancel %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecoverRunning marks jobs that were running when the process died as
// interrupted. At-most-once means they wait for a human rather than re-running
// with unknown side effects already applied (§3.5).
func (s *Store) RecoverRunning(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE status = 'running'`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'interrupted', finished_at = ?, error_kind = ?,
			error_message = 'spool restarted while this job was running'
		WHERE status = 'running'`, toMillis(now), string(ErrKindInterrupted))
	if err != nil {
		return nil, fmt.Errorf("recover running jobs: %w", err)
	}
	return ids, nil
}

// FailQueued fails every queued job in a queue. Used when a queue disappears
// from queues.yaml while it still holds work (§3.3).
func (s *Store) FailQueued(ctx context.Context, queue string, kind ErrorKind, msg string, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM jobs WHERE queue = ? AND status = 'queued'`, queue)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'failed', finished_at = ?, error_kind = ?, error_message = ?
		WHERE queue = ? AND status = 'queued'`, toMillis(now), string(kind), msg, queue)
	if err != nil {
		return nil, fmt.Errorf("fail queued jobs in %s: %w", queue, err)
	}
	return ids, nil
}

// Position reports how many jobs sit ahead of this one in its own queue.
func (s *Store) Position(ctx context.Context, j *Job) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE queue = ? AND status = 'queued'
		  AND (priority > ? OR (priority = ? AND id < ?))`,
		j.Queue, j.Priority, j.Priority, j.ID).Scan(&n)
	return n, err
}

// Retryable reports whether a job may be retried.
//
// A succeeded job is deliberately excluded: retrying it would repeat side
// effects that already landed, and at-most-once is the whole point (§3.5).
// Queued and running jobs have nothing to retry yet.
func Retryable(s JobStatus) bool {
	switch s {
	case StatusFailed, StatusNeedsInput, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// NewChild builds the follow-up job that a retry or a reply creates. Neither
// re-runs the original in place: history stays intact and the parent link
// records why the child exists (§3.5).
func NewChild(parent *Job, id string, now time.Time) *Job {
	return &Job{
		ID:              id,
		Queue:           parent.Queue,
		QueueConfigHash: parent.QueueConfigHash,
		Status:          StatusQueued,
		Priority:        parent.Priority,
		Input:           parent.Input,
		Args:            parent.Args,
		Model:           parent.Model,
		Labels:          parent.Labels,
		ClientRef:       parent.ClientRef,
		SubmittedBy:     parent.SubmittedBy,
		ParentJobID:     parent.ID,
		CreatedAt:       now.UTC(),
	}
}

// Children lists the jobs created from a parent, oldest first.
func (s *Store) Children(ctx context.Context, parentID string) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE parent_job_id = ? ORDER BY id`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", parentID, err)
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// CancelRunning marks a running job cancelled. The executor calls this after it
// has actually stopped the subprocess.
func (s *Store) CancelRunning(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'cancelled', finished_at = ? WHERE id = ? AND status = 'running'`,
		toMillis(now), id)
	if err != nil {
		return fmt.Errorf("cancel running %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PrunableTranscripts lists jobs whose transcript has outlived its queue's
// retention. Only the transcript is pruned: the job row is history and stays
// (§3.6 prunes "transcripts ... according to each queue's retention").
func (s *Store) PrunableTranscripts(ctx context.Context, queue string, before time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM jobs
		WHERE queue = ? AND finished_at IS NOT NULL AND finished_at < ?`,
		queue, toMillis(before))
	if err != nil {
		return nil, fmt.Errorf("list prunable transcripts for %s: %w", queue, err)
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
