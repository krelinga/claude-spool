package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/claude-spool/backend/internal/claudecli"
	"github.com/krelinga/claude-spool/backend/internal/config"
	"github.com/krelinga/claude-spool/backend/internal/event"
	"github.com/krelinga/claude-spool/backend/internal/store"
)

// fakeClaude stands in for the CLI. Its `auth status` and `auth login`
// behaviour reproduces what the spike captured from 2.1.282, including the
// OSC 8 duplicated URL and the newline-free prompt.
const fakeClaude = `#!/bin/bash
mode="$1"
sub="$2"

if [ "$mode" = "auth" ] && [ "$sub" = "status" ]; then
  if [ -f "$SPOOL_FAKE_STATE/logged_out" ]; then
    echo '{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}'
    exit 1
  fi
  echo '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","email":"me@example.com","orgName":"Org","subscriptionType":"max"}'
  exit 0
fi

if [ "$mode" = "auth" ] && [ "$sub" = "login" ]; then
  URL='https://claude.com/cai/oauth/authorize?code=true&client_id=abc&state=xyz&code_challenge_method=S256'
  printf 'Opening browser to sign in\xe2\x80\xa6\r\n'
  # The URL twice: once inside an OSC 8 hyperlink, once as visible blue text.
  printf 'If the browser didnt open, visit: \033]8;;%s\007\033[94m%s\033[39m\033]8;;\007\r\n' "$URL" "$URL"
  printf 'Paste code here if prompted > '
  read -r code
  if [ "$code" = "goodcode" ]; then
    rm -f "$SPOOL_FAKE_STATE/logged_out"
    printf 'Login successful.\r\n'
    exit 0
  fi
  printf 'Invalid authorization code.\r\n'
  exit 1
fi

# A -p run: the keep-alive.
if [ -f "$SPOOL_FAKE_STATE/logged_out" ]; then
  echo '{"type":"result","subtype":"success","is_error":true,"result":"Not logged in \xc2\xb7 Please run /login","terminal_reason":"api_error"}'
  exit 1
fi
if [ -f "$SPOOL_FAKE_STATE/rate_limited" ]; then
  echo '{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached","terminal_reason":"api_error"}'
  exit 1
fi
echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"ok","terminal_reason":"completed"}'
`

type harness struct {
	m      *Manager
	st     *store.Store
	cfg    *config.Config
	lock   *claudecli.Lock
	state  string
	events *collector
}

type collector struct {
	mu   sync.Mutex
	seen []event.Envelope
}

func (c *collector) add(e event.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, e)
}

func (c *collector) count(t event.Type) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.seen {
		if e.Event == t {
			n++
		}
	}
	return n
}

// waitFor blocks until at least n events of a type have been delivered. The
// broker hands off through a goroutine, so reading immediately is a race.
func (c *collector) waitFor(t *testing.T, typ event.Type, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.count(typ) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d %s events, saw %d", n, typ, c.count(typ))
}

// settle gives any spurious extra event time to arrive before asserting a
// count, so an "exactly once" test cannot pass by reading too early.
func (c *collector) settle() { time.Sleep(150 * time.Millisecond) }

