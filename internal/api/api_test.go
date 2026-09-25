package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/claude-spool-be/internal/auth"
	"github.com/krelinga/claude-spool-be/internal/config"
	"github.com/krelinga/claude-spool-be/internal/event"
	"github.com/krelinga/claude-spool-be/internal/store"
)

const (
	adminToken  = "admin-token-0123456789"
	scopedToken = "scoped-token-0123456789"
)

const apiConfig = `
data_dir: DATA_DIR
tokens:
  - name: admin
    token: "admin-token-0123456789"
    queues: ["*"]
  - name: ios-share
    token: "scoped-token-0123456789"
    queues: [media]
`

const apiQueues = `
queues:
  media:
    description: Add entries to Notion Media
    prompt: "/notion-media {{input}}"
    args:
      url: { type: string, format: uri }
    outcome_extension:
      notion_url: { type: string }
    allowed_tools: [Skill, "mcp__notion__notion-search"]
    system_prompt: "secret policy text"
    weight: 2
  adhoc:
    prompt: "{{input}}"
    allowed_tools: [Skill]
`

type fakeExec struct {
	woken     int
	running   [2]string
	isRunning bool
	// cancelOK is what Cancel reports; cancelled records what was asked for.
	cancelOK  bool
	cancelled string
}

func (f *fakeExec) Wake() { f.woken++ }
func (f *fakeExec) Running() (string, string, bool) {
	return f.running[0], f.running[1], f.isRunning
}

func (f *fakeExec) Cancel(id string) bool {
	f.cancelled = id
	return f.cancelOK
}

type apiHarness struct {
	srv    http.Handler
	st     *store.Store
	cfg    *config.Config
	exec   *fakeExec
	broker *event.Broker
	auth   *fakeAuth
}

// fakeAuth stands in for the auth manager: the real one spawns claude.
type fakeAuth struct {
	mu            sync.Mutex
	status        auth.Status
	keepaliveErr  error
	keepaliveRuns int
	startErr      error
	submitErr     error
	submitted     string
	cancelled     string
	attemptID     string
}

func (f *fakeAuth) Status(context.Context) auth.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeAuth) Probe(ctx context.Context) (auth.Status, error) { return f.Status(ctx), nil }

func (f *fakeAuth) Keepalive(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keepaliveRuns++
	return f.keepaliveErr
}

func (f *fakeAuth) StartLogin(context.Context) (string, string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", "", time.Time{}, f.startErr
	}
	f.attemptID = "login_test"
	return f.attemptID, "https://claude.com/cai/oauth/authorize?code=true&state=x",
		time.Now().Add(10 * time.Minute), nil
}

func (f *fakeAuth) SubmitCode(ctx context.Context, id, code string) (auth.Status, error) {
	f.mu.Lock()
	if f.submitErr != nil {
		err := f.submitErr
		f.mu.Unlock()
		return f.Status(ctx), err
	}
	f.submitted = code
	f.status.State = auth.StateOK
	f.mu.Unlock()
	return f.Status(ctx), nil
}

func (f *fakeAuth) CancelLogin(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = id
	return nil
}

func newAPI(t *testing.T) *apiHarness {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(strings.Replace(apiConfig, "DATA_DIR", dir, 1)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	qs, err := config.ParseQueues([]byte(apiQueues))
	if err != nil {
		t.Fatal(err)
	}
	ex := &fakeExec{}
	broker := event.NewBroker()
	notifier := event.NewNotifier(cfg, func() *config.QueueSet { return qs }, broker)
	fa := &fakeAuth{status: auth.Status{State: auth.StateOK, Account: "me@example.com",
		Mode: config.ModeLogin, Relogins: 2}}
	s := New(cfg, st, func() *config.QueueSet { return qs }, ex, fa, notifier, broker,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &apiHarness{srv: s.Handler(), st: st, cfg: cfg, exec: ex, broker: broker, auth: fa}
}

func (h *apiHarness) do(t *testing.T, method, path, token string, body any, headers ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, kv := range headers {
		req.Header.Set(kv[0], kv[1])
	}
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, req)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return v
}

// --- auth ---

func TestAuthRequired(t *testing.T) {
	h := newAPI(t)
	for _, path := range []string{"/v1/queues", "/v1/jobs", "/v1/executor"} {
		w := h.do(t, "GET", path, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", path, w.Code)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: missing WWW-Authenticate", path)
		}
	}
	if w := h.do(t, "GET", "/v1/queues", "wrong-token-here", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", w.Code)
	}
	// Health is deliberately open, and says nothing about the workload.
	if w := h.do(t, "GET", "/healthz", "", nil); w.Code != http.StatusOK {
		t.Errorf("healthz = %d", w.Code)
	}
}

