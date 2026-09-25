package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/claude-spool/backend/internal/config"
	"github.com/krelinga/claude-spool/backend/internal/store"
)

const secret = "sender-test-secret-0123456789"

type receiver struct {
	mu       sync.Mutex
	requests []captured
	// status is returned to the sender; change it to make deliveries fail.
	status int
	srv    *httptest.Server
}

type captured struct {
	body      []byte
	signature string
	timestamp string
	event     string
	delivery  string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, captured{
			body:      body,
			signature: req.Header.Get(SignatureHeader),
			timestamp: req.Header.Get(TimestampHeader),
			event:     req.Header.Get(EventHeader),
			delivery:  req.Header.Get(DeliveryHeader),
		})
		status := r.status
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) setStatus(s int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = s
}

func (r *receiver) all() []captured {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]captured{}, r.requests...)
}

func newSender(t *testing.T, url string) (*Sender, *store.Store) {
	t.Helper()
	cfg, err := config.Parse([]byte(
		"tokens:\n  - name: a\n    token: \"0123456789abcdef0123\"\n    queues: [\"*\"]\n" +
			"webhooks:\n  - name: test\n    url: " + url + "\n    secret: \"" + secret + "\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func enqueue(t *testing.T, st *store.Store, url string, at time.Time) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"event": "job.succeeded", "event_id": "ev_1"})
	err := st.EnqueueDeliveries(context.Background(), []store.Delivery{{
		Event: "job.succeeded", Queue: "media", JobID: "01A", URL: url, Payload: payload,
	}}, at)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSignAndVerify(t *testing.T) {
	body := []byte(`{"event":"job.succeeded"}`)
	sig := Sign(secret, "1700000000", body)
	if !Verify(secret, "1700000000", sig, body) {
		t.Error("a signature must verify against the same inputs")
	}
	// Each input is covered by the signature.
	if Verify("other-secret-0123456789", "1700000000", sig, body) {
		t.Error("verified with the wrong secret")
	}
	if Verify(secret, "1700000001", sig, body) {
		t.Error("verified with the wrong timestamp — replay protection is broken")
	}
	if Verify(secret, "1700000000", sig, []byte(`{"event":"job.failed"}`)) {
		t.Error("verified with a tampered body")
	}
	// The timestamp is a distinct field, not concatenated ambiguously: moving a
	// digit between timestamp and body must not produce the same signature.
	if Sign(secret, "17", []byte("00.x")) == Sign(secret, "1700", []byte("x")) {
		t.Error("signature is ambiguous across the timestamp/body boundary")
	}
}

func TestDeliverSuccess(t *testing.T) {
	r := newReceiver(t)
	s, st := newSender(t, r.srv.URL)
	enqueue(t, st, r.srv.URL, time.Now())

	if _, err := s.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}

	got := r.all()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	c := got[0]
	if c.event != "job.succeeded" {
		t.Errorf("event header = %q", c.event)
	}
	if c.delivery == "" {
		t.Error("delivery id header missing")
	}
	if !Verify(secret, c.timestamp, c.signature, c.body) {
		t.Errorf("signature did not verify: %q", c.signature)
	}

	// Delivered rows are not retried.
	counts, _ := st.OutboxCounts(context.Background())
	if counts[store.OutboxDelivered] != 1 {
		t.Errorf("outbox counts = %v", counts)
	}
	if _, err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 1 {
		t.Error("a delivered event was sent again")
	}
}

func TestRetryOnFailure(t *testing.T) {
	r := newReceiver(t)
	r.setStatus(http.StatusInternalServerError)
	s, st := newSender(t, r.srv.URL)
	enqueue(t, st, r.srv.URL, time.Now())

	if _, err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 1 {
		t.Fatalf("receiver got %d requests", len(r.all()))
	}
	counts, _ := st.OutboxCounts(context.Background())
	if counts[store.OutboxPending] != 1 {
		t.Errorf("a failed delivery should stay pending: %v", counts)
	}

	// It is scheduled into the future, so an immediate step does nothing.
	if _, err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 1 {
		t.Error("failed delivery retried immediately instead of backing off")
	}

	// Once the backoff has passed and the receiver recovers, it goes through.
	r.setStatus(http.StatusOK)
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := s.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 2 {
		t.Fatalf("receiver got %d requests after backoff", len(r.all()))
	}
	counts, _ = st.OutboxCounts(context.Background())
	if counts[store.OutboxDelivered] != 1 {
		t.Errorf("outbox counts = %v", counts)
	}
}

// A retry reuses the event_id so a receiver can dedupe.
func TestRetryReusesEventID(t *testing.T) {
	r := newReceiver(t)
	r.setStatus(http.StatusBadGateway)
	s, st := newSender(t, r.srv.URL)
	enqueue(t, st, r.srv.URL, time.Now())

	s.step(context.Background())
	r.setStatus(http.StatusOK)
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	s.step(context.Background())

	got := r.all()
	if len(got) != 2 {
		t.Fatalf("got %d requests", len(got))
	}
	var a, b map[string]any
	json.Unmarshal(got[0].body, &a)
	json.Unmarshal(got[1].body, &b)
	if a["event_id"] != b["event_id"] || a["event_id"] == nil {
		t.Errorf("event_id changed across retries: %v vs %v", a["event_id"], b["event_id"])
	}
	if got[0].delivery != got[1].delivery {
		t.Errorf("delivery id changed across retries: %q vs %q", got[0].delivery, got[1].delivery)
	}
}

// After the retry window the delivery is abandoned rather than retried forever.
func TestGivesUpAfterRetryWindow(t *testing.T) {
	r := newReceiver(t)
	r.setStatus(http.StatusInternalServerError)
	s, st := newSender(t, r.srv.URL)
	enqueue(t, st, r.srv.URL, time.Now())

	base := time.Now()
	// Walk forward past the window; each step is one attempt.
	for i := range maxAttempts(24*time.Hour) + 2 {
		s.now = func() time.Time { return base.Add(time.Duration(i) * 2 * time.Hour) }
		if _, err := s.step(context.Background()); err != nil {
			t.Fatal(err)
		}
		counts, _ := st.OutboxCounts(context.Background())
		if counts[store.OutboxDead] == 1 {
			return
		}
	}
	counts, _ := st.OutboxCounts(context.Background())
	t.Errorf("delivery never marked dead: %v", counts)
}

func TestNon2xxIsFailure(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusNotFound, http.StatusInternalServerError} {
		r := newReceiver(t)
		r.setStatus(status)
		s, st := newSender(t, r.srv.URL)
		enqueue(t, st, r.srv.URL, time.Now())
		s.step(context.Background())
		counts, _ := st.OutboxCounts(context.Background())
		if counts[store.OutboxPending] != 1 {
			t.Errorf("status %d was treated as success", status)
		}
	}
}

// 2xx other than 200 counts as accepted: 202 is the natural answer for a
// receiver that queues the notification itself.
func TestAccepted2xx(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		r := newReceiver(t)
		r.setStatus(status)
		s, st := newSender(t, r.srv.URL)
		enqueue(t, st, r.srv.URL, time.Now())
		s.step(context.Background())
		counts, _ := st.OutboxCounts(context.Background())
		if counts[store.OutboxDelivered] != 1 {
			t.Errorf("status %d was not treated as delivered: %v", status, counts)
		}
	}
}

