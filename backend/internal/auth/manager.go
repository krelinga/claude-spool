// Package auth owns the credential lifecycle: probing, keep-alive, the
// re-login flow, and the history that measures how often re-auth actually
// happens (design §3.2).
//
// The design's whole approach to re-auth is to make it rare, visible early,
// harmless, and quick — and to *measure* it, because the real cadence is not
// documented and could be weeks or could be daily.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/claude-spool-be/backend/internal/claudecli"
	"github.com/krelinga/claude-spool-be/backend/internal/config"
	"github.com/krelinga/claude-spool-be/backend/internal/event"
	"github.com/krelinga/claude-spool-be/backend/internal/store"
)

// State is the credential state the API reports.
type State string

const (
	// StateUnknown is before the first probe has run.
	StateUnknown State = "unknown"
	StateOK      State = "ok"
	StateExpired State = "expired"
)

const (
	// probeTimeout bounds `claude auth status`, which is local-only and took
	// 64ms in the spike.
	probeTimeout = 15 * time.Second
	// keepaliveTimeout bounds the minimal real request.
	keepaliveTimeout = 2 * time.Minute
	// keepaliveModel is deliberately the cheapest model: this call exists to
	// refresh a token, not to do work.
	keepaliveModel = "haiku"
	// tickInterval is how often the loop wakes to decide whether anything is
	// due. The keep-alive interval itself is configured.
	tickInterval = time.Minute
)

// Status is the full picture behind GET /v1/auth.
type Status struct {
	State   State                 `json:"state"`
	Account string                `json:"account,omitempty"`
	Mode    config.CredentialMode `json:"mode"`

	LastOKAt      *time.Time     `json:"last_ok_at,omitempty"`
	LastLoginAt   *time.Time     `json:"last_login_at,omitempty"`
	SessionAge    *time.Duration `json:"-"`
	SessionAgeSec *float64       `json:"session_age_seconds,omitempty"`
	Relogins      int            `json:"relogins_total"`

	// LoginInProgress is an attempt awaiting its code.
	LoginInProgress bool   `json:"login_in_progress"`
	LoginURL        string `json:"login_url,omitempty"`
}

// authStatusJSON is what `claude auth status` prints. Verified on CLI 2.1.282:
// JSON by default, exit 1 when logged out, and no expiry field — which is why
// the keep-alive request, not this, is the authoritative check.
type authStatusJSON struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	Email            string `json:"email"`
	OrgName          string `json:"orgName"`
	SubscriptionType string `json:"subscriptionType"`
}

type Manager struct {
	cfg    *config.Config
	st     *store.Store
	lock   *claudecli.Lock
	notify *event.Notifier
	log    *slog.Logger
	now    func() time.Time

	// wakeSender nudges the webhook sender once events are committed.
	wakeSender func()
	// wakeExecutor nudges the executor when auth is restored, so queued work
	// starts immediately rather than at the next poll.
	wakeExecutor func()

	mu      sync.Mutex
	state   State
	account string
	attempt *loginAttempt

	// lastJobRunAt lets the keep-alive skip when a real job has just run: that
	// request already refreshed the token.
	lastJobRunAt time.Time
}

type Option func(*Manager)

func WithClock(now func() time.Time) Option {
	return func(m *Manager) { m.now = now }
}
func WithSenderWake(f func()) Option {
	return func(m *Manager) { m.wakeSender = f }
}
func WithExecutorWake(f func()) Option {
	return func(m *Manager) { m.wakeExecutor = f }
}

