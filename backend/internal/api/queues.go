package api

import (
	"net/http"

	"github.com/krelinga/claude-spool-be/backend/internal/config"
	"github.com/krelinga/claude-spool-be/backend/internal/store"
)

// queueView is the safe subset of a queue definition (§3.7). Prompts, system
// prompts and tool allowlists are deliberately withheld: clients need to
// render a picker, not to see the policy.
type queueView struct {
	Name          string                         `json:"name"`
	Description   string                         `json:"description,omitempty"`
	Args          map[string]config.ArgSpec      `json:"args,omitempty"`
	OutcomeFields map[string]config.OutcomeField `json:"outcome_fields,omitempty"`
	Model         string                         `json:"model,omitempty"`
	MaxTurns      int                            `json:"max_turns"`
	Timeout       string                         `json:"timeout"`
	Weight        float64                        `json:"weight"`
	ConfigHash    string                         `json:"config_hash"`

	Paused       bool   `json:"paused"`
	PausedReason string `json:"paused_reason,omitempty"`
	store.QueueStats
}

func viewQueue(q *config.Queue, rt store.QueueRuntime, stats store.QueueStats) queueView {
	return queueView{
		Name: q.Name, Description: q.Description, Args: q.Args,
		OutcomeFields: q.OutcomeExtension, Model: q.Model, MaxTurns: q.MaxTurns,
		Timeout: q.Timeout.String(), Weight: q.Weight, ConfigHash: q.ConfigHash,
		Paused: rt.Paused, PausedReason: rt.PausedReason, QueueStats: stats,
	}
}

func (s *Server) listQueues(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r)
	qs := s.queues()
	runtimes, err := s.st.AllQueueRuntime(r.Context())
	if err != nil {
		s.serverError(w, "could not read queue runtime", err)
		return
	}
	now := s.now()
	out := []queueView{}
	for _, name := range qs.Names() {
		if !tok.AllowsQueue(name) {
			continue
		}
		q, _ := qs.Get(name)
		stats, err := s.st.Stats(r.Context(), name, now)
		if err != nil {
			s.serverError(w, "could not read queue stats", err)
			return
		}
		out = append(out, viewQueue(q, runtimes[name], stats))
	}
	writeJSON(w, http.StatusOK, map[string]any{"queues": out})
}

func (s *Server) getQueue(w http.ResponseWriter, r *http.Request) {
	q, ok := s.queueFor(w, r)
	if !ok {
		return
	}
	rt, err := s.st.QueueRuntime(r.Context(), q.Name)
	if err != nil {
		s.serverError(w, "could not read queue runtime", err)
		return
	}
	stats, err := s.st.Stats(r.Context(), q.Name, s.now())
	if err != nil {
		s.serverError(w, "could not read queue stats", err)
		return
	}
	writeJSON(w, http.StatusOK, viewQueue(q, rt, stats))
}

func (s *Server) pauseQueue(w http.ResponseWriter, r *http.Request)  { s.setPaused(w, r, true) }
func (s *Server) resumeQueue(w http.ResponseWriter, r *http.Request) { s.setPaused(w, r, false) }

func (s *Server) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	q, ok := s.queueFor(w, r)
	if !ok {
		return
	}
	reason := ""
	if paused {
		reason = "manual"
	}
	if err := s.st.SetQueuePaused(r.Context(), q.Name, paused, reason); err != nil {
		s.serverError(w, "could not change queue pause state", err)
		return
	}
	if !paused {
		// Resuming a queue that was auto-paused for a missing capability is how
		// an operator says "I fixed the config"; let the executor try again.
		s.exec.Wake()
	}
	rt, err := s.st.QueueRuntime(r.Context(), q.Name)
	if err != nil {
		s.serverError(w, "could not read queue runtime", err)
		return
	}
	writeJSON(w, http.StatusOK, rt)
}

// --- executor ---

type executorView struct {
	store.Executor
	RunningJob   string `json:"running_job,omitempty"`
	RunningQueue string `json:"running_queue,omitempty"`
	TotalDepth   int    `json:"total_depth"`
}

func (s *Server) getExecutor(w http.ResponseWriter, r *http.Request) {
	state, err := s.st.ExecutorState(r.Context())
	if err != nil {
		s.serverError(w, "could not read executor state", err)
		return
	}
	depths, err := s.st.Depths(r.Context())
	if err != nil {
		s.serverError(w, "could not read queue depths", err)
		return
	}
	total := 0
	for _, n := range depths {
		total += n
	}
	view := executorView{Executor: state, TotalDepth: total}
	if id, queue, ok := s.exec.Running(); ok {
		view.RunningJob, view.RunningQueue = id, queue
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) pauseExecutor(w http.ResponseWriter, r *http.Request) {
	if err := s.st.SetExecutorState(r.Context(), store.ExecPaused, "manual", nil); err != nil {
		s.serverError(w, "could not pause executor", err)
		return
	}
	s.getExecutor(w, r)
}

// resumeExecutor clears a manual pause and also lifts a usage block early,
// which is the documented way to say "my limit reset, try now" (§3.7).
//
// It deliberately does not clear a blocked_auth state: that would just make
// the next job fail. Re-login is what clears it.
func (s *Server) resumeExecutor(w http.ResponseWriter, r *http.Request) {
	state, err := s.st.ExecutorState(r.Context())
	if err != nil {
		s.serverError(w, "could not read executor state", err)
		return
	}
	if state.State == store.ExecBlockedAuth {
		writeError(w, http.StatusConflict, "blocked_auth",
			"the executor is blocked on authentication; re-login to clear it")
		return
	}
	if err := s.st.SetExecutorState(r.Context(), store.ExecReady, "", nil); err != nil {
		s.serverError(w, "could not resume executor", err)
		return
	}
	s.exec.Wake()
	s.getExecutor(w, r)
}