func TestUnreachableReceiverRetries(t *testing.T) {
	// Nothing listening on this port.
	s, st := newSender(t, "http://127.0.0.1:1/hook")
	enqueue(t, st, "http://127.0.0.1:1/hook", time.Now())
	if _, err := s.step(context.Background()); err != nil {
		t.Fatalf("a dead receiver must not fail the sender: %v", err)
	}
	counts, _ := st.OutboxCounts(context.Background())
	if counts[store.OutboxPending] != 1 {
		t.Errorf("outbox counts = %v", counts)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	prev := time.Duration(0)
	for n := 1; n <= 20; n++ {
		d := backoff(n)
		if d < prev {
			t.Errorf("backoff(%d) = %v, went backwards from %v", n, d, prev)
		}
		if d > time.Hour {
			t.Errorf("backoff(%d) = %v, exceeds the cap", n, d)
		}
		prev = d
	}
	if backoff(1) > time.Minute {
		t.Errorf("first retry waits %v, too long for a transient blip", backoff(1))
	}
}

func TestRunDeliversAndStops(t *testing.T) {
	r := newReceiver(t)
	s, st := newSender(t, r.srv.URL)
	enqueue(t, st, r.srv.URL, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for len(r.all()) == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("Run never delivered")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not stop on cancel")
	}
}
