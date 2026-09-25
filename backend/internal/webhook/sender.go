// Package webhook delivers events from the transactional outbox.
//
// The sender is deliberately separate from the executor: a receiver that is
// down must never hold up a job, so the executor only ever writes an outbox row
// and moves on (design §3.7, §4).
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/krelinga/claude-spool/backend/internal/config"
	"github.com/krelinga/claude-spool/backend/internal/store"
)

const (
	// Signature headers. A receiver verifies the body against the timestamp to
	// reject replays.
	SignatureHeader = "X-Spool-Signature"
	TimestampHeader = "X-Spool-Timestamp"
	EventHeader     = "X-Spool-Event"
	DeliveryHeader  = "X-Spool-Delivery"

	deliveryTimeout = 15 * time.Second
	idlePoll        = 30 * time.Second
	batchSize       = 20
)

type Sender struct {
	cfg  *config.Config
	st   *store.Store
	log  *slog.Logger
	http *http.Client
	now  func() time.Time

	wake chan struct{}
	// secrets maps a receiver URL to its signing secret. Outbox rows store the
	// URL, so this is how a delivery finds its key at send time.
	secrets map[string]string
}

func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Sender {
	secrets := map[string]string{}
	for i := range cfg.Webhooks {
		secrets[cfg.Webhooks[i].URL] = cfg.Webhooks[i].HMACSecret()
	}
	return &Sender{
		cfg: cfg, st: st, log: log,
		http:    &http.Client{Timeout: deliveryTimeout},
		now:     time.Now,
		wake:    make(chan struct{}, 1),
		secrets: secrets,
	}
}

// Wake nudges the loop when something has just been enqueued.
func (s *Sender) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run delivers until ctx is cancelled.
func (s *Sender) Run(ctx context.Context) error {
	if len(s.cfg.Webhooks) == 0 {
		s.log.Info("no webhook receivers configured; sender idle")
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		wait, err := s.step(ctx)
		if err != nil {
			s.log.Error("webhook sender step failed", "error", err)
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
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Sender) step(ctx context.Context) (time.Duration, error) {
	due, err := s.st.ClaimDueDeliveries(ctx, s.now(), batchSize)
	if err != nil {
		return idlePoll, err
	}
	if len(due) == 0 {
		// Sleep until the next retry is due rather than polling.
		next, ok, err := s.st.NextDeliveryDue(ctx)
		if err != nil {
			return idlePoll, err
		}
		if !ok {
			return idlePoll, nil
		}
		if d := next.Sub(s.now()); d > 0 {
			return min(d, idlePoll), nil
		}
		return 0, nil
	}
	for _, e := range due {
		if ctx.Err() != nil {
			return 0, nil
		}
		s.deliver(ctx, e)
	}
	return 0, nil
}

func (s *Sender) deliver(ctx context.Context, e store.OutboxEntry) {
	log := s.log.With("delivery", e.ID, "event", e.Event, "url", e.URL, "attempt", e.Attempts+1)

	err := s.post(ctx, e)
	if err == nil {
		if err := s.st.MarkDelivered(ctx, e.ID); err != nil {
			log.Error("could not mark delivery delivered", "error", err)
			return
		}
		log.Info("webhook delivered")
		return
	}

	// The retry window is measured from the row's first scheduling, so a
	// receiver that is down for a day does not retry forever (§3.7).
	next := s.now().Add(backoff(e.Attempts + 1))
	if e.Attempts+1 >= maxAttempts(s.cfg.WebhookRetryWindow.Duration()) {
		if err := s.st.MarkDead(ctx, e.ID); err != nil {
			log.Error("could not mark delivery dead", "error", err)
			return
		}
		log.Error("webhook gave up after the retry window", "error", err)
		return
	}
	if err := s.st.RescheduleDelivery(ctx, e.ID, next); err != nil {
		log.Error("could not reschedule delivery", "error", err)
		return
	}
	log.Warn("webhook delivery failed; will retry", "error", err, "next_at", next)
}

func (s *Sender) post(ctx context.Context, e store.OutboxEntry) error {
	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(e.Payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	ts := strconv.FormatInt(s.now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "spool/1")
	req.Header.Set(EventHeader, e.Event)
	req.Header.Set(DeliveryHeader, strconv.FormatInt(e.ID, 10))
	req.Header.Set(TimestampHeader, ts)
	if secret, ok := s.secrets[e.URL]; ok {
		req.Header.Set(SignatureHeader, Sign(secret, ts, e.Payload))
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Read and discard so the connection can be reused.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("receiver returned %s: %s", resp.Status, truncate(string(body), 200))
	}
	return nil
}

// Sign computes the signature a receiver checks: HMAC-SHA256 over
// "<timestamp>.<body>", hex encoded and prefixed with the scheme version.
//
// The timestamp is inside the signed material so a captured delivery cannot be
// replayed later against a receiver that enforces a freshness window.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify is the check a receiver performs. It lives here so the shape is
// documented in one place and testable.
func Verify(secret, timestamp, signature string, body []byte) bool {
	return hmac.Equal([]byte(Sign(secret, timestamp, body)), []byte(signature))
}

// backoff grows exponentially and then holds at an hour, so a receiver that is
// down for a long time is retried steadily rather than ever more rarely.
func backoff(attempt int) time.Duration {
	const base = 10 * time.Second
	const cap = time.Hour
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	if d > cap || d <= 0 {
		return cap
	}
	return d
}

// maxAttempts is how many tries fit in the retry window, given backoff.
func maxAttempts(window time.Duration) int {
	var total time.Duration
	for n := 1; n < 1000; n++ {
		total += backoff(n)
		if total >= window {
			return n
		}
	}
	return 1000
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