func (c *collector) find(t event.Type) (event.Envelope, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.seen {
		if e.Event == t {
			return e, true
		}
	}
	return event.Envelope{}, false
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tmp := t.TempDir()

	bin := filepath.Join(tmp, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeState := filepath.Join(tmp, "state")
	if err := os.MkdirAll(fakeState, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOOL_FAKE_STATE", fakeState)

	cfg := &config.Config{
		DataDir:   filepath.Join(tmp, "data"),
		PublicURL: "https://spool.lan",
		Claude: config.ClaudeConfig{
			Binary: bin, ConfigDir: filepath.Join(tmp, "data", "claude"),
			CredentialMode: config.ModeLogin, SyncSkills: true,
			KeepaliveInterval: config.Duration(4 * time.Hour),
			LoginAttemptTTL:   config.Duration(2 * time.Minute),
		},
		Webhooks: []config.WebhookConfig{{
			Name: "test", URL: "http://127.0.0.1:1/hook", Secret: "test-secret-0123456789",
		}},
		WebhookRetryWindow: config.Duration(24 * time.Hour),
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	broker := event.NewBroker()
	ch, stop := broker.Subscribe()
	coll := &collector{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range ch {
			coll.add(e)
		}
	}()
	t.Cleanup(func() { stop(); <-done })

	qs, err := config.ParseQueues([]byte("queues:\n  adhoc:\n    prompt: \"{{input}}\"\n    tools: [Skill]\n"))
	if err != nil {
		t.Fatal(err)
	}
	notifier := event.NewNotifier(cfg, func() *config.QueueSet { return qs }, broker)
	lock := claudecli.NewLock()
	m := New(cfg, st, lock, notifier, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &harness{m: m, st: st, cfg: cfg, lock: lock, state: fakeState, events: coll}
}

func (h *harness) logOut(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.state, "logged_out"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) rateLimit(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.state, "rate_limited"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- probe ---

func TestProbeReportsLoggedIn(t *testing.T) {
	h := newHarness(t)
	st, err := h.m.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if st.State != StateOK {
		t.Errorf("State = %q", st.State)
	}
	if st.Account != "me@example.com" {
		t.Errorf("Account = %q", st.Account)
	}
	if st.Mode != config.ModeLogin {
		t.Errorf("Mode = %q", st.Mode)
	}
}

// auth status exits 1 when logged out but still prints usable JSON, so the
// non-zero exit must not be mistaken for an unparseable response.
func TestProbeHandlesLoggedOutExitCode(t *testing.T) {
	h := newHarness(t)
	h.logOut(t)
	st, err := h.m.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe returned an error for a logged-out CLI: %v", err)
	}
	if st.State != StateExpired {
		t.Errorf("State = %q, want expired", st.State)
	}
	// It blocks the executor and notifies, once.
	exec, _ := h.st.ExecutorState(context.Background())
	if exec.State != store.ExecBlockedAuth {
		t.Errorf("executor state = %v, want blocked_auth", exec.State)
	}
	h.events.waitFor(t, event.AuthRequired, 1)
	env, _ := h.events.find(event.AuthRequired)
	if env.Auth == nil || env.Auth.LoginURL != "https://spool.lan/v1/auth/login" {
		t.Errorf("auth.required must carry an actionable login URL: %+v", env.Auth)
	}
}

// One auth.required per incident, however many checks fail (§3.2).
func TestRepeatedFailuresNotifyOnce(t *testing.T) {
	h := newHarness(t)
	h.logOut(t)
	for range 3 {
		h.m.Probe(context.Background())
		h.m.Keepalive(context.Background())
	}
	h.events.waitFor(t, event.AuthRequired, 1)
	h.events.settle()
	if n := h.events.count(event.AuthRequired); n != 1 {
		t.Errorf("auth.required fired %d times, want 1", n)
	}
}

// --- keep-alive ---

func TestKeepaliveRecordsSuccess(t *testing.T) {
	h := newHarness(t)
	if err := h.m.Keepalive(context.Background()); err != nil {
		t.Fatalf("Keepalive: %v", err)
	}
	ev, ok, err := h.st.LastAuthEvent(context.Background(), store.AuthProbeOK)
	if err != nil || !ok {
		t.Fatalf("no probe_ok recorded: %v %v", ok, err)
	}
	if ev.At.IsZero() {
		t.Error("probe_ok has no timestamp")
	}
	if h.m.State() != StateOK {
		t.Errorf("State = %q", h.m.State())
	}
}

func TestKeepaliveDetectsExpiry(t *testing.T) {
	h := newHarness(t)
	h.logOut(t)
	if err := h.m.Keepalive(context.Background()); err != nil {
		t.Fatalf("Keepalive: %v", err)
	}
	if h.m.State() != StateExpired {
		t.Errorf("State = %q, want expired", h.m.State())
	}
	if _, ok, _ := h.st.LastAuthEvent(context.Background(), store.AuthExpired); !ok {
		t.Error("no expired event recorded")
	}
}

// A usage limit is not an expiry: mistaking one for the other would block on
// auth and demand a pointless re-login.
func TestKeepaliveDoesNotTreatRateLimitAsExpiry(t *testing.T) {
	h := newHarness(t)
	h.rateLimit(t)
	if err := h.m.Keepalive(context.Background()); err != nil {
		t.Fatalf("Keepalive: %v", err)
	}
	if h.m.State() == StateExpired {
		t.Error("a usage limit was classified as an auth expiry")
	}
	exec, _ := h.st.ExecutorState(context.Background())
	if exec.State == store.ExecBlockedAuth {
		t.Error("executor blocked on auth because of a usage limit")
	}
	h.events.settle()
	if h.events.count(event.AuthRequired) != 0 {
		t.Error("auth.required fired for a usage limit")
	}
}

// The keep-alive holds the same lock as a job (§3.2).
func TestKeepaliveTakesTheClaudeLock(t *testing.T) {
	h := newHarness(t)
	release, ok := h.lock.TryAcquire("pretend job")
	if !ok {
		t.Fatal("could not take the lock")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := h.m.Keepalive(ctx)
	if err == nil {
		t.Error("keep-alive ran while a job held the lock")
	}
	release()

	if err := h.m.Keepalive(context.Background()); err != nil {
		t.Errorf("keep-alive failed once the lock was free: %v", err)
	}
}

// --- recovery ---

func TestAuthRestoredUnblocksExecutor(t *testing.T) {
	h := newHarness(t)
	h.logOut(t)
	h.m.Keepalive(context.Background())
	if exec, _ := h.st.ExecutorState(context.Background()); exec.State != store.ExecBlockedAuth {
		t.Fatal("not blocked")
	}

	var woken int
	h.m.wakeExecutor = func() { woken++ }
	if err := os.Remove(filepath.Join(h.state, "logged_out")); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Keepalive(context.Background()); err != nil {
		t.Fatal(err)
	}

	if h.m.State() != StateOK {
		t.Errorf("State = %q", h.m.State())
	}
	exec, _ := h.st.ExecutorState(context.Background())
	if exec.State != store.ExecReady {
		t.Errorf("executor state = %v, want ready", exec.State)
	}
	h.events.waitFor(t, event.AuthRestored, 1)
	h.events.settle()
	if n := h.events.count(event.AuthRestored); n != 1 {
		t.Errorf("auth.restored fired %d times, want 1", n)
	}
	if woken == 0 {
		t.Error("restoring auth should wake the executor so queued work starts")
	}
}

// ObserveAuthFailure is how the executor reports a job's auth failure, so the
// state machine stays in one place.
func TestObserveAuthFailureFromExecutor(t *testing.T) {
	h := newHarness(t)
	h.m.ObserveAuthFailure(context.Background(), "Login expired. Run /login.")
	if h.m.State() != StateExpired {
		t.Errorf("State = %q", h.m.State())
	}
	h.events.waitFor(t, event.AuthRequired, 1)
	exec, _ := h.st.ExecutorState(context.Background())
	if exec.State != store.ExecBlockedAuth {
		t.Errorf("executor state = %v", exec.State)
	}
}

// --- login flow, under a real PTY ---

func TestLoginFlow(t *testing.T) {
	h := newHarness(t)
	h.logOut(t)
	h.m.Probe(context.Background())

	id, url, expiresAt, err := h.m.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if id == "" {
		t.Error("no attempt id")
	}
	// The URL must be the clean one, not the OSC 8 copy with escapes attached.
	if !strings.HasPrefix(url, "https://claude.com/cai/oauth/authorize?") {
		t.Errorf("URL = %q", url)
	}
	if strings.ContainsAny(url, "\x1b\x07\r\n") {
		t.Errorf("URL carries control characters: %q", url)
	}
	if !strings.Contains(url, "state=xyz") {
		t.Errorf("URL lost its query: %q", url)
	}
	if !expiresAt.After(time.Now()) {
		t.Errorf("ExpiresAt = %v", expiresAt)
	}
	if _, ok, _ := h.st.LastAuthEvent(context.Background(), store.AuthLoginStarted); !ok {
		t.Error("login_started not recorded")
	}
	// The attempt holds the claude lock for its whole life.
	if _, free := h.lock.TryAcquire("other"); free {
		t.Error("the lock was free during a login attempt")
	}

	status, err := h.m.SubmitCode(context.Background(), id, "goodcode")
	if err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if status.State != StateOK {
		t.Errorf("State = %q", status.State)
	}
	if _, ok, _ := h.st.LastAuthEvent(context.Background(), store.AuthLoginCompleted); !ok {
		t.Error("login_completed not recorded")
	}
	exec, _ := h.st.ExecutorState(context.Background())
	if exec.State != store.ExecReady {
		t.Errorf("executor state = %v, want ready", exec.State)
	}
	h.events.waitFor(t, event.AuthRestored, 1)
	// And the lock is free again.
	release, free := h.lock.TryAcquire("after")
	if !free {
		t.Fatal("the lock was not released after the login completed")
	}
	release()
}

func TestLoginWithBadCode(t *testing.T) {
	h := newHarness(t)
	h.logOut(t)

	id, _, _, err := h.m.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.SubmitCode(context.Background(), id, "wrongcode"); err == nil {
		t.Fatal("a bad code was accepted")
	}
	if _, ok, _ := h.st.LastAuthEvent(context.Background(), store.AuthLoginFailed); !ok {
		t.Error("login_failed not recorded")
	}
	// A failed attempt must still free the lock, or nothing can run again.
	release, free := h.lock.TryAcquire("after")
	if !free {
		t.Fatal("the lock leaked after a failed login")
	}
	release()
	// And it must not leave a phantom attempt behind.
	if h.m.Status(context.Background()).LoginInProgress {
		t.Error("attempt still reported in progress after failing")
	}
}

func TestSubmitCodeUnknownAttempt(t *testing.T) {
	h := newHarness(t)
	if _, err := h.m.SubmitCode(context.Background(), "login_nope", "x"); err == nil {
		t.Error("expected an error for an unknown attempt")
	}
}

// A repeated start returns the same attempt rather than stranding the first.
func TestStartLoginTwiceReturnsSameAttempt(t *testing.T) {
	h := newHarness(t)
	id1, url1, _, err := h.m.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id2, url2, _, err := h.m.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("second StartLogin: %v", err)
	}
	if id1 != id2 || url1 != url2 {
		t.Errorf("got a second attempt: %s/%s vs %s/%s", id1, url1, id2, url2)
	}
	h.m.CancelLogin(context.Background(), id1)
}

func TestCancelLoginFreesTheLock(t *testing.T) {
	h := newHarness(t)
	id, _, _, err := h.m.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.CancelLogin(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	release, free := h.lock.TryAcquire("after")
	if !free {
		t.Fatal("lock not released after cancelling")
	}
	release()
	if h.m.Status(context.Background()).LoginInProgress {
		t.Error("attempt still in progress after cancel")
	}
}

// An abandoned attempt must not hold the lock forever.
func TestLoginAttemptExpires(t *testing.T) {
	h := newHarness(t)
	h.cfg.Claude.LoginAttemptTTL = config.Duration(300 * time.Millisecond)

	id, _, _, err := h.m.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, free := h.lock.TryAcquire("probe"); free {
			// Reacquired: the expired attempt let go.
			if _, err := h.m.SubmitCode(context.Background(), id, "goodcode"); err == nil {
				t.Error("an expired attempt still accepted a code")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("an abandoned login attempt never released the claude lock")
}

// --- cadence measurement ---

func TestSessionLifetimesAndStatus(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	base := time.Now().Add(-10 * 24 * time.Hour)

	// Two full sessions, then a third still running.
	h.st.RecordAuthEvent(ctx, store.AuthLoginCompleted, "a", base)
	h.st.RecordAuthEvent(ctx, store.AuthExpired, "a", base.Add(72*time.Hour))
	h.st.RecordAuthEvent(ctx, store.AuthLoginCompleted, "b", base.Add(73*time.Hour))
	h.st.RecordAuthEvent(ctx, store.AuthExpired, "b", base.Add(97*time.Hour))
	h.st.RecordAuthEvent(ctx, store.AuthLoginCompleted, "c", base.Add(98*time.Hour))

	lifetimes, err := h.st.SessionLifetimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(lifetimes) != 2 {
		t.Fatalf("lifetimes = %v, want 2 (the live session is not reported)", lifetimes)
	}
	if lifetimes[0] != 72*time.Hour || lifetimes[1] != 24*time.Hour {
		t.Errorf("lifetimes = %v", lifetimes)
	}

	status := h.m.Status(ctx)
	if status.Relogins != 3 {
		t.Errorf("Relogins = %d, want 3", status.Relogins)
	}
	if status.SessionAgeSec == nil {
		t.Fatal("no session age reported")
	}
	// Age is measured from the most recent completed login.
	wantAge := time.Since(base.Add(98 * time.Hour)).Seconds()
	if diff := *status.SessionAgeSec - wantAge; diff > 5 || diff < -5 {
		t.Errorf("SessionAgeSec = %v, want about %v", *status.SessionAgeSec, wantAge)
	}
}

// Under long_lived_token there is no session to keep warm, so the loop must
// exit rather than spin.
func TestRunIsIdleUnderLongLivedToken(t *testing.T) {
	h := newHarness(t)
	h.cfg.Claude.CredentialMode = config.ModeLongLivedToken
	done := make(chan error, 1)
	go func() { done <- h.m.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not return under long_lived_token")
	}
}

// Captured verbatim from CLI 2.1.282 (backend/spike/out/40-auth-status). The probe must
// parse the real thing, not just the fake's approximation.
func TestParseRealAuthStatusOutput(t *testing.T) {
	const loggedIn = `{
  "loggedIn": true,
  "authMethod": "claude.ai",
  "apiProvider": "firstParty",
  "analyticsDisabled": false,
  "projectsDirectory": "/data/claude/projects",
  "configDirectory": "/data/claude",
  "email": "me@example.com",
  "orgId": "36f38d57-a6f3-4eaa-b60d-f554d79989ed",
  "orgName": "me@example.com's Organization",
  "subscriptionType": "max"
}`
	const loggedOut = `{
  "loggedIn": false,
  "authMethod": "none",
  "apiProvider": "firstParty",
  "analyticsDisabled": false,
  "projectsDirectory": "/data/claude/projects",
  "configDirectory": "/data/claude"
}`

	var in authStatusJSON
	if err := json.Unmarshal([]byte(loggedIn), &in); err != nil {
		t.Fatalf("real logged-in output did not parse: %v", err)
	}
	if !in.LoggedIn || in.AuthMethod != "claude.ai" || in.Email != "me@example.com" {
		t.Errorf("parsed = %+v", in)
	}
	if in.SubscriptionType != "max" {
		t.Errorf("SubscriptionType = %q", in.SubscriptionType)
	}

	var out authStatusJSON
	if err := json.Unmarshal([]byte(loggedOut), &out); err != nil {
		t.Fatalf("real logged-out output did not parse: %v", err)
	}
	if out.LoggedIn {
		t.Error("logged-out output parsed as logged in")
	}
	// Logged out carries no email, so the account falls back to the org name,
	// which is also absent — the caller must tolerate an empty account.
	if out.Email != "" || out.OrgName != "" {
		t.Errorf("unexpected identity while logged out: %+v", out)
	}
}

// trimToJSON exists because a banner could precede the JSON body.
func TestTrimToJSON(t *testing.T) {
	cases := map[string]string{
		`{"loggedIn":true}`:             `{"loggedIn":true}`,
		`warning: x` + "\n" + `{"a":1}`: `{"a":1}`,
		`no json here`:                  `no json here`,
	}
	for in, want := range cases {
		if got := string(trimToJSON([]byte(in))); got != want {
			t.Errorf("trimToJSON(%q) = %q, want %q", in, got, want)
		}
	}
}
