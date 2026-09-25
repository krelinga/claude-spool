package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/krelinga/claude-spool-be/internal/claudecli"
	"github.com/krelinga/claude-spool-be/internal/store"
	"github.com/oklog/ulid/v2"
)

// The re-login flow exists to be a 30-second phone task (§3.2): the
// notification links here, the user approves in a browser, pastes the code
// back, and the executor unblocks by itself.
//
// `claude auth login` is interactive, so it runs under a PTY. Its exact
// behaviour was captured in spike probe 4 and is parsed by
// internal/claudecli/login.go — the URL arrives twice on one line, the prompt
// has no trailing newline, and the code is read without echo.

var (
	ErrNoAttempt      = errors.New("no such login attempt")
	ErrAttemptExpired = errors.New("login attempt expired")
	ErrLoginBusy      = errors.New("a login attempt is already in progress")
)

// loginAttempt is one in-flight interactive login. It holds the claude lock for
// its whole life, because a keep-alive or job running concurrently would be
// exactly the refresh-token race the design avoids.
type loginAttempt struct {
	id        string
	url       string
	startedAt time.Time
	expiresAt time.Time

	cmd     *exec.Cmd
	tty     *os.File
	release func()
	cancel  context.CancelFunc

	mu     sync.Mutex
	output strings.Builder
	done   chan struct{}
	// result is set once the attempt concludes.
	err      error
	finished bool
}

func (a *loginAttempt) seen() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.output.String()
}

// StartLogin launches `claude auth login` and returns the URL to approve.
func (m *Manager) StartLogin(ctx context.Context) (id, url string, expiresAt time.Time, err error) {
	m.mu.Lock()
	if m.attempt != nil && !m.attempt.expired(m.now()) {
		existing := m.attempt
		m.mu.Unlock()
		// Returning the in-flight attempt is friendlier than refusing: a phone
		// that retried the request gets the same URL rather than an error.
		return existing.id, existing.url, existing.expiresAt, nil
	}
	m.mu.Unlock()

	// Abandon a stale attempt before starting a new one, so its lock is freed.
	m.abandonStale()

	// Do not block the request indefinitely waiting for a running job.
	acquireCtx, acquireCancel := context.WithTimeout(ctx, 5*time.Second)
	defer acquireCancel()
	release, err := m.lock.Acquire(acquireCtx, "login")
	if err != nil {
		who, _, _ := m.lock.Holder()
		return "", "", time.Time{}, fmt.Errorf("%w: claude is busy (%s)", ErrLoginBusy, who)
	}

	// The attempt outlives this request, so it gets its own context.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cfg.Claude.LoginAttemptTTL.Duration())

	cmd := exec.CommandContext(runCtx, m.cfg.Claude.Binary, "auth", "login")
	cmd.Env = claudecli.Invocation{ConfigDir: m.cfg.Claude.ConfigDir}.Env(os.Environ())

	tty, err := pty.Start(cmd)
	if err != nil {
		cancel()
		release()
		return "", "", time.Time{}, fmt.Errorf("start claude auth login under a pty: %w", err)
	}

	now := m.now()
	a := &loginAttempt{
		id:        "login_" + strings.ToLower(ulid.Make().String()),
		startedAt: now,
		expiresAt: now.Add(m.cfg.Claude.LoginAttemptTTL.Duration()),
		cmd:       cmd, tty: tty, release: release, cancel: cancel,
		done: make(chan struct{}),
	}

	// Drain the PTY continuously: the CLI writes the URL, then the prompt, then
	// the result, and a reader that stops would block the child on write.
	go a.drain()
	go func() {
		<-runCtx.Done()
		// TTL reached, or the attempt concluded; either way stop holding the lock.
		m.finishAttempt(a, ErrAttemptExpired)
	}()

	url, err = a.awaitURL(runCtx)
	if err != nil {
		m.finishAttempt(a, err)
		return "", "", time.Time{}, err
	}
	a.url = url

	m.mu.Lock()
	m.attempt = a
	m.mu.Unlock()

	if err := m.st.RecordAuthEvent(ctx, store.AuthLoginStarted, a.id, now); err != nil {
		m.log.Error("could not record login_started", "error", err)
	}
	m.log.Info("login attempt started", "attempt", a.id, "expires_at", a.expiresAt)
	return a.id, url, a.expiresAt, nil
}

func (a *loginAttempt) expired(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.finished || now.After(a.expiresAt)
}

func (a *loginAttempt) drain() {
	buf := make([]byte, 4096)
	for {
		n, err := a.tty.Read(buf)
		if n > 0 {
			a.mu.Lock()
			a.output.Write(buf[:n])
			a.mu.Unlock()
		}
		if err != nil {
			// EOF or the PTY closing means the child is gone.
			close(a.done)
			return
		}
	}
}

// awaitURL waits for the authorization URL to appear in the output.
func (a *loginAttempt) awaitURL(ctx context.Context) (string, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(30 * time.Second)
	for {
		if url, err := claudecli.ParseLoginURL(a.seen()); err == nil {
			return url, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-a.done:
			// The child exited before printing a URL.
			return "", fmt.Errorf("claude auth login exited without a URL: %s", firstLine(a.seen()))
		case <-deadline:
			return "", fmt.Errorf("timed out waiting for the authorization URL: %s", firstLine(a.seen()))
		case <-ticker.C:
		}
	}
}

