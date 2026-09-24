#!/bin/bash
# Probes 1 and 2: what does system/init actually report?
#
# Answers: are the Notion connector tools present and what are their exact
# names; do synced skills appear, and under what name. These feed a queue's
# allowed_tools and requires blocks, and internal/claudecli MissingCapabilities.
set -uo pipefail
out=${1:-/out}/10-init
mkdir -p "$out"

# The cheapest real request that still produces a full init event.
claude -p "reply with the single word ok" \
  --output-format stream-json --verbose \
  --model haiku --max-turns 1 \
  > "$out/stream.jsonl" 2> "$out/stderr.txt"
echo "exit=$?" > "$out/exit.txt"

jq -c 'select(.type=="system")' "$out/stream.jsonl" > "$out/system-events.jsonl" 2>/dev/null || true
jq -S . "$out/system-events.jsonl" > "$out/init-pretty.json" 2>/dev/null || true

{
  echo "# Raw init event"
  cat "$out/init-pretty.json"
  echo
  echo "# tools[]"
  jq -r 'select(.subtype=="init") | .tools[]?' "$out/stream.jsonl" 2>/dev/null
  echo
  echo "# mcp_servers[] — exact names and statuses for queue requires.connectors"
  jq -c 'select(.subtype=="init") | .mcp_servers[]?' "$out/stream.jsonl" 2>/dev/null
  echo
  echo "# slash_commands[] — where synced skills should appear"
  jq -r 'select(.subtype=="init") | .slash_commands[]?' "$out/stream.jsonl" 2>/dev/null
  echo
  echo "# Any mcp__ tool names — copy these verbatim into allowed_tools"
  grep -o 'mcp__[A-Za-z0-9_-]*' "$out/stream.jsonl" | sort -u
  echo
  echo "# Every top-level key on the init event (in case we are modelling the wrong ones)"
  jq -r 'select(.subtype=="init") | keys[]' "$out/stream.jsonl" 2>/dev/null
} > "$out/SUMMARY.txt" 2>&1

cat "$out/SUMMARY.txt"
