#!/bin/bash
# Probes 5 and part of 2: what does `claude auth status` report, is it
# machine-readable, and is the "expires in N days" warning exposed anywhere?
#
# Feeds the auth manager (§7 step 3): the probe, the expiry warning, and the
# auth_events cadence measurement all depend on this.
set -uo pipefail
out=${1:-/out}/40-auth-status
mkdir -p "$out"

for variant in "" "--json" "--output-format json"; do
  name="plain"; [ -n "$variant" ] && name=$(echo "$variant" | tr -d ' -')
  # shellcheck disable=SC2086
  claude auth status $variant > "$out/status-$name.txt" 2> "$out/status-$name.stderr"
  echo "exit=$?" >> "$out/status-$name.txt"
done

# Does it touch the network, or only read the credential file? Timing is a
# rough proxy; a slow call is talking to the server.
start=$(date +%s%N)
claude auth status > /dev/null 2>&1
end=$(date +%s%N)
echo "$(( (end - start) / 1000000 ))ms" > "$out/timing.txt"

# Is there an expiry warning anywhere in a normal run's output?
claude -p "reply ok" --output-format stream-json --verbose --model haiku --max-turns 1 \
  > "$out/run-stream.jsonl" 2> "$out/run-stderr.txt"

{
  echo "# claude auth status — plain"
  cat "$out/status-plain.txt"
  echo
  echo "# Does it accept a JSON flag?"
  for f in json outputformatjson; do
    echo "-- $f --"
    cat "$out/status-$f.txt" 2>/dev/null
    head -3 "$out/status-$f.stderr" 2>/dev/null
  done
  echo
  echo "# auth status latency (high suggests it validates with the server)"
  cat "$out/timing.txt"
  echo
  echo "# Anything resembling an expiry warning, in status or in a run"
  grep -ri -E "expir|renew|days|/login" "$out/status-plain.txt" "$out/run-stderr.txt" \
    "$out/run-stream.jsonl" 2>/dev/null | head -20 || echo "(nothing found)"
} > "$out/SUMMARY.txt" 2>&1

cat "$out/SUMMARY.txt"
