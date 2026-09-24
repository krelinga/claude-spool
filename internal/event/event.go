// Package event defines the notification envelope Spool emits (design §3.7).
//
// Events are the only way Spool reaches out. They go to webhook receivers and
// to the /v1/events SSE stream, and the same envelope serves both.
package event

import (
	"encoding/json"
	"time"

	"github.com/krelinga/claude-spool-be/internal/store"
)

type Type string

// Job events carry a queue, so a receiver can be subscribed per queue.
const (
	JobSucceeded   Type = "job.succeeded"
	JobFailed      Type = "job.failed"
	JobNeedsInput  Type = "job.needs_input"
	JobInterrupted Type = "job.interrupted"
	JobCancelled   Type = "job.cancelled"
)

// Service events concern the shared machinery, so they always go to every
// receiver regardless of per-queue subscriptions (§3.7).
const (
	AuthRequired         Type = "auth.required"
	AuthExpiring         Type = "auth.expiring"
	AuthRestored         Type = "auth.restored"
	ExecutorBlockedUsage Type = "executor.blocked_usage"
	QueueAutoPaused      Type = "queue.auto_paused"
)

func (t Type) IsService() bool {
	switch t {
	case AuthRequired, AuthExpiring, AuthRestored, ExecutorBlockedUsage, QueueAutoPaused:
		return true
	}
	return false
}

func (t Type) Valid() bool {
	switch t {
	case JobSucceeded, JobFailed, JobNeedsInput, JobInterrupted, JobCancelled:
		return true
	}
	return t.IsService()
}

// ForStatus maps a terminal job status to the event it emits. It returns false
// for a non-terminal status, which emits nothing.
func ForStatus(s store.JobStatus) (Type, bool) {
	switch s {
	case store.StatusSucceeded:
		return JobSucceeded, true
	case store.StatusFailed:
		return JobFailed, true
	case store.StatusNeedsInput:
		return JobNeedsInput, true
	case store.StatusInterrupted:
		return JobInterrupted, true
	case store.StatusCancelled:
		return JobCancelled, true
	}
	return "", false
}

// Envelope is the payload delivered to a receiver.
type Envelope struct {
	Event Type `json:"event"`
	// EventID lets a receiver dedupe: a retried delivery reuses it.
	EventID string    `json:"event_id"`
	At      time.Time `json:"at"`

	Queue string `json:"queue,omitempty"`
	JobID string `json:"job_id,omitempty"`

	Job      *JobInfo      `json:"job,omitempty"`
	Auth     *AuthInfo     `json:"auth,omitempty"`
	Executor *ExecutorInfo `json:"executor,omitempty"`
	QueueRef *QueueInfo    `json:"queue_info,omitempty"`
}

// JobInfo is the notification-shaped view of a job: enough to show and act on,
// without the transcript or the rendered prompt.
type JobInfo struct {
	ID         string          `json:"id"`
	Queue      string          `json:"queue"`
	Status     store.JobStatus `json:"status"`
	Summary    string          `json:"summary,omitempty"`
	Question   string          `json:"question,omitempty"`
	Links      []string        `json:"links,omitempty"`
	ErrorKind  store.ErrorKind `json:"error_kind,omitempty"`
	ErrorMsg   string          `json:"error_message,omitempty"`
	Outcome    json.RawMessage `json:"outcome,omitempty"`
	Labels     []string        `json:"labels,omitempty"`
	ClientRef  string          `json:"client_ref,omitempty"`
	CostUSD    float64         `json:"cost_usd,omitempty"`
	NumTurns   int             `json:"num_turns,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

type AuthInfo struct {
	State      string     `json:"state"`
	Since      *time.Time `json:"since,omitempty"`
	QueuedJobs int        `json:"queued_jobs"`
	// LoginURL is where the phone starts the re-login flow, so the
	// notification is actionable rather than merely informative (§3.2).
	LoginURL string `json:"login_url,omitempty"`
}

type ExecutorInfo struct {
	State        string     `json:"state"`
	Reason       string     `json:"reason,omitempty"`
	BlockedUntil *time.Time `json:"blocked_until,omitempty"`
	QueuedJobs   int        `json:"queued_jobs"`
}

type QueueInfo struct {
	Name   string `json:"name"`
	Paused bool   `json:"paused"`
	Reason string `json:"reason,omitempty"`
	Depth  int    `json:"depth"`
}

// outcomeFields are the parts of the structured outcome worth lifting into the
// envelope so a receiver need not parse it.
type outcomeFields struct {
	Question string   `json:"question"`
	Links    []string `json:"links"`
}

// FromJob builds the envelope for a job's terminal state.
func FromJob(t Type, j *store.Job, now time.Time, id string) Envelope {
	info := &JobInfo{
		ID: j.ID, Queue: j.Queue, Status: j.Status, Summary: j.Summary,
		ErrorKind: j.ErrorKind, ErrorMsg: j.ErrorMessage, Outcome: j.Outcome,
		Labels: j.Labels, ClientRef: j.ClientRef, CostUSD: j.CostUSD,
		NumTurns: j.NumTurns, CreatedAt: j.CreatedAt, FinishedAt: j.FinishedAt,
	}
	if len(j.Outcome) > 0 {
		var o outcomeFields
		if err := json.Unmarshal(j.Outcome, &o); err == nil {
			info.Question, info.Links = o.Question, o.Links
		}
	}
	return Envelope{
		Event: t, EventID: id, At: now.UTC(),
		Queue: j.Queue, JobID: j.ID, Job: info,
	}
}
