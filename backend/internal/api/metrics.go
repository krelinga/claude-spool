package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/krelinga/claude-spool/backend/internal/auth"

	"github.com/krelinga/claude-spool/backend/internal/store"
)

// metrics renders the Prometheus text exposition format by hand.
//
// The metric set is the one in design §4. Hand-rolling avoids a client library
// for a handful of gauges read straight out of SQLite on scrape; there are no
// counters that need in-process accumulation yet, because every count Spool
// cares about is already a row in the database.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	ctx := r.Context()

	metric := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	sample := func(name string, labels map[string]string, v any) {
		if len(labels) == 0 {
			fmt.Fprintf(&b, "%s %v\n", name, v)
			return
		}
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, fmt.Sprintf("%s=%q", k, labels[k]))
		}
		fmt.Fprintf(&b, "%s{%s} %v\n", name, strings.Join(pairs, ","), v)
	}

	qs := s.queues()
	names := qs.Names()

	// --- per queue ---
	depths, err := s.st.Depths(ctx)
	if err != nil {
		s.serverError(w, "could not read queue depths", err)
		return
	}
	runtimes, err := s.st.AllQueueRuntime(ctx)
	if err != nil {
		s.serverError(w, "could not read queue runtime", err)
		return
	}

	metric("spool_queue_depth", "Jobs queued per queue.", "gauge")
	for _, n := range names {
		sample("spool_queue_depth", map[string]string{"queue": n}, depths[n])
	}

	metric("spool_queue_paused", "1 when a queue is paused.", "gauge")
	for _, n := range names {
		sample("spool_queue_paused", map[string]string{"queue": n}, boolGauge(runtimes[n].Paused))
	}

	// Oldest queued age is the signal that work is stuck for a reason the
	// depth alone will not show (§4's oldest_queued_age > 2h alert).
	metric("spool_oldest_queued_age_seconds", "Age of the oldest queued job per queue.", "gauge")
	now := s.now()
	for _, n := range names {
		age := 0.0
		if jobs, err := s.st.List(ctx, store.ListFilter{Queue: n, Status: store.StatusQueued, Limit: 200}); err == nil {
			for _, j := range jobs {
				if d := now.Sub(j.CreatedAt).Seconds(); d > age {
					age = d
				}
			}
		}
		sample("spool_oldest_queued_age_seconds", map[string]string{"queue": n}, fmt.Sprintf("%.0f", age))
	}

	// spool_jobs_total is sourced from the jobs table rather than an in-process
	// counter, so it survives a restart. That makes it a gauge-like total: it
	// only decreases when retention prunes rows.
	counts, err := s.st.JobCounts(ctx)
	if err != nil {
		s.serverError(w, "could not read job counts", err)
		return
	}
	metric("spool_jobs_total", "Jobs recorded, by queue, status and error kind.", "counter")
	for _, c := range counts {
		sample("spool_jobs_total", map[string]string{
			"queue": c.Queue, "status": string(c.Status), "error_kind": string(c.ErrorKind),
		}, c.Count)
	}

	// --- shared ---
	exec, err := s.st.ExecutorState(ctx)
	if err != nil {
		s.serverError(w, "could not read executor state", err)
		return
	}
	metric("spool_executor_state", "1 for the executor's current state.", "gauge")
	for _, st := range []store.ExecutorState{
		store.ExecReady, store.ExecBlockedAuth, store.ExecBlockedUsage, store.ExecPaused,
	} {
		sample("spool_executor_state", map[string]string{"state": string(st)}, boolGauge(exec.State == st))
	}

	metric("spool_executor_running", "1 when a job is executing.", "gauge")
	_, _, running := s.exec.Running()
	sample("spool_executor_running", nil, boolGauge(running))

	// --- auth (§4) ---
	if s.auth != nil {
		st := s.auth.Status(ctx)
		metric("spool_auth_state", "1 for the current credential state.", "gauge")
		for _, v := range []auth.State{auth.StateOK, auth.StateExpired, auth.StateUnknown} {
			sample("spool_auth_state", map[string]string{"state": string(v)}, boolGauge(st.State == v))
		}
		if st.SessionAgeSec != nil {
			metric("spool_auth_session_age_seconds",
				"Age of the current login session, since the last completed login.", "gauge")
			sample("spool_auth_session_age_seconds", nil, fmt.Sprintf("%.0f", *st.SessionAgeSec))
		}
		metric("spool_auth_relogins_total", "Completed interactive logins.", "counter")
		sample("spool_auth_relogins_total", nil, st.Relogins)
		metric("spool_auth_login_in_progress", "1 while an interactive login awaits its code.", "gauge")
		sample("spool_auth_login_in_progress", nil, boolGauge(st.LoginInProgress))

		// The observed session lifetimes, which is the measurement the
		// credential-mode decision rests on (§3.2). Exposed as a real histogram
		// so Grafana can chart the distribution rather than a single number.
		if lifetimes, err := s.st.SessionLifetimes(ctx); err == nil {
			metric("spool_auth_session_lifetime_seconds",
				"Observed lifetime of each login session, from login to expiry.", "histogram")
			buckets := []float64{3600, 6 * 3600, 24 * 3600, 3 * 24 * 3600,
				7 * 24 * 3600, 14 * 24 * 3600, 30 * 24 * 3600}
			var sum float64
			counts := make([]int, len(buckets))
			for _, d := range lifetimes {
				secs := d.Seconds()
				sum += secs
				for i, b := range buckets {
					if secs <= b {
						counts[i]++
					}
				}
			}
			for i, b := range buckets {
				sample("spool_auth_session_lifetime_seconds_bucket",
					map[string]string{"le": strconv.FormatFloat(b, 'f', -1, 64)}, counts[i])
			}
			sample("spool_auth_session_lifetime_seconds_bucket",
				map[string]string{"le": "+Inf"}, len(lifetimes))
			sample("spool_auth_session_lifetime_seconds_sum", nil, fmt.Sprintf("%.0f", sum))
			sample("spool_auth_session_lifetime_seconds_count", nil, len(lifetimes))
		}
	}

	outbox, err := s.st.OutboxCounts(ctx)
	if err != nil {
		s.serverError(w, "could not read outbox counts", err)
		return
	}
	metric("spool_webhook_outbox", "Webhook deliveries by state.", "gauge")
	for _, st := range []string{store.OutboxPending, store.OutboxDelivered, store.OutboxDead} {
		sample("spool_webhook_outbox", map[string]string{"state": st}, outbox[st])
	}
	metric("spool_webhook_dead_total", "Deliveries abandoned after the retry window.", "counter")
	sample("spool_webhook_dead_total", nil, outbox[store.OutboxDead])

	if s.broker != nil {
		metric("spool_event_subscribers", "Live SSE subscribers.", "gauge")
		sample("spool_event_subscribers", nil, s.broker.Subscribers())
	}

	metric("spool_build_info", "Build and configuration facts.", "gauge")
	sample("spool_build_info", map[string]string{
		"credential_mode": string(s.cfg.Claude.CredentialMode),
	}, 1)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

func boolGauge(b bool) int {
	if b {
		return 1
	}
	return 0
}