func TestMalformedAuthorizationHeader(t *testing.T) {
	h := newAPI(t)
	for _, hdr := range []string{"Basic abc", "Bearer", "Bearer   ", adminToken} {
		req := httptest.NewRequest("GET", "/v1/queues", nil)
		req.Header.Set("Authorization", hdr)
		w := httptest.NewRecorder()
		h.srv.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q = %d, want 401", hdr, w.Code)
		}
	}
}

// A scoped token must not reach adhoc, which is effectively "run anything the
// allowlist permits" (§3.3).
func TestTokenScoping(t *testing.T) {
	h := newAPI(t)

	w := h.do(t, "POST", "/v1/queues/adhoc/jobs", scopedToken, submitRequest{Input: "rm -rf"})
	if w.Code != http.StatusNotFound {
		t.Errorf("scoped token reached adhoc: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/v1/queues/adhoc", scopedToken, nil); w.Code != http.StatusNotFound {
		t.Errorf("scoped token read adhoc: %d", w.Code)
	}
	// Its own queue still works.
	if w := h.do(t, "POST", "/v1/queues/media/jobs", scopedToken, submitRequest{Input: "Dune"}); w.Code != http.StatusAccepted {
		t.Errorf("scoped token blocked from its own queue: %d %s", w.Code, w.Body)
	}
	// And the listing shows only what it can reach.
	list := decode[struct {
		Queues []queueView `json:"queues"`
	}](t, h.do(t, "GET", "/v1/queues", scopedToken, nil))
	if len(list.Queues) != 1 || list.Queues[0].Name != "media" {
		t.Errorf("scoped listing = %+v", list.Queues)
	}
}

func TestScopedTokenCannotSeeOtherQueuesJobs(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/adhoc/jobs", adminToken, submitRequest{Input: "secret"}))

	if w := h.do(t, "GET", "/v1/jobs/"+created.ID, scopedToken, nil); w.Code != http.StatusNotFound {
		t.Errorf("scoped token read another queue's job: %d", w.Code)
	}
	if w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/cancel", scopedToken, nil); w.Code != http.StatusNotFound {
		t.Errorf("scoped token cancelled another queue's job: %d", w.Code)
	}
	feed := decode[struct {
		Jobs []store.Job `json:"jobs"`
	}](t, h.do(t, "GET", "/v1/jobs", scopedToken, nil))
	for _, j := range feed.Jobs {
		if j.Queue == "adhoc" {
			t.Errorf("adhoc job leaked into a scoped feed: %+v", j)
		}
	}
}

// --- queues ---

// Prompts and tool allowlists are policy; the API must not hand them out.
func TestQueueViewOmitsPolicy(t *testing.T) {
	h := newAPI(t)
	w := h.do(t, "GET", "/v1/queues/media", adminToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	body := w.Body.String()
	for _, secret := range []string{"notion-media", "secret policy text", "mcp__notion__notion-search"} {
		if strings.Contains(body, secret) {
			t.Errorf("queue view leaked %q: %s", secret, body)
		}
	}
	// It still carries what a picker needs.
	for _, want := range []string{"Add entries to Notion Media", "url", "notion_url", "config_hash"} {
		if !strings.Contains(body, want) {
			t.Errorf("queue view missing %q: %s", want, body)
		}
	}
}

func TestGetQueueStats(t *testing.T) {
	h := newAPI(t)
	h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "a"})
	h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "b"})

	v := decode[queueView](t, h.do(t, "GET", "/v1/queues/media", adminToken, nil))
	if v.Depth != 2 {
		t.Errorf("Depth = %d, want 2", v.Depth)
	}
	if v.Weight != 2 {
		t.Errorf("Weight = %v", v.Weight)
	}
	if v.Paused {
		t.Error("queue should start unpaused")
	}
}

func TestPauseResumeQueue(t *testing.T) {
	h := newAPI(t)
	if w := h.do(t, "POST", "/v1/queues/media/pause", adminToken, nil); w.Code != http.StatusOK {
		t.Fatalf("pause = %d %s", w.Code, w.Body)
	}
	v := decode[queueView](t, h.do(t, "GET", "/v1/queues/media", adminToken, nil))
	if !v.Paused || v.PausedReason != "manual" {
		t.Errorf("not paused: %+v", v)
	}
	// Submission is still accepted while paused: queues never stop accepting.
	if w := h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "x"}); w.Code != http.StatusAccepted {
		t.Errorf("paused queue refused a submission: %d", w.Code)
	}

	before := h.exec.woken
	h.do(t, "POST", "/v1/queues/media/resume", adminToken, nil)
	v = decode[queueView](t, h.do(t, "GET", "/v1/queues/media", adminToken, nil))
	if v.Paused {
		t.Error("still paused after resume")
	}
	if h.exec.woken <= before {
		t.Error("resume should wake the executor")
	}
}

