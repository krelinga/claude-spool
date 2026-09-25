#!/bin/bash
# Probe 7: the longevity test. Leave this running for weeks.
#
# Two jobs at once:
#   - measures the real re-auth cadence, which is the whole point of the
#     auth_events table and the claudeai_login vs long_lived_token decision;
#   - captures real failure shapes (expiry, usage limits) as they happen,
#     which probe 50 can only approximate.
#
# Usage: 70-keepalive.sh /out [interval_seconds]
# Default interval is 4h, matching the design's keep-alive (§3.2). Run a second
# container WITHOUT this script to see whether idleness alone kills a session.
set -uo pipefail
out=${1:-/out}/70-keepalive
interval=${2:-14400}
mkdir -p "$out/failures"
log="$out/log.jsonl"

echo "keepalive starting, interval ${interval}s, logging to $log" >&2

while true; do
  ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  epoch=$(date +%s)

  status_out=$(claude auth status 2>&1)
  status_exit=$?
  # auth status is JSON; pull out the fields that measure the cadence rather
  # than embedding the whole blob in every line.
  logged_in=$(printf '%s' "$status_out" | jq -r '.loggedIn // false' 2>/dev/null || echo false)
  auth_method=$(printf '%s' "$status_out" | jq -r '.authMethod // ""' 2>/dev/null || echo "")

  stream=$(mktemp)
  start=$(date +%s%N)
  claude -p "reply with the single word ok" \
    --output-format stream-json --verbose --model haiku --max-turns 3 \
    > "$stream" 2> "$stream.err"
  run_exit=$?
  latency=$(( ($(date +%s%N) - start) / 1000000 ))

  is_error=$(jq -r 'select(.type=="result") | .is_error' "$stream" 2>/dev/null | head -1)
  [ -z "$is_error" ] && is_error=null
  terminal=$(jq -r 'select(.type=="result") | .terminal_reason // ""' "$stream" 2>/dev/null | head -1)
  result_text=$(jq -r 'select(.type=="result") | .result // ""' "$stream" 2>/dev/null | head -c 300)

  # One line per check, so the cadence can be plotted later.
  jq -n -c \
    --arg ts "$ts" --argjson epoch "$epoch" \
    --argjson logged_in "${logged_in:-false}" --arg auth_method "$auth_method" \
    --argjson status_exit "$status_exit" --argjson run_exit "$run_exit" \
    --argjson latency_ms "$latency" --argjson is_error "$is_error" \
    --arg terminal_reason "$terminal" --arg result "$result_text" \
    '{ts:$ts, epoch:$epoch, logged_in:$logged_in, auth_method:$auth_method,
      status_exit:$status_exit, run_exit:$run_exit, latency_ms:$latency_ms,
      is_error:$is_error, terminal_reason:$terminal_reason, result:$result}' \
    >> "$log" 2>/dev/null || \
    echo "{\"ts\":\"$ts\",\"note\":\"log encode failed\",\"run_exit\":$run_exit}" >> "$log"

  # Keep the full stream whenever anything went wrong: these are the real
  # error shapes the classifier needs.
  if [ "$run_exit" -ne 0 ] || [ "$status_exit" -ne 0 ]; then
    cp "$stream" "$out/failures/$epoch-stream.jsonl" 2>/dev/null
    cp "$stream.err" "$out/failures/$epoch-stderr.txt" 2>/dev/null
    printf '%s\n' "$status_out" > "$out/failures/$epoch-auth-status.txt"
    # The first failure is the interesting one; note it loudly in the log too.
    echo "{\"ts\":\"$ts\",\"event\":\"failure_captured\",\"epoch\":$epoch}" >> "$log"
    echo "FAILURE captured at $ts (run_exit=$run_exit status_exit=$status_exit)" >&2
  fi

  rm -f "$stream" "$stream.err"
  sleep "$interval"
done
