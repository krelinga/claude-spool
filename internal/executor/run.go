package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/krelinga/claude-spool-be/internal/claudecli"
	"github.com/krelinga/claude-spool-be/internal/config"
	"github.com/krelinga/claude-spool-be/internal/event"
	"github.com/krelinga/claude-spool-be/internal/store"
)

// runJob executes one job to completion and records its outcome.
func (e *Executor) runJob(ctx context.Context, job *store.Job) {
	log := e.log.With("job", job.ID, "queue", job.Queue)

	q, ok := e.queues().Get(job.Queue)
	if !ok {
		e.finish(ctx, job, store.Result{
			Status:       store.StatusFailed,
			ErrorKind:    store.ErrKindQueueRemoved,
			ErrorMessage: "queue no longer exists in queues.yaml",
		})
		return
	}

	run, err := e.execute(ctx, job, q, log)
	if err != nil {
		// A setup failure never reached Claude, so nothing has side effects and
		// the job can simply fail.
		log.Error("could not start job", "error", err)
		e.finish(ctx, job, store.Result{
			Status:       store.StatusFailed,
			ErrorKind:    store.ErrKindCLI,
			ErrorMessage: err.Error(),
		})
		return
	}

	cl := claudecli.Classify(run)
	res := cl.Result
	log.Info("job finished",
		"status", res.Status, "error_kind", res.ErrorKind,
		"tool_calls", res.ToolCalls, "turns", res.NumTurns, "cost_usd", res.CostUSD)

	// Auth and usage failures are shared-state problems: they block every queue
	// at once, and the job itself may be safe to retry (§3.3, §3.5).
	if res.ErrorKind.Blocking() {
		e.block(ctx, res.ErrorKind, res.ErrorMessage, cl.ResetAt, log)
		if res.ToolCalls == 0 {
			if err := e.st.Requeue(ctx, job.ID); err == nil {
				log.Info("requeued job that made no tool calls", "error_kind", res.ErrorKind)
				return
			} else if !errors.Is(err, store.ErrNotFound) {
				log.Error("requeue failed", "error", err)
			}
		} else {
			log.Warn("not requeuing: job already made tool calls", "tool_calls", res.ToolCalls)
		}
	}

	// A connector caught mid-connect is a race, not a config problem. Retry it
	// a few times instead of pausing the queue or failing outright.
	if res.ErrorKind == store.ErrKindCapabilityPending && res.ToolCalls == 0 &&
		job.RunAttempts < maxCapabilityRetries {
		if err := e.st.Requeue(ctx, job.ID); err == nil {
			log.Info("requeued job whose connectors were still connecting",
				"attempt", job.RunAttempts)
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			log.Error("requeue failed", "error", err)
		}
	}

	// A missing skill or connector is a config or credential-mode problem, not
	// a one-off. Pause the queue so it stops burning jobs (§3.4).
	if res.ErrorKind == store.ErrKindCapabilityMissing {
		if err := e.st.SetQueuePaused(ctx, job.Queue, true, "capability_missing"); err != nil {
			log.Error("could not auto-pause queue", "error", err)
		} else {
			log.Warn("queue auto-paused", "reason", res.ErrorMessage)
			env := e.notify.Envelope(event.QueueAutoPaused)
			env.Queue = job.Queue
			env.QueueRef = &event.QueueInfo{
				Name: job.Queue, Paused: true, Reason: res.ErrorMessage,
			}
			if depths, err := e.st.Depths(ctx); err == nil {
				env.QueueRef.Depth = depths[job.Queue]
			}
			if err := e.st.EnqueueDeliveries(ctx, e.notify.Deliveries(env), e.now()); err != nil {
				log.Error("could not enqueue queue.auto_paused", "error", err)
			} else {
				e.wakeSender()
			}
		}
	}

	e.finish(ctx, job, res)
}

func (e *Executor) finish(ctx context.Context, job *store.Job, res store.Result) {
	// The parent context may already be cancelled by shutdown; the result still
	// has to land, or the job would look interrupted when it actually finished.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	// The notification is built from the job as it will be once written, and
	// committed in the same transaction: a finished job always has its event
	// queued, and a rolled-back result never emits one (§3.6).
	finished := *job
	finished.Status = res.Status
	finished.Summary = res.Summary
	finished.ErrorKind = res.ErrorKind
	finished.ErrorMessage = res.ErrorMessage
	finished.Outcome = res.Outcome
	finished.CostUSD = res.CostUSD
	finished.NumTurns = res.NumTurns
	now := e.now()
	finished.FinishedAt = &now

	var deliveries []store.Delivery
	if env, ok := e.notify.JobEnvelope(&finished); ok {
		deliveries = e.notify.Deliveries(env)
	}
	if err := e.st.FinishWithDeliveries(writeCtx, job.ID, res, deliveries, now); err != nil {
		e.log.Error("could not record job result", "job", job.ID, "error", err)
		return
	}
	if len(deliveries) > 0 {
		e.wakeSender()
	}
}