// SubmitCode completes an attempt by writing the pasted code to the PTY.
func (m *Manager) SubmitCode(ctx context.Context, id, code string) (Status, error) {
	m.mu.Lock()
	a := m.attempt
	m.mu.Unlock()

	if a == nil || a.id != id {
		return m.Status(ctx), ErrNoAttempt
	}
	if a.expired(m.now()) {
		m.finishAttempt(a, ErrAttemptExpired)
		return m.Status(ctx), ErrAttemptExpired
	}

	code = strings.TrimSpace(code)
	if code == "" {
		return m.Status(ctx), errors.New("code is required")
	}
	// The CLI reads the code without echo, so there is nothing to read back:
	// success is confirmed from the output markers and exit status alone.
	if _, err := io.WriteString(a.tty, code+"\n"); err != nil {
		m.finishAttempt(a, err)
		return m.Status(ctx), fmt.Errorf("write code to claude: %w", err)
	}

	err := a.awaitResult(ctx)
	if err != nil {
		m.recordLoginFailed(ctx, a, err)
		m.finishAttempt(a, err)
		return m.Status(ctx), err
	}

	// Release the lock before probing, since the probe is cheap but the login
	// process must be gone before anything else runs claude.
	m.finishAttempt(a, nil)

	now := m.now()
	if err := m.st.RecordAuthEvent(ctx, store.AuthLoginCompleted, a.id, now); err != nil {
		m.log.Error("could not record login_completed", "error", err)
	}
	if err := m.st.RecordAuthEvent(ctx, store.AuthProbeOK, "after login", now); err != nil {
		m.log.Error("could not record probe_ok", "error", err)
	}
	// Refresh the account details, then unblock the executor and fire
	// auth.restored.
	if _, err := m.Probe(ctx); err != nil {
		m.log.Warn("probe after login failed", "error", err)
	}
	m.transitionToOK(ctx)
	m.log.Info("login completed", "attempt", a.id)
	return m.Status(ctx), nil
}

// awaitResult waits for the CLI to confirm or fail.
func (a *loginAttempt) awaitResult(ctx context.Context) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(60 * time.Second)
	for {
		out := a.seen()
		if claudecli.LoginSucceeded(out) {
			return nil
		}
		select {
		case <-a.done:
			// The process exited. Success may have been printed just before.
			if claudecli.LoginSucceeded(a.seen()) {
				return nil
			}
			return fmt.Errorf("login failed: %s", lastMeaningfulLine(a.seen()))
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("timed out waiting for login confirmation")
		case <-ticker.C:
		}
	}
}

// lastMeaningfulLine picks the most likely error text out of PTY output.
func lastMeaningfulLine(out string) string {
	lines := strings.Split(claudecli.StripANSI(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(strings.ReplaceAll(lines[i], "\r", ""))
		if l == "" || strings.HasPrefix(l, "Script ") {
			continue
		}
		// The prompt is not an error; skip past it to whatever followed.
		if strings.Contains(l, claudecli.LoginPromptMarker) {
			if after := strings.TrimSpace(strings.SplitN(l, ">", 2)[len(strings.SplitN(l, ">", 2))-1]); after != "" {
				return after
			}
			continue
		}
		return l
	}
	return "no output"
}

func (m *Manager) recordLoginFailed(ctx context.Context, a *loginAttempt, cause error) {
	if err := m.st.RecordAuthEvent(ctx, store.AuthLoginFailed,
		a.id+": "+firstLine(cause.Error()), m.now()); err != nil {
		m.log.Error("could not record login_failed", "error", err)
	}
}

// CancelLogin abandons an attempt, freeing the claude lock.
func (m *Manager) CancelLogin(ctx context.Context, id string) error {
	m.mu.Lock()
	a := m.attempt
	m.mu.Unlock()
	if a == nil || a.id != id {
		return ErrNoAttempt
	}
	m.finishAttempt(a, errors.New("cancelled"))
	return nil
}

// finishAttempt tears down an attempt exactly once, always releasing the lock.
func (m *Manager) finishAttempt(a *loginAttempt, cause error) {
	a.mu.Lock()
	if a.finished {
		a.mu.Unlock()
		return
	}
	a.finished = true
	a.err = cause
	a.mu.Unlock()

	if a.cancel != nil {
		a.cancel()
	}
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
	}
	if a.tty != nil {
		_ = a.tty.Close()
	}
	if a.release != nil {
		a.release()
	}

	m.mu.Lock()
	if m.attempt == a {
		m.attempt = nil
	}
	m.mu.Unlock()
}

// abandonStale clears an attempt whose TTL has passed.
func (m *Manager) abandonStale() {
	m.mu.Lock()
	a := m.attempt
	m.mu.Unlock()
	if a != nil && a.expired(m.now()) {
		m.log.Warn("abandoning expired login attempt", "attempt", a.id)
		m.finishAttempt(a, ErrAttemptExpired)
	}
}
