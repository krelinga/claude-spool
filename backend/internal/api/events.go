package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sseKeepalive bounds how long a connection sits silent. Without it an idle
// proxy will close the stream and the client will not know why.
const sseKeepalive = 25 * time.Second

// streamEvents is the live feed of everything Spool emits (§3.7).
//
// Unlike webhooks, this is not filtered by per-queue notify lists: it is the
// "everything" stream for an operator or a phone app that is already open. It
// is also best-effort — a client that cannot keep up misses events rather than
// slowing the executor. The outbox is what guarantees delivery.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	if s.broker == nil {
		writeError(w, http.StatusNotImplemented, "unavailable", "event stream is not enabled")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	tok := tokenFrom(r)

	events, stop := s.broker.Subscribe()
	defer stop()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer would defeat the point.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case env, ok := <-events:
			if !ok {
				return
			}
			// A scoped token must not learn about other queues' jobs here
			// either. Service events carry no queue and go to everyone.
			if env.Queue != "" && (tok == nil || !tok.AllowsQueue(env.Queue)) {
				continue
			}
			b, err := json.Marshal(env)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n", env.Event, env.EventID, b)
			flusher.Flush()
		}
	}
}
