// Package executor owns the one thing Spool does serially: running Claude.
//
// There is exactly one executor for the whole service, and it runs one
// `claude` process at a time across every queue. That is not a performance
// choice — it is credential safety. One login means one holder of the refresh
// token, so nothing inside the container races to rotate it (design §3.2).
package executor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/krelinga/claude-spool-be/backend/internal/claudecli"
	"github.com/krelinga/claude-spool-be/backend/internal/event"

	"github.com/krelinga/claude-spool-be/backend/internal/config"
	"github.com/krelinga/claude-spool-be/backend/internal/sched"
	"github.com/krelinga/claude-spool-be/backend/internal/store"
)

// Grace periods for stopping a run that has overrun its timeout (§3.4).
// SIGKILL is not in the design's SIGINT -> SIGTERM sequence; it is a backstop,
// because a wedged process holds the only executor slot and would otherwise
// block every queue indefinitely.
const (
	sigintGrace  = 10 * time.Second
	sigtermGrace = 5 * time.Second
	// idlePoll bounds how long the loop sleeps when it has nothing to do. Jobs
	// normally arrive via Wake; this is the backstop.
	idlePoll = 30 * time.Second
	// stderrLimit caps how much CLI stderr is kept for classification.
	stderrLimit = 8 << 10
	// maxCapabilityRetries bounds retries for a connector that keeps reporting
	// "pending", so a genuinely broken connector still reaches a human.
	maxCapabilityRetries = 3
)

// AuthObserver is told about outcomes that bear on the credential. The auth
// manager implements it, so the executor does not duplicate the state machine:
// one place decides what an auth failure means and fires one auth.required per
// incident (§3.2).
type AuthObserver interface {
	NoteJobRan(at time.Time)
	ObserveAuthFailure(ctx context.Context, detail string)
}

type Executor struct {
	cfg    *config.Config
	st     *store.Store
	log    *slog.Logger
	queues func() *config.QueueSet
	notify *event.Notifier
	// lock serialises every claude subprocess in the process, jobs included.
	lock *claudecli.Lock
	auth AuthObserver
	// wakeSender nudges the webhook sender once notifications are committed.
	wakeSender func()
	now        func() time.Time

	wake chan struct{}

	mu      sync.Mutex
	current *runningJob
}

type runningJob struct {
	id     string
	queue  string
	cancel context.CancelFunc
	// cancelled distinguishes an operator stopping the job from a timeout, so
	// the outcome reads cancelled rather than failed.
	cancelled bool
}

type Option func(*Executor)

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(e *Executor) { e.now = now }
}

// WithSenderWake registers a callback to nudge the webhook sender as soon as
// deliveries are committed, instead of waiting for its poll.
func WithSenderWake(f func()) Option {
	return func(e *Executor) { e.wakeSender = f }
}

// WithLock shares the process-wide claude lock. Without it the executor makes
// its own, which is correct only when nothing else runs claude.
func WithLock(l *claudecli.Lock) Option {
	return func(e *Executor) { e.lock = l }
}

func New(cfg *config.Config, st *store.Store, queues func() *config.QueueSet, notify *event.Notifier, log *slog.Logger, opts ...Option) *Executor {
	e := &Executor{
		cfg: cfg, st: st, log: log, queues: queues, notify: notify,
		wakeSender: func() {},
		now:        time.Now,
		wake:       make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(e)
	}
	if e.lock == nil {
		e.lock = claudecli.NewLock()
	}
	return e
}

// SetAuthObserver registers the auth manager. It is set after construction
// because the manager needs the executor's Wake to unblock queued work.
func (e *Executor) SetAuthObserver(a AuthObserver) { e.auth = a }

// Lock exposes the shared claude lock.
func (e *Executor) Lock() *claudecli.Lock { return e.lock }

// Wake nudges the loop after a submission or a resume. It never blocks.
func (e *Executor) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Running reports the job currently executing, if any.
func (e *Executor) Running() (id, queue string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return "", "", false
	}
	return e.current.id, e.current.queue, true
}

// Cancel stops the named job if it is the one currently running, reporting
// whether it took effect. The run's own finish path records the outcome, so a
// cancelled job is never left marked running.
func (e *Executor) Cancel(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil || e.current.id != id {
		return false
	}
	e.current.cancelled = true
	e.current.cancel()
	return true
}

// wasCancelled reports whether the in-flight job was cancelled by an operator.
func (e *Executor) wasCancelled(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current != nil && e.current.id == id && e.current.cancelled
}

