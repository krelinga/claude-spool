package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newJob(id, queue string) *Job {
	return &Job{
		ID: id, Queue: queue, QueueConfigHash: "abc123", Status: StatusQueued,
		Input: "some input", SubmittedBy: "test", CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestInsertAndGet(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	j := newJob("01AAAA", "media")
	j.Args = map[string]any{"url": "https://example.com"}
	j.Labels = []string{"a", "b"}
	j.Model = "sonnet"
	if _, created, err := s.Insert(ctx, j); err != nil || !created {
		t.Fatalf("Insert = %v, %v", created, err)
	}

	got, err := s.Get(ctx, "01AAAA")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Queue != "media" || got.Status != StatusQueued || got.Input != "some input" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Args["url"] != "https://example.com" {
		t.Errorf("Args = %v", got.Args)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "a" {
		t.Errorf("Labels = %v", got.Labels)
	}
	if !got.CreatedAt.Equal(j.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, j.CreatedAt)
	}

	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

// Duplicate submissions must not create a second job: these prompts have side
// effects, and a duplicate Notion entry is worse than a failure (§3.5).
func TestIdempotency(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	a := newJob("01AAAA", "media")
	a.IdempotencyKey = "share-sheet-1"
	if _, created, err := s.Insert(ctx, a); err != nil || !created {
		t.Fatalf("first insert: %v %v", created, err)
	}

	b := newJob("01BBBB", "media")
	b.IdempotencyKey = "share-sheet-1"
	got, created, err := s.Insert(ctx, b)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if created {
		t.Error("duplicate idempotency key created a second job")
	}
	if got.ID != "01AAAA" {
		t.Errorf("returned job %s, want the original", got.ID)
	}

	// The same key in a different queue is a different job.
	c := newJob("01CCCC", "adhoc")
	c.IdempotencyKey = "share-sheet-1"
	if _, created, err := s.Insert(ctx, c); err != nil || !created {
		t.Errorf("same key in another queue should be accepted: %v %v", created, err)
	}
}

// Jobs without an idempotency key must not collide with each other.
func TestNullIdempotencyKeysDoNotCollide(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, id := range []string{"01A", "01B", "01C"} {
		if _, created, err := s.Insert(ctx, newJob(id, "media")); err != nil || !created {
			t.Fatalf("insert %s: %v %v", id, created, err)
		}
	}
}

func TestClaimIsFIFOWithinQueueAndRespectsPriority(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC()

	for _, id := range []string{"01A", "01B", "01C"} {
		s.Insert(ctx, newJob(id, "media"))
	}
	urgent := newJob("01Z", "media")
	urgent.Priority = 5
	s.Insert(ctx, urgent)

	// Priority wins; ULID order breaks ties, so FIFO holds within a priority.
	for _, want := range []string{"01Z", "01A", "01B", "01C"} {
		j, err := s.Claim(ctx, "media", now)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if j.ID != want {
			t.Fatalf("Claim = %s, want %s", j.ID, want)
		}
		if j.Status != StatusRunning || j.RunAttempts != 1 || j.StartedAt == nil {
			t.Errorf("claimed job not marked running: %+v", j)
		}
	}
	if _, err := s.Claim(ctx, "media", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("Claim on empty queue = %v, want ErrNotFound", err)
	}
}

func TestClaimIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Insert(ctx, newJob("01A", "media"))

	if _, err := s.Claim(ctx, "media", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A second claim must not hand out the same job.
	if _, err := s.Claim(ctx, "media", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Claim = %v, want ErrNotFound", err)
	}
}

func TestFinish(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Insert(ctx, newJob("01A", "media"))
	s.Claim(ctx, "media", time.Now())

	res := Result{
		Status: StatusSucceeded, Summary: "Added Dune",
		Outcome:   json.RawMessage(`{"status":"succeeded","notion_url":"https://x"}`),
		SessionID: "sess-1", NumTurns: 4, CostUSD: 0.12, DurationMS: 4200, ToolCalls: 3,
	}
	if err := s.Finish(ctx, "01A", res, time.Now()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	j, _ := s.Get(ctx, "01A")
	if j.Status != StatusSucceeded || j.Summary != "Added Dune" || j.ToolCalls != 3 {
		t.Errorf("finished job: %+v", j)
	}
	if j.FinishedAt == nil {
		t.Error("FinishedAt not set")
	}
	var outcome map[string]any
	if err := json.Unmarshal(j.Outcome, &outcome); err != nil {
		t.Fatalf("outcome not valid JSON: %v", err)
	}
	if outcome["notion_url"] != "https://x" {
		t.Errorf("outcome = %v", outcome)
	}

	// A non-terminal status is a programming error, not a stored state.
	if err := s.Finish(ctx, "01A", Result{Status: StatusRunning}, time.Now()); err == nil {
		t.Error("Finish accepted a non-terminal status")
	}
}

// A crash mid-run must leave the job for a human, never silently re-run it.
func TestRecoverRunningMarksInterrupted(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Insert(ctx, newJob("01A", "media"))
	s.Insert(ctx, newJob("01B", "media"))
	s.Claim(ctx, "media", time.Now())

	ids, err := s.RecoverRunning(ctx, time.Now())
	if err != nil {
		t.Fatalf("RecoverRunning: %v", err)
	}
	if len(ids) != 1 || ids[0] != "01A" {
		t.Fatalf("recovered %v, want [01A]", ids)
	}
	j, _ := s.Get(ctx, "01A")
	if j.Status != StatusInterrupted || j.ErrorKind != ErrKindInterrupted {
		t.Errorf("job not interrupted: %+v", j)
	}
	// The still-queued job is untouched.
	if b, _ := s.Get(ctx, "01B"); b.Status != StatusQueued {
		t.Errorf("queued job disturbed: %v", b.Status)
	}
}

func TestRequeue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Insert(ctx, newJob("01A", "media"))
	s.Claim(ctx, "media", time.Now())

	if err := s.Requeue(ctx, "01A"); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	j, _ := s.Get(ctx, "01A")
	if j.Status != StatusQueued || j.StartedAt != nil {
		t.Errorf("requeued job: %+v", j)
	}
	// Run attempts are kept, so a retry loop is visible in history.
	if j.RunAttempts != 1 {
		t.Errorf("RunAttempts = %d, want 1", j.RunAttempts)
	}
	// Requeueing a queued job is a no-op error, not a silent success.
	if err := s.Requeue(ctx, "01A"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Requeue(queued) = %v", err)
	}
}

func TestCancelOnlyQueued(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Insert(ctx, newJob("01A", "media"))
	if err := s.Cancel(ctx, "01A", time.Now()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if j, _ := s.Get(ctx, "01A"); j.Status != StatusCancelled {
		t.Errorf("status = %v", j.Status)
	}

	s.Insert(ctx, newJob("01B", "media"))
	s.Claim(ctx, "media", time.Now())
	if err := s.Cancel(ctx, "01B", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancelling a running job should be refused, got %v", err)
	}
}

func TestListFilters(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Insert(ctx, newJob("01A", "media"))
	s.Insert(ctx, newJob("01B", "adhoc"))
	s.Insert(ctx, newJob("01C", "media"))

	all, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != "01C" {
		t.Errorf("List returned %d jobs, newest-first expected: %v", len(all), all[0].ID)
	}
	media, _ := s.List(ctx, ListFilter{Queue: "media"})
	if len(media) != 2 {
		t.Errorf("queue filter returned %d", len(media))
	}
	page, _ := s.List(ctx, ListFilter{Limit: 1})
	if len(page) != 1 || page[0].ID != "01C" {
		t.Errorf("limit: %v", page)
	}
	next, _ := s.List(ctx, ListFilter{Cursor: page[0].ID, Limit: 1})
	if len(next) != 1 || next[0].ID != "01B" {
		t.Errorf("cursor paging: %v", next)
	}
}

func TestPosition(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, id := range []string{"01A", "01B", "01C"} {
		s.Insert(ctx, newJob(id, "media"))
	}
	c, _ := s.Get(ctx, "01C")
	n, err := s.Position(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Position = %d, want 2", n)
	}
}

func TestExecutorState(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now()

	e, err := s.ExecutorState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != ExecReady || !e.Runnable(now) {
		t.Errorf("initial state = %+v, want ready", e)
	}

	until := now.Add(time.Hour)
	if err := s.SetExecutorState(ctx, ExecBlockedUsage, "usage limit", &until); err != nil {
		t.Fatal(err)
	}
	e, _ = s.ExecutorState(ctx)
	if e.State != ExecBlockedUsage || e.Reason != "usage limit" || e.BlockedUntil == nil {
		t.Fatalf("blocked state = %+v", e)
	}
	if e.Runnable(now) {
		t.Error("usage-blocked executor should not be runnable before reset")
	}
	// A usage block lapses on its own once the reset time passes.
	if !e.Runnable(until.Add(time.Minute)) {
		t.Error("usage block should lapse after blocked_until")
	}

	// An auth block never lapses on its own: it needs a human to re-login.
	s.SetExecutorState(ctx, ExecBlockedAuth, "login expired", nil)
	e, _ = s.ExecutorState(ctx)
	if e.Runnable(now.Add(100 * time.Hour)) {
		t.Error("auth block must not lapse on a timer")
	}
}

func TestQueueRuntime(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	// An untouched queue reads as unpaused rather than erroring.
	r, err := s.QueueRuntime(ctx, "media")
	if err != nil || r.Paused {
		t.Fatalf("QueueRuntime(new) = %+v, %v", r, err)
	}

	if err := s.SetQueuePaused(ctx, "media", true, "manual"); err != nil {
		t.Fatal(err)
	}
	r, _ = s.QueueRuntime(ctx, "media")
	if !r.Paused || r.PausedReason != "manual" {
		t.Errorf("paused runtime = %+v", r)
	}

	// Pausing must not clobber round-robin credits, and vice versa.
	if err := s.SetRRCredits(ctx, map[string]float64{"media": 2.5}); err != nil {
		t.Fatal(err)
	}
	r, _ = s.QueueRuntime(ctx, "media")
	if r.RRCredit != 2.5 || !r.Paused {
		t.Errorf("after credit save = %+v", r)
	}
	s.SetQueuePaused(ctx, "media", false, "")
	r, _ = s.QueueRuntime(ctx, "media")
	if r.RRCredit != 2.5 || r.Paused {
		t.Errorf("after resume = %+v", r)
	}
}

func TestDepthsAndStats(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC()

	s.Insert(ctx, newJob("01A", "media"))
	s.Insert(ctx, newJob("01B", "media"))
	s.Insert(ctx, newJob("01C", "adhoc"))

	depths, err := s.Depths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depths["media"] != 2 || depths["adhoc"] != 1 {
		t.Errorf("Depths = %v", depths)
	}

	s.Claim(ctx, "media", now)
	s.Finish(ctx, "01A", Result{Status: StatusSucceeded, Summary: "ok"}, now)
	s.Claim(ctx, "media", now)
	s.Finish(ctx, "01B", Result{Status: StatusFailed, ErrorKind: ErrKindCLI}, now)

	st, err := s.Stats(ctx, "media", now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Depth != 0 {
		t.Errorf("Depth = %d", st.Depth)
	}
	if st.CompletedJobs7d != 2 {
		t.Errorf("CompletedJobs7d = %d", st.CompletedJobs7d)
	}
	if st.SuccessRate7d == nil || *st.SuccessRate7d != 0.5 {
		t.Errorf("SuccessRate7d = %v, want 0.5", st.SuccessRate7d)
	}
	if st.LastSuccessAt == nil || st.LastFailureAt == nil {
		t.Errorf("last success/failure not recorded: %+v", st)
	}

	// A queue with no completed jobs reports no rate rather than a fake 0%.
	empty, _ := s.Stats(ctx, "adhoc", now)
	if empty.SuccessRate7d != nil {
		t.Errorf("SuccessRate7d = %v, want nil", *empty.SuccessRate7d)
	}
	if empty.Depth != 1 {
		t.Errorf("adhoc depth = %d", empty.Depth)
	}
}

func TestFailQueued(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.Insert(ctx, newJob("01A", "gone"))
	s.Insert(ctx, newJob("01B", "gone"))
	s.Insert(ctx, newJob("01C", "media"))

	ids, err := s.FailQueued(ctx, "gone", ErrKindQueueRemoved, "queue removed from config", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("failed %v", ids)
	}
	j, _ := s.Get(ctx, "01A")
	if j.Status != StatusFailed || j.ErrorKind != ErrKindQueueRemoved {
		t.Errorf("job = %+v", j)
	}
	if other, _ := s.Get(ctx, "01C"); other.Status != StatusQueued {
		t.Error("other queues must be unaffected")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Insert(context.Background(), newJob("01A", "media"))
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if j, err := s2.Get(context.Background(), "01A"); err != nil || j.Queue != "media" {
		t.Errorf("data lost across reopen: %v %v", j, err)
	}
}