func New(cfg *config.Config, st *store.Store, lock *claudecli.Lock, notify *event.Notifier,
	log *slog.Logger, opts ...Option) *Manager {
	m := &Manager{
		cfg: cfg, st: st, lock: lock, notify: notify, log: log,
		now:          time.Now,
		state:        StateUnknown,
		wakeSender:   func() {},
		wakeExecutor: func() {},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// NoteJobRan records that a real request just went out, so the keep-alive can
// skip its next turn.
func (m *Manager) NoteJobRan(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastJobRunAt = at
}

// ObserveAuthFailure lets the executor report an auth failure it classified
// from a job, so the credential state machine lives in one place.
func (m *Manager) ObserveAuthFailure(ctx context.Context, detail string) {
	m.observeExpired(ctx, detail)
}

// State returns the cached credential state.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Run probes once at startup, then keeps the session warm.
func (m *Manager) Run(ctx context.Context) error {
	if m.cfg.Claude.CredentialMode != config.ModeLogin {
		// Under long_lived_token there is no session to keep warm: the token
		// lasts a year and skills are vendored (§3.2).
		m.log.Info("auth manager idle", "credential_mode", m.cfg.Claude.CredentialMode)
		return nil
	}

	if _, err := m.Probe(ctx); err != nil && !errors.Is(err, context.Canceled) {
		m.log.Warn("initial auth probe failed", "error", err)
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if m.keepaliveDue(ctx) {
				if err := m.Keepalive(ctx); err != nil && !errors.Is(err, context.Canceled) {
					m.log.Warn("keep-alive failed", "error", err)
				}
			}
		}
	}
}

// keepaliveDue reports whether the session needs a nudge.
func (m *Manager) keepaliveDue(ctx context.Context) bool {
	interval := m.cfg.Claude.KeepaliveInterval.Duration()
	if interval <= 0 {
		return false
	}
	now := m.now()

	m.mu.Lock()
	lastJob := m.lastJobRunAt
	inLogin := m.attempt != nil
	state := m.state
	m.mu.Unlock()

	// Never interrupt a login in progress; it holds the lock anyway.
	if inLogin {
		return false
	}
	// While expired, a keep-alive would just fail again. Probe cheaply instead,
	// so a re-login performed by other means is still noticed.
	if state == StateExpired {
		if _, err := m.Probe(ctx); err != nil {
			return false
		}
		return m.State() == StateOK
	}
	// A real job that ran recently already refreshed the token (§3.2).
	if !lastJob.IsZero() && now.Sub(lastJob) < interval {
		return false
	}
	last, ok, err := m.st.LastAuthEvent(ctx, store.AuthProbeOK)
	if err != nil {
		m.log.Error("could not read last auth probe", "error", err)
		return false
	}
	if !ok {
		return true
	}
	return now.Sub(last.At) >= interval
}

// Probe runs `claude auth status`: cheap, local-only, and safe to call often.
//
// It does not take the claude lock. The spike measured it at 64ms with no
// network round trip, so it neither refreshes nor rotates anything — unlike the
// keep-alive, which does and therefore must be serialised.
func (m *Manager) Probe(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.cfg.Claude.Binary, "auth", "status")
	cmd.Env = claudecli.Invocation{ConfigDir: m.cfg.Claude.ConfigDir}.Env(os.Environ())
	out, err := cmd.Output()

	var parsed authStatusJSON
	// Exit 1 means logged out, and still prints usable JSON.
	if jsonErr := json.Unmarshal(trimToJSON(out), &parsed); jsonErr != nil {
		if err != nil {
			return m.Status(ctx), fmt.Errorf("auth status: %w", err)
		}
		return m.Status(ctx), fmt.Errorf("auth status returned unparseable output: %w", jsonErr)
	}

	account := parsed.Email
	if account == "" {
		account = parsed.OrgName
	}
	if parsed.LoggedIn {
		m.setState(StateOK, account)
	} else {
		m.observeExpired(ctx, "auth status reports not logged in")
	}
	return m.Status(ctx), nil
}

// trimToJSON tolerates a stray banner before the JSON body.
func trimToJSON(b []byte) []byte {
	if i := strings.IndexByte(string(b), '{'); i > 0 {
		return b[i:]
	}
	return b
}

// Keepalive makes one minimal real request, which is the authoritative check
// that the session still works and the thing that keeps the access token
// refreshing inside its window (§3.2).
func (m *Manager) Keepalive(ctx context.Context) error {
	// Through the same lock as a job: nothing may race the refresh token.
	release, err := m.lock.Acquire(ctx, "keepalive")
	if err != nil {
		return err
	}
	defer release()

	runCtx, cancel := context.WithTimeout(ctx, keepaliveTimeout)
	defer cancel()

	inv := claudecli.Invocation{
		Binary:     m.cfg.Claude.Binary,
		Prompt:     "reply with the single word ok",
		ConfigDir:  m.cfg.Claude.ConfigDir,
		Model:      keepaliveModel,
		MaxTurns:   1,
		Tools:      []string{},
		SyncSkills: m.cfg.Claude.SyncSkills,
	}
	cmd := inv.Command(runCtx)
	out, runErr := cmd.CombinedOutput()

	if isAuthFailure(string(out)) {
		m.observeExpired(ctx, firstLine(string(out)))
		return nil
	}
	if runErr != nil {
		// A failure that is not an auth failure says nothing about the
		// credential: a network blip or a usage limit must not look like expiry.
		m.log.Warn("keep-alive request failed without an auth error",
			"error", runErr, "output", firstLine(string(out)))
		return nil
	}
	m.recordProbeOK(ctx)
	return nil
}

