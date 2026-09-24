package executor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/claude-spool-be/internal/config"
	"github.com/krelinga/claude-spool-be/internal/store"
)

// fakeClaude stands in for the CLI. It reports what environment and working
// directory it was given, then emits a stream-json run whose shape is chosen
// by a marker in the prompt.
const fakeClaude = `#!/bin/bash
prompt=""
while [ $# -gt 0 ]; do
  case "$1" in
    -p) prompt="$2"; shift 2 ;;
    *) shift ;;
  esac
done

if [ -n "$SPOOL_TEST_DUMP" ]; then
  env > "$SPOOL_TEST_DUMP/env.txt"
  pwd > "$SPOOL_TEST_DUMP/cwd.txt"
  ls -A . > "$SPOOL_TEST_DUMP/workdir.txt"
  printf '%s' "$prompt" > "$SPOOL_TEST_DUMP/prompt.txt"
fi

init='{"type":"system","subtype":"init","session_id":"sess-1","slash_commands":["notion-media"],"mcp_servers":[{"name":"notion","status":"connected"}]}'
tool='{"type":"assistant","session_id":"sess-1","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Skill"}]}}'

case "$prompt" in
  *HANG*)
    echo "$init"
    sleep 300
    ;;
  *NOCONNECTOR*)
    echo '{"type":"system","subtype":"init","session_id":"sess-1","slash_commands":[],"mcp_servers":[]}'
    sleep 30
    ;;
  *AUTHCLEAN*)
    echo "$init"
    echo '{"type":"result","subtype":"error_during_execution","session_id":"sess-1","is_error":true,"result":"Login expired. Run /login."}'
    exit 1
    ;;
  *AUTHDIRTY*)
    echo "$init"
    echo "$tool"
    echo '{"type":"result","subtype":"error_during_execution","session_id":"sess-1","is_error":true,"result":"Login expired. Run /login."}'
    exit 1
    ;;
  *USAGE*)
    echo "$init"
    echo '{"type":"result","subtype":"error_during_execution","session_id":"sess-1","is_error":true,"result":"Claude AI usage limit reached"}'
    exit 1
    ;;
  *NEEDSINPUT*)
    echo "$init"
    echo '{"type":"result","subtype":"success","session_id":"sess-1","is_error":false,"num_turns":2,"structured_output":{"status":"needs_input","summary":"Which edition?","question":"Which edition?"}}'
    ;;
  *CRASH*)
    echo "$init"
    echo "boom: something broke" >&2
    exit 3
    ;;
  *)
    echo "$init"
    echo "$tool"
    echo '{"type":"result","subtype":"success","session_id":"sess-1","is_error":false,"num_turns":3,"total_cost_usd":0.05,"duration_ms":1234,"structured_output":{"status":"succeeded","summary":"Added it","notion_url":"https://notion.so/x"}}'
    ;;
esac
`

const testQueues = `
queues:
  media:
    prompt: |
      /notion-media {{input}}
    requires: { skills: [notion-media], connectors: [notion] }
    allowed_tools: [Skill]
    timeout: 5s
    weight: 1
  adhoc:
    prompt: "{{input}}"
    allowed_tools: [Skill]
    timeout: 5s
    weight: 1
`