// --- submission ---

func TestSubmitJob(t *testing.T) {
	h := newAPI(t)
	before := h.exec.woken
	w := h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{
		Input: "Dune", Args: map[string]any{"url": "https://example.com"},
		Labels: []string{"book"}, ClientRef: "share-1",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d %s", w.Code, w.Body)
	}
	got := decode[submitResponse](t, w)
	if got.ID == "" || got.Status != store.StatusQueued || got.Queue != "media" {
		t.Errorf("response = %+v", got)
	}
	if got.Position != 0 {
		t.Errorf("Position = %d, want 0 for the first job", got.Position)
	}
	if h.exec.woken != before+1 {
		t.Error("submission should wake the executor")
	}

	j, err := h.st.Get(t.Context(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.SubmittedBy != "admin" {
		t.Errorf("SubmittedBy = %q", j.SubmittedBy)
	}
	if j.QueueConfigHash == "" {
		t.Error("job did not record the queue config hash it was accepted under")
	}
}

func TestSubmitValidation(t *testing.T) {
	h := newAPI(t)
	cases := map[string]any{
		"empty":        submitRequest{},
		"blank input":  submitRequest{Input: "   "},
		"bad arg type": submitRequest{Input: "x", Args: map[string]any{"url": 42}},
		"relative uri": submitRequest{Input: "x", Args: map[string]any{"url": "/nope"}},
		"unknown arg":  submitRequest{Input: "x", Args: map[string]any{"nope": "x"}},
	}
	for name, body := range cases {
		if w := h.do(t, "POST", "/v1/queues/media/jobs", adminToken, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, w.Code)
		}
	}
	// Unknown top-level fields are rejected rather than silently dropped.
	req := httptest.NewRequest("POST", "/v1/queues/media/jobs",
		strings.NewReader(`{"input":"x","nonsense":true}`))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", w.Code)
	}
}

func TestSubmitUnknownQueue(t *testing.T) {
	h := newAPI(t)
	if w := h.do(t, "POST", "/v1/queues/nope/jobs", adminToken, submitRequest{Input: "x"}); w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// A share sheet that fires twice must not create two Notion pages.
func TestIdempotentSubmit(t *testing.T) {
	h := newAPI(t)
	key := [2]string{"Idempotency-Key", "share-sheet-abc"}

	first := h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}, key)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first = %d", first.Code)
	}
	second := h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}, key)
	if second.Code != http.StatusOK {
		t.Errorf("replay = %d, want 200 (not a second 202)", second.Code)
	}
	a, b := decode[submitResponse](t, first), decode[submitResponse](t, second)
	if a.ID != b.ID {
		t.Errorf("replay created a new job: %s vs %s", a.ID, b.ID)
	}
	jobs, _ := h.st.List(t.Context(), store.ListFilter{Queue: "media"})
	if len(jobs) != 1 {
		t.Errorf("stored %d jobs, want 1", len(jobs))
	}
}

func TestQueuePosition(t *testing.T) {
	h := newAPI(t)
	for range 3 {
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "x"})
	}
	last := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "x"}))
	if last.Position != 3 {
		t.Errorf("Position = %d, want 3", last.Position)
	}
}

// --- jobs ---

func TestListJobsFilterAndPaging(t *testing.T) {
	h := newAPI(t)
	for range 3 {
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "m"})
	}
	h.do(t, "POST", "/v1/queues/adhoc/jobs", adminToken, submitRequest{Input: "a"})

	type listBody struct {
		Jobs       []store.Job `json:"jobs"`
		NextCursor string      `json:"next_cursor"`
	}
	all := decode[listBody](t, h.do(t, "GET", "/v1/jobs", adminToken, nil))
	if len(all.Jobs) != 4 {
		t.Errorf("got %d jobs", len(all.Jobs))
	}
	byQueue := decode[listBody](t, h.do(t, "GET", "/v1/jobs?queue=media", adminToken, nil))
	if len(byQueue.Jobs) != 3 {
		t.Errorf("queue filter got %d", len(byQueue.Jobs))
	}
	byStatus := decode[listBody](t, h.do(t, "GET", "/v1/jobs?status=succeeded", adminToken, nil))
	if len(byStatus.Jobs) != 0 {
		t.Errorf("status filter got %d", len(byStatus.Jobs))
	}
	if w := h.do(t, "GET", "/v1/jobs?status=bogus", adminToken, nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad status filter = %d", w.Code)
	}
	if w := h.do(t, "GET", "/v1/jobs?since=nonsense", adminToken, nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad since = %d", w.Code)
	}

	page := decode[listBody](t, h.do(t, "GET", "/v1/jobs?limit=2", adminToken, nil))
	if len(page.Jobs) != 2 || page.NextCursor == "" {
		t.Fatalf("paging: %d jobs, cursor %q", len(page.Jobs), page.NextCursor)
	}
	next := decode[listBody](t, h.do(t, "GET", "/v1/jobs?limit=2&cursor="+page.NextCursor, adminToken, nil))
	if len(next.Jobs) != 2 || next.Jobs[0].ID >= page.Jobs[1].ID {
		t.Errorf("second page did not advance: %+v", next.Jobs)
	}
}

