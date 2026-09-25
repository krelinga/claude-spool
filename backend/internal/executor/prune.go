package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/krelinga/claude-spool/backend/internal/config"
	"github.com/krelinga/claude-spool/backend/internal/store"
)

// PruneInterval is how often retention is enforced. Retention windows are days,
// so hourly is plenty.
const PruneInterval = time.Hour

// Pruner deletes transcripts that have outlived their queue's retention (§3.6).
//
// Only transcripts are removed. The job rows are the history the API serves and
// stay put: at tens of jobs a day they cost almost nothing, whereas a transcript
// is the bulky part.
type Pruner struct {
	cfg    *config.Config
	st     *store.Store
	queues func() *config.QueueSet
	log    logger
	now    func() time.Time
}

func NewPruner(cfg *config.Config, st *store.Store, queues func() *config.QueueSet, log logger) *Pruner {
	return &Pruner{cfg: cfg, st: st, queues: queues, log: log, now: time.Now}
}

func (p *Pruner) Run(ctx context.Context) error {
	// Once at startup, so a long downtime does not leave stale transcripts
	// around until the first tick.
	if n, err := p.Prune(ctx); err != nil {
		p.log.Error("transcript prune failed", "error", err)
	} else if n > 0 {
		p.log.Info("pruned transcripts", "count", n)
	}

	ticker := time.NewTicker(PruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if n, err := p.Prune(ctx); err != nil {
				p.log.Error("transcript prune failed", "error", err)
			} else if n > 0 {
				p.log.Info("pruned transcripts", "count", n)
			}
		}
	}
}

// Prune removes expired transcripts and reports how many went.
func (p *Pruner) Prune(ctx context.Context) (int, error) {
	qs := p.queues()
	now := p.now()
	var removed int

	for _, name := range qs.Names() {
		q, _ := qs.Get(name)
		retention := q.Retention.Duration()
		if retention <= 0 {
			continue
		}
		ids, err := p.st.PrunableTranscripts(ctx, name, now.Add(-retention))
		if err != nil {
			return removed, err
		}
		for _, id := range ids {
			path := filepath.Join(p.cfg.TranscriptDir(), id+".jsonl")
			if err := os.Remove(path); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					p.log.Warn("could not remove transcript", "job", id, "error", err)
				}
				continue
			}
			removed++
		}
	}
	return removed, nil
}

// SweepWorkDirs removes work and run directories left behind by a crash. A
// fresh empty working directory per job is an invariant (§3.1), so leftovers
// from a previous process must not be inherited.
func (p *Pruner) SweepWorkDirs() {
	for _, dir := range []string{p.cfg.WorkDir(), p.cfg.RunDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // not created yet
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			if err := os.RemoveAll(path); err != nil {
				p.log.Warn("could not remove leftover directory", "path", path, "error", err)
				continue
			}
			p.log.Info("removed leftover directory from a previous run", "path", path)
		}
	}
}
