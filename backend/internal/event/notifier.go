package event

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/claude-spool/backend/internal/config"
	"github.com/krelinga/claude-spool/backend/internal/store"
	"github.com/oklog/ulid/v2"
)

// Notifier decides who hears about an event and turns it into outbox rows.
//
// Routing (§3.7): a job event reaches every receiver only if the job's queue
// subscribes to it via its notify list. A service event concerns the shared
// machinery and always reaches every receiver.
type Notifier struct {
	cfg    *config.Config
	queues func() *config.QueueSet
	broker *Broker
	now    func() time.Time
}

func NewNotifier(cfg *config.Config, queues func() *config.QueueSet, broker *Broker) *Notifier {
	return &Notifier{cfg: cfg, queues: queues, broker: broker, now: time.Now}
}

// NewID mints an event_id. Retries of the same delivery reuse it, so a receiver
// can dedupe.
func (n *Notifier) NewID() string { return "ev_" + strings.ToLower(ulid.Make().String()) }

// Envelope builds an envelope with an id and timestamp filled in.
func (n *Notifier) Envelope(t Type) Envelope {
	return Envelope{Event: t, EventID: n.NewID(), At: n.now().UTC()}
}

// JobEnvelope builds the envelope for a job reaching a terminal state. It
// returns false when the status emits no event.
func (n *Notifier) JobEnvelope(j *store.Job) (Envelope, bool) {
	t, ok := ForStatus(j.Status)
	if !ok {
		return Envelope{}, false
	}
	return FromJob(t, j, n.now(), n.NewID()), true
}

// Deliveries returns the outbox rows an envelope produces. It publishes to the
// SSE stream as a side effect, so every event is observable live even when no
// webhook subscribes to it.
func (n *Notifier) Deliveries(env Envelope) []store.Delivery {
	n.publish(env)

	if !env.Event.IsService() {
		q, ok := n.queues().Get(env.Queue)
		if !ok || !slices.Contains(q.Notify, string(env.Event)) {
			return nil
		}
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	var out []store.Delivery
	for i := range n.cfg.Webhooks {
		w := &n.cfg.Webhooks[i]
		if !w.Wants(string(env.Event)) {
			continue
		}
		out = append(out, store.Delivery{
			Event: string(env.Event), Queue: env.Queue, JobID: env.JobID,
			URL: w.URL, Payload: payload,
		})
	}
	return out
}

func (n *Notifier) publish(env Envelope) {
	if n.broker != nil {
		n.broker.Publish(env)
	}
}

// Broker fans events out to live SSE subscribers.
//
// Delivery is best-effort on purpose: a subscriber that cannot keep up loses
// events rather than blocking the executor. Durable delivery is the outbox's
// job, not this one's.
type Broker struct {
	mu   sync.Mutex
	subs map[int]chan Envelope
	next int
}

func NewBroker() *Broker { return &Broker{subs: map[int]chan Envelope{}} }

// Subscribe returns a channel of events and a function to stop listening.
func (b *Broker) Subscribe() (<-chan Envelope, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Envelope, 32)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

func (b *Broker) Publish(env Envelope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- env:
		default:
			// Subscriber is behind; drop rather than stall the caller.
		}
	}
}

// Subscribers reports the current listener count, for /metrics.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