func TestGetJobAndCancel(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}))

	j := decode[store.Job](t, h.do(t, "GET", "/v1/jobs/"+created.ID, adminToken, nil))
	if j.ID != created.ID || j.Input != "Dune" {
		t.Errorf("job = %+v", j)
	}
	if w := h.do(t, "GET", "/v1/jobs/nope", adminToken, nil); w.Code != http.StatusNotFound {
		t.Errorf("missing job = %d", w.Code)
	}

	cancelled := decode[store.Job](t, h.do(t, "POST", "/v1/jobs/"+created.ID+"/cancel", adminToken, nil))
	if cancelled.Status != store.StatusCancelled {
		t.Errorf("status = %v", cancelled.Status)
	}
	// Cancelling twice is a conflict, not a silent success.
	if w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/cancel", adminToken, nil); w.Code != http.StatusConflict {
		t.Errorf("double cancel = %d, want 409", w.Code)
	}
}

func TestTranscript(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}))

	// No transcript until the job has run.
	if w := h.do(t, "GET", "/v1/jobs/"+created.ID+"/transcript", adminToken, nil); w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}

	os.MkdirAll(h.cfg.TranscriptDir(), 0o755)
	line := `{"type":"result","subtype":"success"}`
	os.WriteFile(filepath.Join(h.cfg.TranscriptDir(), created.ID+".jsonl"), []byte(line+"\n"), 0o600)

	w := h.do(t, "GET", "/v1/jobs/"+created.ID+"/transcript", adminToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), line) {
		t.Errorf("body = %q", w.Body.String())
	}
}

// --- executor ---

func TestExecutorEndpoints(t *testing.T) {
	h := newAPI(t)
	h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "x"})
	h.exec.isRunning = true
	h.exec.running = [2]string{"01RUNNING", "media"}

	v := decode[executorView](t, h.do(t, "GET", "/v1/executor", adminToken, nil))
	if v.State != store.ExecReady {
		t.Errorf("State = %v", v.State)
	}
	if v.RunningJob != "01RUNNING" || v.RunningQueue != "media" {
		t.Errorf("running job = %+v", v)
	}
	if v.TotalDepth != 1 {
		t.Errorf("TotalDepth = %d", v.TotalDepth)
	}

	v = decode[executorView](t, h.do(t, "POST", "/v1/executor/pause", adminToken, nil))
	if v.State != store.ExecPaused {
		t.Errorf("after pause = %v", v.State)
	}
	v = decode[executorView](t, h.do(t, "POST", "/v1/executor/resume", adminToken, nil))
	if v.State != store.ExecReady {
		t.Errorf("after resume = %v", v.State)
	}
}

// Resume clears a usage block early, but must not paper over an auth block:
// only a re-login clears that (§3.7).
func TestResumeRefusesToClearAuthBlock(t *testing.T) {
	h := newAPI(t)
	until := time.Now().Add(time.Hour)
	h.st.SetExecutorState(t.Context(), store.ExecBlockedUsage, "limit", &until)
	if v := decode[executorView](t, h.do(t, "POST", "/v1/executor/resume", adminToken, nil)); v.State != store.ExecReady {
		t.Errorf("usage block not cleared: %v", v.State)
	}

	h.st.SetExecutorState(t.Context(), store.ExecBlockedAuth, "login expired", nil)
	w := h.do(t, "POST", "/v1/executor/resume", adminToken, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("resume over an auth block = %d, want 409", w.Code)
	}
	st, _ := h.st.ExecutorState(t.Context())
	if st.State != store.ExecBlockedAuth {
		t.Errorf("auth block was cleared: %v", st.State)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newAPI(t)
	if w := h.do(t, "DELETE", "/v1/queues/media", adminToken, nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", w.Code)
	}
}

// --- events and metrics ---

func TestCancelEmitsEvent(t *testing.T) {
	h := newAPI(t)
	events, stop := h.broker.Subscribe()
	defer stop()

	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}))
	h.do(t, "POST", "/v1/jobs/"+created.ID+"/cancel", adminToken, nil)

	select {
	case env := <-events:
		if env.Event != event.JobCancelled || env.JobID != created.ID {
			t.Errorf("envelope = %+v", env)
		}
	case <-time.After(2 * time.Second):
		t.Error("cancellation did not emit an event")
	}
}

