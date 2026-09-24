package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/claude-spool-be/internal/config"
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
}

func (f *fakeExec) Wake() { f.woken++ }
func (f *fakeExec) Running() (string, string, bool) {
	return f.running[0], f.running[1], f.isRunning
}

type apiHarness struct {
	srv  http.Handler
	st   *store.Store
	cfg  *config.Config
	exec *fakeExec
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
	s := New(cfg, st, func() *config.QueueSet { return qs }, ex,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &apiHarness{srv: s.Handler(), st: st, cfg: cfg, exec: ex}
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