// Recover reconciles state left behind by a crash or restart. Jobs that were
// running become interrupted and wait for a human: at-most-once means never
// silently re-running work whose side effects may already have landed (§3.5).
func (e *Executor) Recover(ctx context.Context) error {
	ids, err := e.st.RecoverRunning(ctx, e.now())
	if err != nil {
		return err
	}
	for _, id := range ids {
		e.log.Warn("job interrupted by restart", "job", id)
		e.notifyJob(ctx, id)
	}
	// A queue that vanished from config while holding work fails loudly rather
	// than leaving jobs queued forever (§3.3).
	qs := e.queues()
	depths, err := e.st.Depths(ctx)
	if err != nil {
		return err
	}
	for queue := range depths {
		if _, ok := qs.Get(queue); ok {
			continue
		}
		failed, err := e.st.FailQueued(ctx, queue, store.ErrKindQueueRemoved,
			"queue no longer exists in queues.yaml", e.now())
		if err != nil {
			return err
		}
		e.log.Warn("failed jobs for removed queue", "queue", queue, "jobs", len(failed))
		for _, id := range failed {
			e.notifyJob(ctx, id)
		}
	}
	return nil
}

// NotifyJob is notifyJob for callers outside the package, used when a queue
// disappearing fails jobs during a reload.
func (e *Executor) NotifyJob(ctx context.Context, id string) { e.notifyJob(ctx, id) }

// notifyJob emits the event for a job whose terminal state was written outside
// the executor's own finish path (restart recovery, queue removal). It is a
// separate transaction from the status change, so a crash in between loses the
// notification but never the job.
func (e *Executor) notifyJob(ctx context.Context, id string) {
	j, err := e.st.Get(ctx, id)
	if err != nil {
		e.log.Error("could not load job to notify", "job", id, "error", err)
		return
	}
	env, ok := e.notify.JobEnvelope(j)
	if !ok {
		return
	}
	if err := e.st.EnqueueDeliveries(ctx, e.notify.Deliveries(env), e.now()); err != nil {
		e.log.Error("could not enqueue notification", "job", id, "error", err)
		return
	}
	e.wakeSender()
}

// Run is the scheduling loop. It returns when ctx is cancelled.
func (e *Executor) Run(ctx context.Context) error {
	e.log.Info("executor started")
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		wait, err := e.step(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			e.log.Error("executor step failed", "error", err)
			wait = idlePoll
		}
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-e.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// step runs at most one job. It returns how long to wait before looking again;
// zero means "there may be more work right now".
func (e *Executor) step(ctx context.Context) (time.Duration, error) {
	now := e.now()

	state, err := e.st.ExecutorState(ctx)
	if err != nil {
		return idlePoll, err
	}
	if !state.Runnable(now) {
		// A usage block lifts by itself at the reset time; an auth block waits
		// for a human, so it just idles until something wakes us.
		if state.State == store.ExecBlockedUsage && state.BlockedUntil != nil {
			if d := state.BlockedUntil.Sub(now); d > 0 {
				return min(d, idlePoll), nil
			}
			if err := e.st.SetExecutorState(ctx, store.ExecReady, "", nil); err != nil {
				return idlePoll, err
			}
			return 0, nil
		}
		return idlePoll, nil
	}

	queue, err := e.pick(ctx)
	if err != nil {
		return idlePoll, err
	}
	if queue == "" {
		return idlePoll, nil
	}

	job, err := e.st.Claim(ctx, queue, now)
	if errors.Is(err, store.ErrNotFound) {
		// The head job was cancelled between the depth read and the claim.
		return 0, nil
	}
	if err != nil {
		return idlePoll, err
	}

	e.runJob(ctx, job)
	return 0, nil
}

// pick chooses the next queue: weighted round-robin over queues that are
// unpaused and have queued work, FIFO within each (§3.3).
func (e *Executor) pick(ctx context.Context) (string, error) {
	depths, err := e.st.Depths(ctx)
	if err != nil {
		return "", err
	}
	if len(depths) == 0 {
		return "", nil
	}
	runtimes, err := e.st.AllQueueRuntime(ctx)
	if err != nil {
		return "", err
	}
	qs := e.queues()

	var candidates []sched.Candidate
	for name, depth := range depths {
		if depth == 0 {
			continue
		}
		q, ok := qs.Get(name)
		if !ok {
			// Removed from config; Recover fails these, and a reload will too.
			continue
		}
		if runtimes[name].Paused {
			continue
		}
		candidates = append(candidates, sched.Candidate{
			Name: name, Weight: q.Weight, Credit: runtimes[name].RRCredit,
		})
	}
	if len(candidates) == 0 {
		return "", nil
	}
	winner, credits := sched.Pick(candidates)
	if err := e.st.SetRRCredits(ctx, credits); err != nil {
		return "", err
	}
	return winner, nil
}