func TestEventStream(t *testing.T) {
	h := newAPI(t)
	srv := httptest.NewServer(h.srv)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	// The stream opens with a comment so a client knows it is connected.
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("no preamble: %v", err)
	}

	// Publishing requires a subscriber to exist first, which the read above
	// guarantees.
	go func() {
		time.Sleep(50 * time.Millisecond)
		h.broker.Publish(event.Envelope{
			Event: event.JobSucceeded, EventID: "ev_test", At: time.Now(),
			Queue: "media", JobID: "01A",
		})
	}()

	var sawEvent, sawData bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !(sawEvent && sawData) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(line, "event: job.succeeded") {
			sawEvent = true
		}
		if strings.HasPrefix(line, "data: ") {
			var env event.Envelope
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
				t.Errorf("data is not a valid envelope: %v", err)
			}
			if env.JobID == "01A" {
				sawData = true
			}
		}
	}
	if !sawEvent || !sawData {
		t.Errorf("stream did not carry the event (event=%v data=%v)", sawEvent, sawData)
	}
}

// A scoped token must not see other queues' events on the live stream either.
func TestEventStreamRespectsTokenScope(t *testing.T) {
	h := newAPI(t)
	srv := httptest.NewServer(h.srv)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+scopedToken)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	reader.ReadString('\n')

	go func() {
		time.Sleep(50 * time.Millisecond)
		h.broker.Publish(event.Envelope{Event: event.JobSucceeded, EventID: "ev_adhoc",
			At: time.Now(), Queue: "adhoc", JobID: "01ADHOC"})
		h.broker.Publish(event.Envelope{Event: event.JobSucceeded, EventID: "ev_media",
			At: time.Now(), Queue: "media", JobID: "01MEDIA"})
	}()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, "01ADHOC") {
			t.Fatal("scoped token received another queue's event")
		}
		if strings.Contains(line, "01MEDIA") {
			return // its own queue arrived, and adhoc did not
		}
	}
	t.Error("scoped token never received its own queue's event")
}

func TestMetrics(t *testing.T) {
	h := newAPI(t)
	h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "x"})
	h.do(t, "POST", "/v1/queues/media/pause", adminToken, nil)

	// Prometheus scrapes this without a token.
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()

	for _, want := range []string{
		`spool_queue_depth{queue="media"} 1`,
		`spool_queue_depth{queue="adhoc"} 0`,
		`spool_queue_paused{queue="media"} 1`,
		`spool_queue_paused{queue="adhoc"} 0`,
		`spool_executor_state{state="ready"} 1`,
		`spool_executor_state{state="blocked_auth"} 0`,
		"spool_webhook_outbox",
		"spool_oldest_queued_age_seconds",
		"# TYPE spool_queue_depth gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	// Every metric must be preceded by HELP and TYPE, or Prometheus complains.
	// Histogram series are the exception: _bucket, _sum and _count share the
	// base name's TYPE declaration and must not carry their own.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "spool_") {
			continue
		}
		name := strings.FieldsFunc(line, func(r rune) bool { return r == '{' || r == ' ' })[0]
		base := name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if trimmed, ok := strings.CutSuffix(name, suffix); ok {
				if strings.Contains(body, "# TYPE "+trimmed+" histogram") {
					base = trimmed
				}
				break
			}
		}
		if !strings.Contains(body, "# TYPE "+base+" ") {
			t.Errorf("metric %s has no TYPE line", name)
		}
	}
}

func TestMetricsReflectsBlockedExecutor(t *testing.T) {
	h := newAPI(t)
	until := time.Now().Add(time.Hour)
	h.st.SetExecutorState(t.Context(), store.ExecBlockedUsage, "limit", &until)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `spool_executor_state{state="blocked_usage"} 1`) {
		t.Errorf("blocked state not reported:\n%s", body)
	}
	if !strings.Contains(body, `spool_executor_state{state="ready"} 0`) {
		t.Error("ready should be 0 while blocked")
	}
}

// --- auth endpoints ---

