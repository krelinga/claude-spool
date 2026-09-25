package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"time"

	"github.com/krelinga/claude-spool-be/internal/config"
	"github.com/krelinga/claude-spool-be/internal/store"
)

// Reloader watches queues.yaml and swaps the live definitions when it changes.
//
// Queues are infrastructure-as-code (§3.3), so editing the file is how they
// change; hot reload means that does not need a restart. A job already queued
// runs under the *new* config, which is simpler and fine for a personal tool —
// the config hash recorded on each job is what keeps history honest.
type Reloader struct {
	path    string
	current func() *config.QueueSet
	swap    func(*config.QueueSet)
	onFail  func(queue string, jobs []string)
	log     logger
	st      *store.Store
	now     func() time.Time

	lastHash string
}

// PollInterval is how often the file is checked. A personal tool does not need
// inotify, and polling cannot miss an edit the way a watch can miss a rename.
const PollInterval = 10 * time.Second

func NewReloader(path string, current func() *config.QueueSet, swap func(*config.QueueSet),
	st *store.Store, log logger) *Reloader {
	r := &Reloader{path: path, current: current, swap: swap, st: st, log: log, now: time.Now}
	if b, err := os.ReadFile(path); err == nil {
		r.lastHash = hashBytes(b)
	}
	return r
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Run polls until ctx is cancelled.
func (r *Reloader) Run(ctx context.Context) error {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.ReloadIfChanged(ctx)
		}
	}
}

// ReloadIfChanged reparses the file when its contents have changed. A file that
// fails to parse is logged and ignored: the running config keeps working rather
// than the service losing its queues over a typo.
func (r *Reloader) ReloadIfChanged(ctx context.Context) bool {
	b, err := os.ReadFile(r.path)
	if err != nil {
		r.log.Error("could not read queues file", "path", r.path, "error", err)
		return false
	}
	h := hashBytes(b)
	if h == r.lastHash {
		return false
	}

	next, err := config.ParseQueues(b)
	if err != nil {
		// Do not adopt a broken file, and do not spam: the hash is recorded so
		// the same broken content is complained about once.
		r.lastHash = h
		r.log.Error("queues file is invalid; keeping the previous definitions", "error", err)
		return false
	}

	prev := r.current()
	r.lastHash = h
	r.swap(next)
	r.log.Info("queues reloaded", "queues", next.Names())

	// A queue that has disappeared must not leave work queued forever (§3.3).
	for _, name := range prev.Names() {
		if _, still := next.Get(name); still {
			continue
		}
		failed, err := r.st.FailQueued(ctx, name, store.ErrKindQueueRemoved,
			"queue was removed from queues.yaml", r.now())
		if err != nil {
			r.log.Error("could not fail jobs for a removed queue", "queue", name, "error", err)
			continue
		}
		if len(failed) > 0 {
			r.log.Warn("failed queued jobs for a removed queue", "queue", name, "jobs", len(failed))
		}
		if r.onFail != nil {
			r.onFail(name, failed)
		}
	}
	return true
}

// OnQueueRemoved registers a callback for jobs failed by a queue disappearing,
// so they can be notified.
func (r *Reloader) OnQueueRemoved(f func(queue string, jobs []string)) { r.onFail = f }
