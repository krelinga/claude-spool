// Package api serves the HTTP surface described in design §3.7.
//
// Queues are exposed read-only. Their definitions carry prompts and tool
// allowlists — security policy — so they come from queues.yaml and a client
// can never widen them (§3.3).
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/krelinga/claude-spool/backend/internal/config"
	"github.com/krelinga/claude-spool/backend/internal/event"
	"github.com/krelinga/claude-spool/backend/internal/store"
)

// Executor is the part of the executor the API needs. Keeping it narrow means
// the API cannot start or stop a run by accident.
type Executor interface {
	Wake()
	Running() (id, queue string, ok bool)
	// Cancel stops the named job if it is the one currently running. It reports
	// whether it took effect; the executor records the outcome itself.
	Cancel(id string) bool
}

type Server struct {
	cfg    *config.Config
	st     *store.Store
	queues func() *config.QueueSet
	exec   Executor
	auth   AuthManager
	notify *event.Notifier
	broker *event.Broker
	log    *slog.Logger
	now    func() time.Time
}

func New(cfg *config.Config, st *store.Store, queues func() *config.QueueSet, exec Executor,
	authMgr AuthManager, notify *event.Notifier, broker *event.Broker, log *slog.Logger) *Server {
	return &Server{
		cfg: cfg, st: st, queues: queues, exec: exec, auth: authMgr,
		notify: notify, broker: broker, log: log, now: time.Now,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: liveness only, nothing about the workload.
	mux.HandleFunc("GET /healthz", s.healthz)
	// Prometheus scrapes without a bearer token in practice, and the design
	// puts this outside /v1 (§3.7). It exposes counts and states, never job
	// content. Keep it on the LAN side of Caddy.
	mux.HandleFunc("GET /metrics", s.metrics)

	v1 := http.NewServeMux()
	v1.HandleFunc("GET /v1/queues", s.listQueues)
	v1.HandleFunc("GET /v1/queues/{queue}", s.getQueue)
	v1.HandleFunc("POST /v1/queues/{queue}/jobs", s.submitJob)
	v1.HandleFunc("GET /v1/queues/{queue}/jobs", s.listQueueJobs)
	v1.HandleFunc("POST /v1/queues/{queue}/pause", s.pauseQueue)
	v1.HandleFunc("POST /v1/queues/{queue}/resume", s.resumeQueue)
	v1.HandleFunc("GET /v1/jobs", s.listJobs)
	v1.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	v1.HandleFunc("GET /v1/jobs/{id}/transcript", s.getTranscript)
	v1.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancelJob)
	v1.HandleFunc("POST /v1/jobs/{id}/retry", s.retryJob)
	v1.HandleFunc("POST /v1/jobs/{id}/reply", s.replyJob)
	v1.HandleFunc("GET /v1/executor", s.getExecutor)
	v1.HandleFunc("POST /v1/executor/pause", s.pauseExecutor)
	v1.HandleFunc("POST /v1/executor/resume", s.resumeExecutor)
	v1.HandleFunc("GET /v1/auth", s.getAuth)
	v1.HandleFunc("POST /v1/auth/check", s.checkAuth)
	v1.HandleFunc("POST /v1/auth/login", s.startLogin)
	v1.HandleFunc("POST /v1/auth/login/{attempt}", s.submitLoginCode)
	v1.HandleFunc("DELETE /v1/auth/login/{attempt}", s.cancelLogin)
	v1.HandleFunc("GET /v1/auth/events", s.listAuthEvents)
	v1.HandleFunc("GET /v1/events", s.streamEvents)
	v1.HandleFunc("GET /v1/metrics", s.metrics)

	mux.Handle("/v1/", s.authenticate(v1))
	return s.recoverPanics(mux)
}

// --- middleware ---

type ctxKey int

const tokenKey ctxKey = iota

func tokenFrom(r *http.Request) *config.TokenConfig {
	t, _ := r.Context().Value(tokenKey).(*config.TokenConfig)
	return t
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="spool"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "a bearer token is required")
			return
		}
		tok, ok := s.cfg.Lookup(presented)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="spool"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "unknown token")
			return
		}
		ctx := contextWithToken(r.Context(), tok)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, rest, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- responses ---

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (s *Server) serverError(w http.ResponseWriter, msg string, err error) {
	s.log.Error(msg, "error", err)
	writeError(w, http.StatusInternalServerError, "internal", msg)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DB().PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unhealthy", "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// queueFor resolves the queue named in the path, enforcing token scope.
//
// An out-of-scope queue reports 404 rather than 403: a scoped token should not
// be able to enumerate queues it cannot reach.
func (s *Server) queueFor(w http.ResponseWriter, r *http.Request) (*config.Queue, bool) {
	name := r.PathValue("queue")
	tok := tokenFrom(r)
	q, ok := s.queues().Get(name)
	if !ok || tok == nil || !tok.AllowsQueue(name) {
		writeError(w, http.StatusNotFound, "no_such_queue", "no such queue")
		return nil, false
	}
	return q, true
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