func TestGetAuth(t *testing.T) {
	h := newAPI(t)
	got := decode[auth.Status](t, h.do(t, "GET", "/v1/auth", adminToken, nil))
	if got.State != auth.StateOK || got.Account != "me@example.com" {
		t.Errorf("status = %+v", got)
	}
	if got.Mode != config.ModeLogin {
		t.Errorf("Mode = %q", got.Mode)
	}
	if got.Relogins != 2 {
		t.Errorf("Relogins = %d", got.Relogins)
	}
	// It needs a token like everything else under /v1.
	if w := h.do(t, "GET", "/v1/auth", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d", w.Code)
	}
}

func TestAuthCheck(t *testing.T) {
	h := newAPI(t)
	if w := h.do(t, "POST", "/v1/auth/check", adminToken, nil); w.Code != http.StatusOK {
		t.Fatalf("code = %d %s", w.Code, w.Body)
	}
	if h.auth.keepaliveRuns != 1 {
		t.Errorf("keepalive ran %d times", h.auth.keepaliveRuns)
	}

	// A busy executor is a conflict, not a server error: the credential is fine.
	h.auth.keepaliveErr = errors.New("claude is busy")
	if w := h.do(t, "POST", "/v1/auth/check", adminToken, nil); w.Code != http.StatusConflict {
		t.Errorf("busy = %d, want 409", w.Code)
	}
}