func (e *Executor) block(ctx context.Context, kind store.ErrorKind, reason string, resetAt *time.Time, log logger) {
	state := store.ExecBlockedAuth
	if kind == store.ErrKindUsageLimit {
		state = store.ExecBlockedUsage
		if resetAt == nil {
			// No reset time given: back off for an hour rather than hammering
			// the API or stalling forever.
			t := e.now().Add(time.Hour)
			resetAt = &t
		}
	} else {
		// An auth block waits for a human; a timer would only mask it.
		resetAt = nil
	}

	// Read the state we are leaving, so the notification fires once per
	// incident rather than once per failed job (§3.2).
	prev, err := e.st.ExecutorState(ctx)
	if err != nil {
		log.Error("could not read executor state", "error", err)
	}
	if err := e.st.SetExecutorState(ctx, state, reason, resetAt); err != nil {
		log.Error("could not set executor state", "error", err)
		return
	}
	log.Warn("executor blocked", "state", state, "reason", reason, "until", resetAt)

	if prev.State == state {
		return
	}
	e.emitBlocked(ctx, state, reason, resetAt, log)
}

func (e *Executor) emitBlocked(ctx context.Context, state store.ExecutorState, reason string, until *time.Time, log logger) {
	depth := 0
	if depths, err := e.st.Depths(ctx); err == nil {
		for _, n := range depths {
			depth += n
		}
	}
	var env event.Envelope
	if state == store.ExecBlockedAuth {
		env = e.notify.Envelope(event.AuthRequired)
		since := e.now().UTC()
		env.Auth = &event.AuthInfo{
			State: "expired", Since: &since, QueuedJobs: depth,
			LoginURL: e.loginURL(),
		}
	} else {
		env = e.notify.Envelope(event.ExecutorBlockedUsage)
		env.Executor = &event.ExecutorInfo{
			State: string(state), Reason: reason, BlockedUntil: until, QueuedJobs: depth,
		}
	}
	if err := e.st.EnqueueDeliveries(ctx, e.notify.Deliveries(env), e.now()); err != nil {
		log.Error("could not enqueue service notification", "event", env.Event, "error", err)
		return
	}
	e.wakeSender()
}

// loginURL points at the re-login flow so the notification is actionable from
// a phone. The endpoint itself arrives with the auth manager (§7 step 3).
func (e *Executor) loginURL() string {
	if e.cfg.PublicURL == "" {
		return ""
	}
	return strings.TrimSuffix(e.cfg.PublicURL, "/") + "/v1/auth/login"
}

type logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	Info(msg string, args ...any)
}

