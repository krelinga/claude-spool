package store

import (
	"encoding/json"
	"time"
)

type JobStatus string

const (
	StatusQueued      JobStatus = "queued"
	StatusRunning     JobStatus = "running"
	StatusSucceeded   JobStatus = "succeeded"
	StatusFailed      JobStatus = "failed"
	StatusNeedsInput  JobStatus = "needs_input"
	StatusCancelled   JobStatus = "cancelled"
	StatusInterrupted JobStatus = "interrupted"
)

// Terminal reports whether a status is final. Terminal jobs are never
// rescheduled: retry and reply create new child jobs instead (§3.5).
func (s JobStatus) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusNeedsInput, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

func (s JobStatus) Valid() bool {
	switch s {
	case StatusQueued, StatusRunning, StatusSucceeded, StatusFailed,
		StatusNeedsInput, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// ErrorKind classifies a run-level failure (design §3.4, layer 1).
type ErrorKind string

const (
	ErrKindAuth              ErrorKind = "auth"
	ErrKindUsageLimit        ErrorKind = "usage_limit"
	ErrKindCapabilityMissing ErrorKind = "capability_missing"
	// ErrKindCapabilityPending is a connector that had not finished connecting
	// when the run started. Observed in the spike: an MCP server reports
	// "pending" at init and contributes no tools, then reads "connected" on the
	// next run. Transient, so it must not auto-pause the queue.
	ErrKindCapabilityPending ErrorKind = "capability_pending"
	ErrKindMaxTurns          ErrorKind = "max_turns"
	ErrKindTimeout           ErrorKind = "timeout"
	ErrKindCLI               ErrorKind = "cli_error"
	ErrKindQueueRemoved      ErrorKind = "queue_removed"
	ErrKindInvalidOutcome    ErrorKind = "invalid_outcome"
	ErrKindInterrupted       ErrorKind = "interrupted"
)

// Blocking reports whether a failure blocks the whole executor rather than
// just failing one job. These are the shared-state failures: one login and one
// usage budget are shared by every queue (§3.3).
func (k ErrorKind) Blocking() bool {
	return k == ErrKindAuth || k == ErrKindUsageLimit
}

// Job is one unit of queued work.
type Job struct {
	ID                string          `json:"id"`
	Queue             string          `json:"queue"`
	QueueConfigHash   string          `json:"queue_config_hash"`
	Status            JobStatus       `json:"status"`
	Priority          int             `json:"priority"`
	Input             string          `json:"input"`
	Args              map[string]any  `json:"args,omitempty"`
	RenderedPrompt    string          `json:"rendered_prompt,omitempty"`
	Model             string          `json:"model,omitempty"`
	Labels            []string        `json:"labels,omitempty"`
	ClientRef         string          `json:"client_ref,omitempty"`
	SubmittedBy       string          `json:"submitted_by"`
	IdempotencyKey    string          `json:"idempotency_key,omitempty"`
	ParentJobID       string          `json:"parent_job_id,omitempty"`
	ResumeSession     string          `json:"resume_session,omitempty"`
	RunAttempts       int             `json:"run_attempts"`
	CreatedAt         time.Time       `json:"created_at"`
	StartedAt         *time.Time      `json:"started_at,omitempty"`
	FinishedAt        *time.Time      `json:"finished_at,omitempty"`
	ErrorKind         ErrorKind       `json:"error_kind,omitempty"`
	ErrorMessage      string          `json:"error_message,omitempty"`
	Outcome           json.RawMessage `json:"outcome,omitempty"`
	Summary           string          `json:"summary,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	NumTurns          int             `json:"num_turns,omitempty"`
	CostUSD           float64         `json:"cost_usd,omitempty"`
	DurationMS        int64           `json:"duration_ms,omitempty"`
	ToolCalls         int             `json:"tool_calls"`
	PermissionDenials []string        `json:"permission_denials,omitempty"`
}

// Result carries everything the executor learned from one run.
type Result struct {
	Status            JobStatus
	ErrorKind         ErrorKind
	ErrorMessage      string
	Outcome           json.RawMessage
	Summary           string
	SessionID         string
	NumTurns          int
	CostUSD           float64
	DurationMS        int64
	ToolCalls         int
	PermissionDenials []string
}