func isAuthFailure(s string) bool { return claudecli.LooksLikeAuthFailure(s) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func (m *Manager) recordProbeOK(ctx context.Context) {
	now := m.now()
	if err := m.st.RecordAuthEvent(ctx, store.AuthProbeOK, "", now); err != nil {
		m.log.Error("could not record auth probe", "error", err)
	}
	m.transitionToOK(ctx)
}

func (m *Manager) setState(s State, account string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = s
	if account != "" {
		m.account = account
	}
}

// transitionToOK unblocks the executor if it was blocked on auth, and fires
// auth.restored exactly once.
func (m *Manager) transitionToOK(ctx context.Context) {
	m.mu.Lock()
	was := m.state
	m.state = StateOK
	m.mu.Unlock()

	exec, err := m.st.ExecutorState(ctx)
	if err != nil {
		m.log.Error("could not read executor state", "error", err)
		return
	}
	if was != StateExpired && exec.State != store.ExecBlockedAuth {
		return
	}
	if exec.State == store.ExecBlockedAuth {
		if err := m.st.SetExecutorState(ctx, store.ExecReady, "", nil); err != nil {
			m.log.Error("could not clear blocked_auth", "error", err)
			return
		}
	}
	m.log.Info("authentication restored; executor unblocked")
	m.emit(ctx, m.authEnvelope(ctx, event.AuthRestored, "ok"))
	m.wakeExecutor()
}

// observeExpired records an expiry, blocks the executor, and fires
// auth.required — once per incident, not once per failed check (§3.2).
func (m *Manager) observeExpired(ctx context.Context, detail string) {
	m.mu.Lock()
	was := m.state
	m.state = StateExpired
	m.mu.Unlock()

	if was == StateExpired {
		return
	}
	now := m.now()
	if err := m.st.RecordAuthEvent(ctx, store.AuthExpired, detail, now); err != nil {
		m.log.Error("could not record auth expiry", "error", err)
	}
	exec, err := m.st.ExecutorState(ctx)
	if err != nil {
		m.log.Error("could not read executor state", "error", err)
	}
	if exec.State != store.ExecBlockedAuth {
		if err := m.st.SetExecutorState(ctx, store.ExecBlockedAuth, detail, nil); err != nil {
			m.log.Error("could not set blocked_auth", "error", err)
		}
	}
	m.log.Warn("authentication expired; executor blocked", "detail", detail)
	m.emit(ctx, m.authEnvelope(ctx, event.AuthRequired, "expired"))
}

func (m *Manager) authEnvelope(ctx context.Context, t event.Type, state string) event.Envelope {
	env := m.notify.Envelope(t)
	depth := 0
	if depths, err := m.st.Depths(ctx); err == nil {
		for _, n := range depths {
			depth += n
		}
	}
	info := &event.AuthInfo{State: state, QueuedJobs: depth, LoginURL: m.loginEndpoint()}
	if ev, ok, err := m.st.LastAuthEvent(ctx, store.AuthExpired); err == nil && ok && state == "expired" {
		at := ev.At
		info.Since = &at
	}
	env.Auth = info
	return env
}

func (m *Manager) loginEndpoint() string {
	if m.cfg.PublicURL == "" {
		return ""
	}
	return strings.TrimSuffix(m.cfg.PublicURL, "/") + "/v1/auth/login"
}

func (m *Manager) emit(ctx context.Context, env event.Envelope) {
	if err := m.st.EnqueueDeliveries(ctx, m.notify.Deliveries(env), m.now()); err != nil {
		m.log.Error("could not enqueue auth notification", "event", env.Event, "error", err)
		return
	}
	m.wakeSender()
}

// Status assembles the reportable state, including the measured cadence.
func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	s := Status{State: m.state, Account: m.account, Mode: m.cfg.Claude.CredentialMode}
	if m.attempt != nil {
		s.LoginInProgress = true
		s.LoginURL = m.attempt.url
	}
	m.mu.Unlock()

	if ev, ok, err := m.st.LastAuthEvent(ctx, store.AuthProbeOK); err == nil && ok {
		at := ev.At
		s.LastOKAt = &at
	}
	if ev, ok, err := m.st.LastAuthEvent(ctx, store.AuthLoginCompleted); err == nil && ok {
		at := ev.At
		s.LastLoginAt = &at
		age := m.now().Sub(at)
		s.SessionAge = &age
		secs := age.Seconds()
		s.SessionAgeSec = &secs
	}
	if n, err := m.st.AuthEventCount(ctx, store.AuthLoginCompleted); err == nil {
		s.Relogins = n
	}
	return s
}
