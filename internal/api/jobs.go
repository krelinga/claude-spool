package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/krelinga/claude-spool-be/internal/store"
	"github.com/oklog/ulid/v2"
)

// maxInputBytes bounds a submission. Prompts come from a share sheet, not a
// file upload.
const maxInputBytes = 64 << 10

type submitRequest struct {
	Input     string         `json:"input"`
	Args      map[string]any `json:"args,omitempty"`
	Labels    []string       `json:"labels,omitempty"`
	Priority  int            `json:"priority,omitempty"`
	Model     string         `json:"model,omitempty"`
	ClientRef string         `json:"client_ref,omitempty"`
}

type submitResponse struct {
	ID       string          `json:"id"`
	Status   store.JobStatus `json:"status"`
	Queue    string          `json:"queue"`
	Position int             `json:"position"`
}

// submitJob accepts work and returns immediately. Nothing waits on Claude: the
// job is committed to SQLite before the response, so a restart cannot lose it
// (F1, F2).
func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	q, ok := s.queueFor(w, r)
	if !ok {
		return
	}

	var req submitRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInputBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not parse body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Input) == "" && len(req.Args) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "input or args is required")
		return
	}
	if err := q.ValidateArgs(req.Args); err != nil {
		writeError(w, http.StatusBadRequest, "bad_args", err.Error())
		return
	}

	now := s.now()
	job := &store.Job{
		ID:              ulid.Make().String(),
		Queue:           q.Name,
		QueueConfigHash: q.ConfigHash,
		Status:          store.StatusQueued,
		Priority:        req.Priority,
		Input:           req.Input,
		Args:            req.Args,
		Labels:          req.Labels,
		Model:           req.Model,
		ClientRef:       req.ClientRef,
		SubmittedBy:     tokenFrom(r).Name,
		IdempotencyKey:  strings.TrimSpace(r.Header.Get("Idempotency-Key")),
		CreatedAt:       now.UTC(),
	}

	stored, created, err := s.st.Insert(r.Context(), job)
	if err != nil {
		s.serverError(w, "could not enqueue job", err)
		return
	}
	pos, err := s.st.Position(r.Context(), stored)
	if err != nil {
		s.serverError(w, "could not read queue position", err)
		return
	}

	status := http.StatusAccepted
	if !created {
		// A replayed idempotency key returns the original job rather than
		// enqueuing a second one with the same side effects.
		status = http.StatusOK
	} else {
		s.exec.Wake()
		s.log.Info("job queued", "job", stored.ID, "queue", stored.Queue, "by", stored.SubmittedBy)
	}
	writeJSON(w, status, submitResponse{
		ID: stored.ID, Status: stored.Status, Queue: stored.Queue, Position: pos,
	})
}

func (s *Server) listQueueJobs(w http.ResponseWriter, r *http.Request) {
	q, ok := s.queueFor(w, r)
	if !ok {
		return
	}
	s.writeJobList(w, r, q.Name)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	s.writeJobList(w, r, r.URL.Query().Get("queue"))
}

func (s *Server) writeJobList(w http.ResponseWriter, r *http.Request, queue string) {
	tok := tokenFrom(r)
	if queue != "" && !tok.AllowsQueue(queue) {
		writeError(w, http.StatusNotFound, "no_such_queue", "no such queue")
		return
	}
	f := store.ListFilter{
		Queue:  queue,
		Status: store.JobStatus(r.URL.Query().Get("status")),
		Cursor: r.URL.Query().Get("cursor"),
		Limit:  intParam(r, "limit", 50),
	}
	if f.Status != "" && !f.Status.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown status filter")
		return
	}
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "since must be RFC3339")
			return
		}
		f.Since = &t
	}
	jobs, err := s.st.List(r.Context(), f)
	if err != nil {
		s.serverError(w, "could not list jobs", err)
		return
	}
	// A scoped token sees only its own queues in the cross-queue feed.
	visible := make([]*store.Job, 0, len(jobs))
	for _, j := range jobs {
		if tok.AllowsQueue(j.Queue) {
			visible = append(visible, j)
		}
	}
	body := map[string]any{"jobs": visible}
	if len(jobs) == f.Limit && len(jobs) > 0 {
		body["next_cursor"] = jobs[len(jobs)-1].ID
	}
	writeJSON(w, http.StatusOK, body)
}

// jobFor loads a job by ID, enforcing token scope. Job IDs are global so
// notifications can deep-link without knowing the queue (§3.7).
func (s *Server) jobFor(w http.ResponseWriter, r *http.Request) (*store.Job, bool) {
	id := r.PathValue("id")
	j, err := s.st.Get(r.Context(), id)
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, "no_such_job", "no such job")
		return nil, false
	}
	if err != nil {
		s.serverError(w, "could not read job", err)
		return nil, false
	}
	if tok := tokenFrom(r); tok == nil || !tok.AllowsQueue(j.Queue) {
		writeError(w, http.StatusNotFound, "no_such_job", "no such job")
		return nil, false
	}
	return j, true
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jobFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jobFor(w, r)
	if !ok {
		return
	}
	err := s.st.Cancel(r.Context(), j.ID, s.now())
	if errors.Is(err, store.ErrNotFound) {
		// The job moved on between the read and the update.
		writeError(w, http.StatusConflict, "not_cancellable",
			"only queued jobs can be cancelled; this one is "+string(j.Status))
		return
	}
	if err != nil {
		s.serverError(w, "could not cancel job", err)
		return
	}
	updated, err := s.st.Get(r.Context(), j.ID)
	if err != nil {
		s.serverError(w, "could not read job", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// getTranscript streams the raw stream-json the run produced.
func (s *Server) getTranscript(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jobFor(w, r)
	if !ok {
		return
	}
	path := filepath.Join(s.cfg.TranscriptDir(), j.ID+".jsonl")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no_transcript",
			"no transcript for this job (it may not have started, or may have been pruned)")
		return
	}
	if err != nil {
		s.serverError(w, "could not open transcript", err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("transcript stream interrupted", "job", j.ID, "error", err)
	}
}