type harness struct {
	e    *Executor
	st   *store.Store
	cfg  *config.Config
	dump string
	tmp  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tmp := t.TempDir()

	bin := filepath.Join(tmp, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(tmp, "dump")
	if err := os.MkdirAll(dump, 0o755); err != nil {
		t.Fatal(err)
	}

	data := filepath.Join(tmp, "data")
	cfg := &config.Config{
		DataDir: data,
		Claude: config.ClaudeConfig{
			Binary: bin, ConfigDir: filepath.Join(data, "claude"),
			CredentialMode: config.ModeLogin, SyncSkills: true,
		},
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	qs, err := config.ParseQueues([]byte(testQueues))
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("SPOOL_TEST_DUMP", dump)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := New(cfg, st, func() *config.QueueSet { return qs }, log)
	return &harness{e: e, st: st, cfg: cfg, dump: dump, tmp: tmp}
}

func (h *harness) submit(t *testing.T, id, queue, input string) *store.Job {
	t.Helper()
	j := &store.Job{
		ID: id, Queue: queue, QueueConfigHash: "h", Status: store.StatusQueued,
		Input: input, SubmittedBy: "test", CreatedAt: time.Now().UTC(),
	}
	if _, created, err := h.st.Insert(context.Background(), j); err != nil || !created {
		t.Fatalf("submit: %v %v", created, err)
	}
	return j
}

// stepOnce runs exactly one scheduling step.
func (h *harness) stepOnce(t *testing.T) {
	t.Helper()
	if _, err := h.e.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
}

func (h *harness) get(t *testing.T, id string) *store.Job {
	t.Helper()
	j, err := h.st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return j
}

func (h *harness) dumpFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dump, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestRunJobSuccess(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "media", "Dune")
	h.stepOnce(t)

	j := h.get(t, "01A")
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status = %v (%s)", j.Status, j.ErrorMessage)
	}
	if j.Summary != "Added it" {
		t.Errorf("Summary = %q", j.Summary)
	}
	if j.SessionID != "sess-1" || j.NumTurns != 3 || j.CostUSD != 0.05 || j.ToolCalls != 1 {
		t.Errorf("run stats not recorded: %+v", j)
	}
	if j.FinishedAt == nil || j.StartedAt == nil {
		t.Error("timestamps missing")
	}
	// The queue's template was applied, not the raw input, and the block
	// scalar's trailing newline was trimmed.
	if j.RenderedPrompt != "/notion-media Dune" {
		t.Errorf("RenderedPrompt = %q", j.RenderedPrompt)
	}
	if got := h.dumpFile(t, "prompt.txt"); got != "/notion-media Dune" {
		t.Errorf("claude received prompt %q", got)
	}
	// The queue's outcome extension survives into stored outcome.
	if !strings.Contains(string(j.Outcome), "notion.so") {
		t.Errorf("Outcome = %s", j.Outcome)
	}

	transcript := filepath.Join(h.cfg.TranscriptDir(), "01A.jsonl")
	b, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("transcript not written: %v", err)
	}
	if n := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; n != 3 {
		t.Errorf("transcript has %d lines, want 3:\n%s", n, b)
	}
}

// The invariant that keeps skills and connectors working (§3.1).
func TestSubprocessEnvironmentIsClean(t *testing.T) {
	h := newHarness(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-must-not-leak")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-must-not-leak")

	h.submit(t, "01A", "media", "Dune")
	h.stepOnce(t)

	env := h.dumpFile(t, "env.txt")
	for _, leak := range []string{"sk-must-not-leak", "tok-must-not-leak"} {
		if strings.Contains(env, leak) {
			t.Errorf("credential override reached the subprocess: %s", leak)
		}
	}
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR="+h.cfg.Claude.ConfigDir) {
		t.Error("CLAUDE_CONFIG_DIR not set to the one login directory")
	}
	if !strings.Contains(env, "CLAUDE_CODE_SYNC_SKILLS=1") {
		t.Error("skill sync not enabled")
	}
	if !strings.Contains(env, "DISABLE_AUTOUPDATER=1") {
		t.Error("autoupdater not disabled")
	}
}

// Every job gets a fresh, empty working directory (§3.1).
func TestWorkingDirectoryIsFreshAndEmpty(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "media", "Dune")
	h.stepOnce(t)

	cwd := strings.TrimSpace(h.dumpFile(t, "cwd.txt"))
	if !strings.HasSuffix(cwd, filepath.Join("work", "01A")) {
		t.Errorf("cwd = %q, want the job's own work dir", cwd)
	}
	if contents := strings.TrimSpace(h.dumpFile(t, "workdir.txt")); contents != "" {
		t.Errorf("work dir was not empty: %q", contents)
	}
	// It is cleaned up afterwards rather than accumulating.
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Errorf("work dir survived the run: %v", err)
	}
}

