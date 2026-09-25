#!/bin/bash
# Probe 6b: is a tool outside the allowlist DENIED, or left hanging?
#
# The whole unattended model rests on this. If a disallowed call blocks waiting
# for a human, every such job burns its full timeout instead of failing fast,
# and --permission-mode/--permission-prompts need rethinking.
set -uo pipefail
out=${1:-/out}/60-permission-denial
mkdir -p "$out"

# Ask for something only Bash can do, while allowing only Read.
prompt="Run the shell command 'echo hello' using the Bash tool and tell me its output."

start=$(date +%s)
timeout 120 claude -p "$prompt" \
  --output-format stream-json --verbose --model haiku --max-turns 4 \
  --permission-mode dontAsk --permission-prompts none \
  --allowedTools Read \
  > "$out/stream.jsonl" 2> "$out/stderr.txt"
code=$?
end=$(date +%s)
echo "exit=$code elapsed=$((end - start))s" > "$out/exit.txt"

{
  echo "# $(cat "$out/exit.txt")"
  echo "# exit=124 means timeout: the call HUNG rather than being denied."
  echo
  echo "## stderr"
  head -10 "$out/stderr.txt"
  echo
  echo "## Tool uses attempted"
  jq -c 'select(.type=="assistant") | .message.content[]? | select(.type=="tool_use") | {name}' \
    "$out/stream.jsonl" 2>/dev/null
  echo
  echo "## Tool results, including any denial text — this shapes the denial regexes"
  jq -c 'select(.type=="user") | .message.content[]? | select(.type=="tool_result") | {is_error, content}' \
    "$out/stream.jsonl" 2>/dev/null
  echo
  echo "## Any permission-shaped event type we are not modelling"
  jq -r 'select(.type|test("permission|denial";"i")) | .' "$out/stream.jsonl" 2>/dev/null
  jq -r '.type' "$out/stream.jsonl" 2>/dev/null | sort -u
  echo
  echo "## Result line"
  jq -S 'select(.type=="result")' "$out/stream.jsonl" 2>/dev/null
} > "$out/SUMMARY.txt" 2>&1

cat "$out/SUMMARY.txt"