// execute prepares the run directory, starts `claude -p`, and streams it.
func (e *Executor) execute(ctx context.Context, job *store.Job, q *config.Queue, log logger) (claudecli.Run, error) {
	var run claudecli.Run

	// Trimmed because a YAML block scalar ("prompt: |") carries a trailing
	// newline that would otherwise ride along into the prompt.
	prompt := strings.TrimSpace(q.Render(q.Prompt, job.Input, job.Args))
	if err := e.st.SetRenderedPrompt(ctx, job.ID, prompt); err != nil {
		return run, fmt.Errorf("record rendered prompt: %w", err)
	}

	// A fresh empty working directory, so no stray .claude/ or .mcp.json from
	// elsewhere on the filesystem reaches the run (§3.1).
	workDir := filepath.Join(e.cfg.WorkDir(), job.ID)
	runDir := filepath.Join(e.cfg.RunDir(), job.ID)
	for _, d := range []string{workDir, runDir, e.cfg.TranscriptDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return run, fmt.Errorf("create %s: %w", d, err)
		}
	}
	defer os.RemoveAll(workDir)
	defer os.RemoveAll(runDir)

	systemFile := filepath.Join(runDir, "system.md")
	if err := os.WriteFile(systemFile, []byte(claudecli.BuildSystemPrompt(q)), 0o600); err != nil {
		return run, fmt.Errorf("write system prompt: %w", err)
	}
	schema, err := claudecli.BuildOutcomeSchema(q)
	if err != nil {
		return run, err
	}

	transcript, err := os.Create(filepath.Join(e.cfg.TranscriptDir(), job.ID+".jsonl"))
	if err != nil {
		return run, fmt.Errorf("create transcript: %w", err)
	}
	defer transcript.Close()

	model := job.Model
	if model == "" {
		model = q.Model
	}
	if model == "" {
		model = e.cfg.Claude.DefaultModel
	}

	inv := claudecli.Invocation{
		Binary:           e.cfg.Claude.Binary,
		Prompt:           prompt,
		WorkDir:          workDir,
		ConfigDir:        e.cfg.Claude.ConfigDir,
		SystemPromptFile: systemFile,
		JSONSchema:       string(schema),
		Tools:            q.Tools,
		AllowedTools:     q.AllowedTools,
		DisallowedTools:  q.DisallowedTools,
		MaxTurns:         q.MaxTurns,
		Model:            model,
		ResumeSession:    job.ResumeSession,
		SyncSkills:       e.cfg.Claude.SyncSkills,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.setCurrent(job, cancel)
	defer e.clearCurrent()

	cmd := inv.Command(runCtx)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return run, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return run, fmt.Errorf("start claude: %w", err)
	}
	log.Info("claude started", "pid", cmd.Process.Pid, "model", model, "timeout", q.Timeout.String())

	collector := claudecli.NewCollector()
	var caps claudecli.Capabilities
	var capOnce sync.Once

	streamDone := make(chan error, 1)
	go func() {
		streamDone <- claudecli.ScanEvents(stdout, transcript, func(ev *claudecli.Event) error {
			collector.Observe(ev)
			// Check declared requirements as soon as init arrives, and stop the
			// run immediately rather than letting Claude improvise without its
			// tools (§3.2).
			if ev.IsInit() {
				capOnce.Do(func() {
					caps = collector.CheckCapabilities(q.Requires)
					if !caps.OK() {
						log.Warn("declared capabilities unavailable; stopping run",
							"missing", caps.Missing, "pending", caps.Pending)
						stop(cmd)
					}
				})
			}
			return nil
		})
	}()

	// Stop the run on timeout or cancellation. exited closes once Wait has
	// reaped the process, which tells the escalation it can stop.
	exited := make(chan struct{})
	var timedOut atomic.Bool
	go func() {
		timer := time.NewTimer(q.Timeout.Duration())
		defer timer.Stop()
		select {
		case <-exited:
			return
		case <-runCtx.Done():
		case <-timer.C:
			timedOut.Store(true)
		}
		signalSequence(cmd, exited)
	}()

	// Drain stdout to EOF *before* calling Wait. Wait closes the pipe as soon
	// as the process exits, so waiting first truncates the stream and can lose
	// the result line the whole classification depends on.
	if streamErr := <-streamDone; streamErr != nil {
		log.Error("transcript stream failed", "error", streamErr)
	}
	waitErr := cmd.Wait()
	close(exited)

	run = claudecli.Run{
		Collector: collector,
		TimedOut:  timedOut.Load(),
		Caps:      caps,
		ExitErr:   waitErr,
		Stderr:    stderr.String(),
	}
	return run, nil
}

// signalSequence escalates until the process is gone. SIGKILL is a backstop
// beyond the design's sequence: a wedged process holds the single executor
// slot, which would block every queue.
func signalSequence(cmd *exec.Cmd, exited <-chan struct{}) {
	for _, step := range []struct {
		sig   syscall.Signal
		grace time.Duration
	}{
		{syscall.SIGINT, sigintGrace},
		{syscall.SIGTERM, sigtermGrace},
		{syscall.SIGKILL, 0},
	} {
		signal(cmd, step.sig)
		if step.grace == 0 {
			return
		}
		select {
		case <-exited:
			return
		case <-time.After(step.grace):
		}
	}
}

// signal sends to the whole process group, so tools the CLI spawned die too.
func signal(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, sig)
		return
	}
	_ = cmd.Process.Signal(sig)
}

// stop begins the shutdown sequence for a run that must not continue.
func stop(cmd *exec.Cmd) { signal(cmd, syscall.SIGINT) }

func (e *Executor) setCurrent(job *store.Job, cancel context.CancelFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.current = &runningJob{id: job.ID, queue: job.Queue, cancel: cancel}
}

func (e *Executor) clearCurrent() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.current = nil
}

// tailBuffer keeps only the last stderrLimit bytes: enough to classify and to
// put in an error message, bounded so a chatty failure cannot exhaust memory.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > stderrLimit {
		b := t.buf.Bytes()
		trimmed := append([]byte{}, b[t.buf.Len()-stderrLimit:]...)
		t.buf.Reset()
		t.buf.Write(trimmed)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

var _ io.Writer = (*tailBuffer)(nil)