func TestTimeoutKillsRun(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "media", "HANG")

	start := time.Now()
	h.stepOnce(t)
	elapsed := time.Since(start)

	j := h.get(t, "01A")
	if j.Status != store.StatusFailed || j.ErrorKind != store.ErrKindTimeout {
		t.Fatalf("got %v/%v, want failed/timeout", j.Status, j.ErrorKind)
	}
	// 5s queue timeout, then SIGINT is enough to stop a sleeping shell.
	if elapsed > 20*time.Second {
		t.Errorf("timeout took %v; escalation may not be working", elapsed)
	}
}

// Auth failure with no tool calls: block the executor, requeue the job (§3.5).
func TestAuthFailureWithNoToolCallsRequeues(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "adhoc", "AUTHCLEAN")
	h.stepOnce(t)

	j := h.get(t, "01A")
	if j.Status != store.StatusQueued {
		t.Errorf("status = %v, want requeued", j.Status)
	}
	if j.RunAttempts != 1 {
		t.Errorf("RunAttempts = %d", j.RunAttempts)
	}
	st, _ := h.st.ExecutorState(context.Background())
	if st.State != store.ExecBlockedAuth {
		t.Errorf("executor state = %v, want blocked_auth", st.State)
	}
	// An auth block must not lapse on a timer: it needs a human.
	if st.BlockedUntil != nil {
		t.Errorf("auth block has a timer: %v", st.BlockedUntil)
	}
	// While blocked, nothing else runs.
	h.submit(t, "01B", "adhoc", "fine")
	h.stepOnce(t)
	if b := h.get(t, "01B"); b.Status != store.StatusQueued {
		t.Errorf("job ran while blocked: %v", b.Status)
	}
}

// Auth failure after a tool call may have had side effects: fail it, do not
// silently re-run it.
func TestAuthFailureAfterToolCallDoesNotRequeue(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "adhoc", "AUTHDIRTY")
	h.stepOnce(t)

	j := h.get(t, "01A")
	if j.Status != store.StatusFailed || j.ErrorKind != store.ErrKindAuth {
		t.Errorf("got %v/%v, want failed/auth", j.Status, j.ErrorKind)
	}
	if j.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d", j.ToolCalls)
	}
}

func TestUsageLimitBlocksWithTimer(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "adhoc", "USAGE")
	h.stepOnce(t)

	st, _ := h.st.ExecutorState(context.Background())
	if st.State != store.ExecBlockedUsage {
		t.Fatalf("state = %v, want blocked_usage", st.State)
	}
	// No reset time in the message, so a bounded backoff rather than forever.
	if st.BlockedUntil == nil {
		t.Fatal("usage block has no expiry")
	}
	if d := time.Until(*st.BlockedUntil); d <= 0 || d > 2*time.Hour {
		t.Errorf("backoff = %v, want a bounded future time", d)
	}
	if j := h.get(t, "01A"); j.Status != store.StatusQueued {
		t.Errorf("job status = %v, want requeued", j.Status)
	}
}