func TestLoginFlowEndpoints(t *testing.T) {
	h := newAPI(t)

	w := h.do(t, "POST", "/v1/auth/login", adminToken, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	start := decode[loginStartResponse](t, w)
	if start.AttemptID == "" || start.URL == "" {
		t.Fatalf("response = %+v", start)
	}
	if !strings.HasPrefix(start.URL, "https://claude.com/cai/oauth/authorize?") {
		t.Errorf("URL = %q", start.URL)
	}
	if !start.ExpiresAt.After(time.Now()) {
		t.Errorf("ExpiresAt = %v", start.ExpiresAt)
	}

	got := decode[auth.Status](t, h.do(t, "POST", "/v1/auth/login/"+start.AttemptID,
		adminToken, submitCodeRequest{Code: "the-code"}))
	if got.State != auth.StateOK {
		t.Errorf("State = %q", got.State)
	}
	if h.auth.submitted != "the-code" {
		t.Errorf("submitted = %q", h.auth.submitted)
	}
}

func TestLoginErrorMapping(t *testing.T) {
	h := newAPI(t)

	// Busy is a conflict, so a phone knows to retry rather than give up.
	h.auth.startErr = auth.ErrLoginBusy
	if w := h.do(t, "POST", "/v1/auth/login", adminToken, nil); w.Code != http.StatusConflict {
		t.Errorf("busy start = %d, want 409", w.Code)
	}
	h.auth.startErr = nil

	start := decode[loginStartResponse](t, h.do(t, "POST", "/v1/auth/login", adminToken, nil))

	h.auth.submitErr = auth.ErrNoAttempt
	if w := h.do(t, "POST", "/v1/auth/login/"+start.AttemptID, adminToken,
		submitCodeRequest{Code: "x"}); w.Code != http.StatusNotFound {
		t.Errorf("unknown attempt = %d, want 404", w.Code)
	}

	h.auth.submitErr = auth.ErrAttemptExpired
	if w := h.do(t, "POST", "/v1/auth/login/"+start.AttemptID, adminToken,
		submitCodeRequest{Code: "x"}); w.Code != http.StatusGone {
		t.Errorf("expired attempt = %d, want 410", w.Code)
	}

	// A wrong code is the user's to fix: 400, not 500.
	h.auth.submitErr = errors.New("login failed: Invalid authorization code.")
	w := h.do(t, "POST", "/v1/auth/login/"+start.AttemptID, adminToken, submitCodeRequest{Code: "bad"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad code = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid authorization code") {
		t.Errorf("the reason should reach the client: %s", w.Body)
	}
}

func TestCancelLoginEndpoint(t *testing.T) {
	h := newAPI(t)
	start := decode[loginStartResponse](t, h.do(t, "POST", "/v1/auth/login", adminToken, nil))
	if w := h.do(t, "DELETE", "/v1/auth/login/"+start.AttemptID, adminToken, nil); w.Code != http.StatusOK {
		t.Errorf("cancel = %d %s", w.Code, w.Body)
	}
	if h.auth.cancelled != start.AttemptID {
		t.Errorf("cancelled = %q", h.auth.cancelled)
	}
}

func TestAuthEventsEndpoint(t *testing.T) {
	h := newAPI(t)
	ctx := t.Context()
	base := time.Now().Add(-100 * time.Hour)
	h.st.RecordAuthEvent(ctx, store.AuthLoginCompleted, "a", base)
	h.st.RecordAuthEvent(ctx, store.AuthExpired, "a", base.Add(48*time.Hour))

	body := decode[struct {
		Events    []store.AuthEvent `json:"events"`
		Lifetimes []float64         `json:"session_lifetimes_seconds"`
		Note      string            `json:"note"`
	}](t, h.do(t, "GET", "/v1/auth/events", adminToken, nil))

	if len(body.Events) != 2 {
		t.Errorf("events = %d", len(body.Events))
	}
	if len(body.Lifetimes) != 1 || body.Lifetimes[0] != (48*time.Hour).Seconds() {
		t.Errorf("lifetimes = %v", body.Lifetimes)
	}
	if body.Note != "" {
		t.Errorf("note should be absent once data exists: %q", body.Note)
	}
}

// An empty result should explain itself rather than leaving a bare [].
func TestAuthEventsEmptyExplainsItself(t *testing.T) {
	h := newAPI(t)
	body := decode[struct {
		Note string `json:"note"`
	}](t, h.do(t, "GET", "/v1/auth/events", adminToken, nil))
	if body.Note == "" {
		t.Error("expected a note explaining why there are no lifetimes yet")
	}
}

func TestAuthMetrics(t *testing.T) {
	h := newAPI(t)
	ctx := t.Context()
	base := time.Now().Add(-100 * time.Hour)
	h.st.RecordAuthEvent(ctx, store.AuthLoginCompleted, "a", base)
	h.st.RecordAuthEvent(ctx, store.AuthExpired, "a", base.Add(48*time.Hour))
	h.auth.status.SessionAgeSec = ptr(3600.0)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{
		`spool_auth_state{state="ok"} 1`,
		`spool_auth_state{state="expired"} 0`,
		"spool_auth_relogins_total 2",
		"spool_auth_session_age_seconds 3600",
		"# TYPE spool_auth_session_lifetime_seconds histogram",
		`spool_auth_session_lifetime_seconds_bucket{le="259200"} 1`,
		"spool_auth_session_lifetime_seconds_count 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	// A 48h session must not land in the 24h bucket.
	if !strings.Contains(body, `spool_auth_session_lifetime_seconds_bucket{le="86400"} 0`) {
		t.Error("48h session was bucketed at or below 24h")
	}
}

func ptr[T any](v T) *T { return &v }

// --- retry, reply, cancelling a running job ---

// A retry must create a child, never re-run the original in place (§3.5).
func TestRetryCreatesChild(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{
			Input: "Dune", Labels: []string{"book"}, ClientRef: "share-1",
		}))
	// Drive it to a failed state.
	h.st.Claim(t.Context(), "media", time.Now())
	h.st.Finish(t.Context(), created.ID, store.Result{
		Status: store.StatusFailed, ErrorKind: store.ErrKindCLI, ErrorMessage: "boom",
	}, time.Now())

	w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/retry", adminToken, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", w.Code, w.Body)
	}
	child := decode[submitResponse](t, w)
	if child.ID == created.ID {
		t.Fatal("retry reused the original job id")
	}

	got, err := h.st.Get(t.Context(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentJobID != created.ID {
		t.Errorf("ParentJobID = %q", got.ParentJobID)
	}
	if got.Status != store.StatusQueued || got.Input != "Dune" {
		t.Errorf("child = %+v", got)
	}
	// Submission metadata carries over, so history reads sensibly.
	if got.ClientRef != "share-1" || len(got.Labels) != 1 {
		t.Errorf("child lost its metadata: %+v", got)
	}
	// The original is untouched.
	parent, _ := h.st.Get(t.Context(), created.ID)
	if parent.Status != store.StatusFailed {
		t.Errorf("parent status changed to %v", parent.Status)
	}
	// And the child is discoverable from the parent.
	kids, _ := h.st.Children(t.Context(), created.ID)
	if len(kids) != 1 || kids[0].ID != child.ID {
		t.Errorf("Children = %v", kids)
	}
}

// Retrying a job that succeeded would repeat side effects that already landed.
func TestRetryRefusesSucceeded(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}))
	h.st.Claim(t.Context(), "media", time.Now())
	h.st.Finish(t.Context(), created.ID, store.Result{
		Status: store.StatusSucceeded, Summary: "added"}, time.Now())

	w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/retry", adminToken, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("retry of a succeeded job = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "repeat") {
		t.Errorf("the reason should be explicit about side effects: %s", w.Body)
	}
}

func TestRetryRefusesQueuedAndRunning(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}))
	if w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/retry", adminToken, nil); w.Code != http.StatusConflict {
		t.Errorf("retry of a queued job = %d, want 409", w.Code)
	}
	h.st.Claim(t.Context(), "media", time.Now())
	if w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/retry", adminToken, nil); w.Code != http.StatusConflict {
		t.Errorf("retry of a running job = %d, want 409", w.Code)
	}
}

