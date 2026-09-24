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
	"sync"
	"syscall"
	"time"

	"github.com/krelinga/claude-spool-be/internal/claudecli"
	"github.com/krelinga/claude-spool-be/internal/config"
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

	// A missing skill or connector is a config or credential-mode problem, not
	// a one-off. Pause the queue so it stops burning jobs (§3.4).
	if res.ErrorKind == store.ErrKindCapabilityMissing {
		if err := e.st.SetQueuePaused(ctx, job.Queue, true, "capability_missing"); err != nil {
			log.Error("could not auto-pause queue", "error", err)
		} else {
			log.Warn("queue auto-paused", "reason", res.ErrorMessage)
		}
	}

	e.finish(ctx, job, res)
}

func (e *Executor) finish(ctx context.Context, job *store.Job, res store.Result) {
	// The parent context may already be cancelled by shutdown; the result still
	// has to land, or the job would look interrupted when it actually finished.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := e.st.Finish(writeCtx, job.ID, res, e.now()); err != nil {
		e.log.Error("could not record job result", "job", job.ID, "error", err)
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
	if err := e.st.SetExecutorState(ctx, state, reason, resetAt); err != nil {
		log.Error("could not set executor state", "error", err)
		return
	}
	log.Warn("executor blocked", "state", state, "reason", reason, "until", resetAt)
}

type logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	Info(msg string, args ...any)
}

// execute prepares the run directory, starts `claude -p`, and streams it.
func (e *Executor) execute(ctx context.Context, job *store.Job, q *config.Queue, log logger) (claudecli.Run, error) {
	var run claudecli.Run

	prompt := q.Render(q.Prompt, job.Input, job.Args)
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
	schemaFile := filepath.Join(runDir, "outcome-schema.json")
	if err := os.WriteFile(schemaFile, schema, 0o600); err != nil {
		return run, fmt.Errorf("write outcome schema: %w", err)
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
		JSONSchemaFile:   schemaFile,
		AllowedTools:     q.AllowedTools,
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
	var missing []string
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
					if m := collector.MissingCapabilities(q.Requires); len(m) > 0 {
						missing = m
						log.Warn("required capabilities missing; stopping run", "missing", m)
						stop(cmd)
					}
				})
			}
			return nil
		})
	}()

	timedOut, waitErr := e.wait(cmd, q.Timeout.Duration(), runCtx)
	streamErr := <-streamDone
	if streamErr != nil {
		log.Error("transcript stream failed", "error", streamErr)
	}

	run = claudecli.Run{
		Collector: collector,
		TimedOut:  timedOut,
		Missing:   missing,
		ExitErr:   waitErr,
		Stderr:    stderr.String(),
	}
	return run, nil
}

// wait waits for the process, enforcing the queue's wall-clock timeout with
// the escalation from §3.4: SIGINT, a grace period, then SIGTERM.
func (e *Executor) wait(cmd *exec.Cmd, timeout time.Duration, ctx context.Context) (timedOut bool, err error) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		return false, err
	case <-timer.C:
		timedOut = true
	case <-ctx.Done():
		// Shutdown or an explicit cancel.
	}

	signalSequence(cmd, done)
	select {
	case err = <-done:
	case <-time.After(time.Second):
		err = errors.New("claude did not exit after SIGKILL")
	}
	return timedOut, err
}

// signalSequence escalates until the process is gone. SIGKILL is a backstop
// beyond the design's sequence: a wedged process holds the single executor
// slot, which would block every queue.
func signalSequence(cmd *exec.Cmd, done <-chan error) {
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
		case <-done:
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
