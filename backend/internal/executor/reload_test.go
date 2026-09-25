package executor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/claude-spool-be/backend/internal/config"
	"github.com/krelinga/claude-spool-be/backend/internal/store"
)

const reloadQueuesA = `
queues:
  media:
    prompt: "/m {{input}}"
    tools: [Skill]
    retention: 180d
  adhoc:
    prompt: "{{input}}"
    tools: [Skill]
    retention: 30d
`

const reloadQueuesB = `
queues:
  media:
    prompt: "/m v2 {{input}}"
    tools: [Skill]
    retention: 180d
`

func reloadHarness(t *testing.T, initial string) (*Reloader, *store.Store, string, func() *config.QueueSet) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "queues.yaml")
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	qs, err := config.ParseQueues([]byte(initial))
	if err != nil {
		t.Fatal(err)
	}
	var cur atomic.Pointer[config.QueueSet]
	cur.Store(qs)

	st, err := store.Open(filepath.Join(dir, "spool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	get := func() *config.QueueSet { return cur.Load() }
	return NewReloader(path, get, cur.Store, st, log), st, path, get
}

func TestReloadPicksUpChanges(t *testing.T) {
	r, _, path, get := reloadHarness(t, reloadQueuesA)
	before, _ := get().Get("media")

	if r.ReloadIfChanged(context.Background()) {
		t.Error("reloaded without a change")
	}
	if err := os.WriteFile(path, []byte(reloadQueuesB), 0o644); err != nil {
		t.Fatal(err)
	}
	if !r.ReloadIfChanged(context.Background()) {
		t.Fatal("did not reload after the file changed")
	}

	after, ok := get().Get("media")
	if !ok {
		t.Fatal("media disappeared")
	}
	if after.Prompt == before.Prompt {
		t.Error("the new prompt was not adopted")
	}
	// The config hash must change with it, or job history would misattribute.
	if after.ConfigHash == before.ConfigHash {
		t.Error("config hash unchanged after a prompt edit")
	}
	if _, gone := get().Get("adhoc"); gone {
		t.Error("adhoc should have been removed")
	}
}

// A queue that disappears must not leave work queued forever (§3.3).
func TestReloadFailsJobsForRemovedQueue(t *testing.T) {
	r, st, path, _ := reloadHarness(t, reloadQueuesA)
	ctx := context.Background()

	for _, id := range []string{"01A", "01B"} {
		st.Insert(ctx, &store.Job{ID: id, Queue: "adhoc", QueueConfigHash: "h",
			Status: store.StatusQueued, SubmittedBy: "t", CreatedAt: time.Now().UTC()})
	}
	st.Insert(ctx, &store.Job{ID: "01M", Queue: "media", QueueConfigHash: "h",
		Status: store.StatusQueued, SubmittedBy: "t", CreatedAt: time.Now().UTC()})

	var notified []string
	r.OnQueueRemoved(func(queue string, jobs []string) { notified = append(notified, jobs...) })

	os.WriteFile(path, []byte(reloadQueuesB), 0o644)
	if !r.ReloadIfChanged(ctx) {
		t.Fatal("did not reload")
	}

	for _, id := range []string{"01A", "01B"} {
		j, _ := st.Get(ctx, id)
		if j.Status != store.StatusFailed || j.ErrorKind != store.ErrKindQueueRemoved {
			t.Errorf("job %s = %v/%v", id, j.Status, j.ErrorKind)
		}
	}
	if len(notified) != 2 {
		t.Errorf("notified %v, want both jobs", notified)
	}
	// The surviving queue's work is untouched.
	if j, _ := st.Get(ctx, "01M"); j.Status != store.StatusQueued {
		t.Errorf("media job disturbed: %v", j.Status)
	}
}

// A typo must not cost the service its queues.
func TestReloadKeepsWorkingConfigOnParseError(t *testing.T) {
	r, _, path, get := reloadHarness(t, reloadQueuesA)
	if err := os.WriteFile(path, []byte("queues:\n  broken:\n    nonsense: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r.ReloadIfChanged(context.Background()) {
		t.Error("adopted an invalid file")
	}
	if _, ok := get().Get("media"); !ok {
		t.Error("the previous definitions were lost")
	}
	if _, ok := get().Get("adhoc"); !ok {
		t.Error("the previous definitions were lost")
	}
	// It must not retry the same broken content on every tick.
	if r.ReloadIfChanged(context.Background()) {
		t.Error("re-reported the same broken file")
	}
}

func TestReloadMissingFileIsNotFatal(t *testing.T) {
	r, _, path, get := reloadHarness(t, reloadQueuesA)
	os.Remove(path)
	if r.ReloadIfChanged(context.Background()) {
		t.Error("reloaded from a missing file")
	}
	if _, ok := get().Get("media"); !ok {
		t.Error("definitions lost when the file went missing")
	}
}

// --- pruning ---

func TestPruneRespectsPerQueueRetention(t *testing.T) {
	dir := t.TempDir()
	qs, err := config.ParseQueues([]byte(reloadQueuesA))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	if err := os.MkdirAll(cfg.TranscriptDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	// adhoc keeps 30d, media keeps 180d.
	cases := []struct {
		id       string
		queue    string
		age      time.Duration
		wantKept bool
	}{
		{"01OLDADHOC", "adhoc", 60 * 24 * time.Hour, false},
		{"01NEWADHOC", "adhoc", 10 * 24 * time.Hour, true},
		{"01OLDMEDIA", "media", 60 * 24 * time.Hour, true}, // within 180d
		{"01ANCIENTMEDIA", "media", 200 * 24 * time.Hour, false},
	}
	for _, c := range cases {
		st.Insert(ctx, &store.Job{ID: c.id, Queue: c.queue, QueueConfigHash: "h",
			Status: store.StatusQueued, SubmittedBy: "t", CreatedAt: now.Add(-c.age)})
		st.Claim(ctx, c.queue, now.Add(-c.age))
		st.Finish(ctx, c.id, store.Result{Status: store.StatusSucceeded}, now.Add(-c.age))
		if err := os.WriteFile(filepath.Join(cfg.TranscriptDir(), c.id+".jsonl"),
			[]byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p := NewPruner(cfg, st, func() *config.QueueSet { return qs },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	removed, err := p.Prune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}
	for _, c := range cases {
		_, statErr := os.Stat(filepath.Join(cfg.TranscriptDir(), c.id+".jsonl"))
		exists := statErr == nil
		if exists != c.wantKept {
			t.Errorf("%s (%s, %v old): exists=%v, want %v", c.id, c.queue, c.age, exists, c.wantKept)
		}
		// The job row itself is history and always stays.
		if _, err := st.Get(ctx, c.id); err != nil {
			t.Errorf("job row for %s was deleted: %v", c.id, err)
		}
	}
}

// An unfinished job has no retention clock and must not be pruned.
func TestPruneSkipsUnfinishedJobs(t *testing.T) {
	dir := t.TempDir()
	qs, _ := config.ParseQueues([]byte(reloadQueuesA))
	cfg := &config.Config{DataDir: dir}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	os.MkdirAll(cfg.TranscriptDir(), 0o755)

	st.Insert(ctx, &store.Job{ID: "01Q", Queue: "adhoc", QueueConfigHash: "h",
		Status: store.StatusQueued, SubmittedBy: "t",
		CreatedAt: time.Now().Add(-100 * 24 * time.Hour).UTC()})
	os.WriteFile(filepath.Join(cfg.TranscriptDir(), "01Q.jsonl"), []byte("{}\n"), 0o600)

	p := NewPruner(cfg, st, func() *config.QueueSet { return qs },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if removed, err := p.Prune(ctx); err != nil || removed != 0 {
		t.Errorf("Prune = %d, %v; want 0", removed, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.TranscriptDir(), "01Q.jsonl")); err != nil {
		t.Error("pruned a transcript for a job that has not finished")
	}
}

func TestSweepWorkDirs(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	for _, d := range []string{
		filepath.Join(cfg.WorkDir(), "01LEFTOVER"),
		filepath.Join(cfg.RunDir(), "01LEFTOVER"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "stray.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	qs, _ := config.ParseQueues([]byte(reloadQueuesA))
	p := NewPruner(cfg, nil, func() *config.QueueSet { return qs },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.SweepWorkDirs()

	for _, d := range []string{cfg.WorkDir(), cfg.RunDir()} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("%s still holds %d entries", d, len(entries))
		}
	}
}