// A reply resumes the parent's session so Claude keeps its context.
func TestReplyResumesSession(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{
			Input: "Dune", Args: map[string]any{"url": "https://example.com"}}))
	h.st.Claim(t.Context(), "media", time.Now())
	h.st.Finish(t.Context(), created.ID, store.Result{
		Status: store.StatusNeedsInput, Summary: "Which edition?", SessionID: "sess-42",
	}, time.Now())

	w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/reply", adminToken,
		replyRequest{Input: "the 1965 first edition"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("reply = %d %s", w.Code, w.Body)
	}
	child := decode[submitResponse](t, w)
	got, err := h.st.Get(t.Context(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ResumeSession != "sess-42" {
		t.Errorf("ResumeSession = %q, want the parent's session", got.ResumeSession)
	}
	// The reply is the prompt; re-sending the original args would restate the
	// request instead of answering the question.
	if got.Input != "the 1965 first edition" {
		t.Errorf("Input = %q", got.Input)
	}
	if len(got.Args) != 0 {
		t.Errorf("Args should not carry over into a reply: %v", got.Args)
	}
	if got.ParentJobID != created.ID {
		t.Errorf("ParentJobID = %q", got.ParentJobID)
	}
}

func TestReplyValidation(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}))

	// Not needs_input yet.
	if w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/reply", adminToken,
		replyRequest{Input: "x"}); w.Code != http.StatusConflict {
		t.Errorf("reply to a queued job = %d, want 409", w.Code)
	}

	h.st.Claim(t.Context(), "media", time.Now())
	h.st.Finish(t.Context(), created.ID, store.Result{
		Status: store.StatusNeedsInput, Summary: "Which edition?", SessionID: "sess-1"}, time.Now())

	// Empty input.
	if w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/reply", adminToken,
		replyRequest{Input: "  "}); w.Code != http.StatusBadRequest {
		t.Errorf("empty reply = %d, want 400", w.Code)
	}

	// A needs_input job with no session cannot be resumed, and must say so
	// rather than silently restarting the task.
	other := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Other"}))
	h.st.Claim(t.Context(), "media", time.Now())
	h.st.Finish(t.Context(), other.ID, store.Result{
		Status: store.StatusNeedsInput, Summary: "?"}, time.Now())
	w := h.do(t, "POST", "/v1/jobs/"+other.ID+"/reply", adminToken, replyRequest{Input: "x"})
	if w.Code != http.StatusConflict {
		t.Errorf("reply without a session = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "retry it instead") {
		t.Errorf("the message should point somewhere useful: %s", w.Body)
	}
}

func TestCancelRunningJob(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}))
	h.st.Claim(t.Context(), "media", time.Now())

	h.exec.cancelOK = true
	w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/cancel", adminToken, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("cancel running = %d %s, want 202", w.Code, w.Body)
	}
	if h.exec.cancelled != created.ID {
		t.Errorf("executor asked to cancel %q", h.exec.cancelled)
	}
	// The API must not mark it cancelled itself: the executor owns the outcome.
	got, _ := h.st.Get(t.Context(), created.ID)
	if got.Status != store.StatusRunning {
		t.Errorf("status = %v; the executor should record the outcome", got.Status)
	}
}

// If it finished between the read and the cancel, say so rather than pretending.
func TestCancelRunningRaceLost(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/media/jobs", adminToken, submitRequest{Input: "Dune"}))
	h.st.Claim(t.Context(), "media", time.Now())

	h.exec.cancelOK = false
	if w := h.do(t, "POST", "/v1/jobs/"+created.ID+"/cancel", adminToken, nil); w.Code != http.StatusConflict {
		t.Errorf("code = %d, want 409", w.Code)
	}
}

// Scoped tokens must not reach another queue's jobs through the new verbs.
func TestRetryReplyRespectScope(t *testing.T) {
	h := newAPI(t)
	created := decode[submitResponse](t,
		h.do(t, "POST", "/v1/queues/adhoc/jobs", adminToken, submitRequest{Input: "secret"}))
	h.st.Claim(t.Context(), "adhoc", time.Now())
	h.st.Finish(t.Context(), created.ID, store.Result{
		Status: store.StatusNeedsInput, SessionID: "s1"}, time.Now())

	for _, path := range []string{"/retry", "/reply"} {
		w := h.do(t, "POST", "/v1/jobs/"+created.ID+path, scopedToken, replyRequest{Input: "x"})
		if w.Code != http.StatusNotFound {
			t.Errorf("%s with a scoped token = %d, want 404", path, w.Code)
		}
	}
}