// A missing connector is a config problem, not a one-off: pause the queue so
// it stops burning jobs, and leave other queues alone (§3.4).
func TestCapabilityMissingPausesOnlyThatQueue(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "media", "NOCONNECTOR")
	h.stepOnce(t)

	j := h.get(t, "01A")
	if j.Status != store.StatusFailed || j.ErrorKind != store.ErrKindCapabilityMissing {
		t.Fatalf("got %v/%v: %s", j.Status, j.ErrorKind, j.ErrorMessage)
	}
	if !strings.Contains(j.ErrorMessage, "notion") {
		t.Errorf("error should name what is missing: %q", j.ErrorMessage)
	}
	rt, _ := h.st.QueueRuntime(context.Background(), "media")
	if !rt.Paused || rt.PausedReason != "capability_missing" {
		t.Errorf("media not auto-paused: %+v", rt)
	}
	// The executor itself is untouched, and other queues keep running.
	st, _ := h.st.ExecutorState(context.Background())
	if st.State != store.ExecReady {
		t.Errorf("executor state = %v, want ready", st.State)
	}
	rt2, _ := h.st.QueueRuntime(context.Background(), "adhoc")
	if rt2.Paused {
		t.Error("adhoc should be unaffected")
	}
	h.submit(t, "01B", "adhoc", "fine")
	h.stepOnce(t)
	if b := h.get(t, "01B"); b.Status != store.StatusSucceeded {
		t.Errorf("adhoc job = %v, want it to still run", b.Status)
	}
}

func TestPausedQueueIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.st.SetQueuePaused(context.Background(), "media", true, "manual")
	h.submit(t, "01A", "media", "Dune")
	h.submit(t, "01B", "adhoc", "other")

	h.stepOnce(t)
	h.stepOnce(t)

	if a := h.get(t, "01A"); a.Status != store.StatusQueued {
		t.Errorf("paused queue ran: %v", a.Status)
	}
	if b := h.get(t, "01B"); b.Status != store.StatusSucceeded {
		t.Errorf("unpaused queue did not run: %v", b.Status)
	}
}

func TestNeedsInputAndCrash(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "adhoc", "NEEDSINPUT")
	h.submit(t, "01B", "adhoc", "CRASH")
	h.stepOnce(t)
	h.stepOnce(t)

	if a := h.get(t, "01A"); a.Status != store.StatusNeedsInput || a.Summary != "Which edition?" {
		t.Errorf("needs_input job = %+v", a)
	}
	b := h.get(t, "01B")
	if b.Status != store.StatusFailed || b.ErrorKind != store.ErrKindCLI {
		t.Errorf("crashed job = %v/%v", b.Status, b.ErrorKind)
	}
	if !strings.Contains(b.ErrorMessage, "boom") {
		t.Errorf("stderr not captured in error: %q", b.ErrorMessage)
	}
}

// The design's motivating case, end to end: a burst in one queue must not
// starve a single job in another.
func TestBurstDoesNotStarveOtherQueue(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"01A", "01B", "01C", "01D"} {
		h.submit(t, id, "media", "Dune")
	}
	h.submit(t, "01Z", "adhoc", "one-off")

	// The adhoc job should run within the first couple of picks, not last.
	for range 2 {
		h.stepOnce(t)
	}
	if z := h.get(t, "01Z"); z.Status == store.StatusQueued {
		t.Error("adhoc job still waiting after two picks behind a media burst")
	}
}

func TestRecoverMarksInterruptedAndFailsRemovedQueues(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "media", "Dune")
	h.st.Claim(context.Background(), "media", time.Now())
	h.submit(t, "01B", "gone", "orphan")

	if err := h.e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if a := h.get(t, "01A"); a.Status != store.StatusInterrupted {
		t.Errorf("in-flight job = %v, want interrupted", a.Status)
	}
	if b := h.get(t, "01B"); b.Status != store.StatusFailed || b.ErrorKind != store.ErrKindQueueRemoved {
		t.Errorf("orphaned job = %v/%v", b.Status, b.ErrorKind)
	}
}

func TestRunningReportsCurrentJob(t *testing.T) {
	h := newHarness(t)
	h.submit(t, "01A", "media", "HANG")

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.e.step(context.Background())
	}()

	deadline := time.After(10 * time.Second)
	for {
		if id, queue, ok := h.e.Running(); ok {
			if id != "01A" || queue != "media" {
				t.Errorf("Running = %s/%s", id, queue)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("job never reported as running")
		case <-time.After(20 * time.Millisecond):
		}
	}
	<-done
	if _, _, ok := h.e.Running(); ok {
		t.Error("Running should be clear once the job finishes")
	}
}
