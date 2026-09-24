#!/bin/bash
# Probe 3: does structured output appear on the stream-json result line, and
# does --json-schema take a file path or inline JSON?
#
# This is the single most load-bearing unknown: internal/claudecli.ExtractOutcome
# guesses four field names and then falls back to parsing a fenced JSON block.
set -uo pipefail
out=${1:-/out}/30-structured-output
mkdir -p "$out"

cat > "$out/schema.json" <<'SCHEMA'
{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["succeeded", "failed", "needs_input"]},
    "summary": {"type": "string"},
    "question": {"type": "string"},
    "links": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["status", "summary"]
}
SCHEMA

prompt="Report that you succeeded at the task 'spike test', with a one line summary."

# Variant A: a file path, which is what Spool currently passes.
claude -p "$prompt" --output-format stream-json --verbose --model haiku --max-turns 1 \
  --json-schema "$out/schema.json" \
  > "$out/a-file-path.jsonl" 2> "$out/a-file-path.stderr"
echo "exit=$?" > "$out/a-file-path.exit"

# Variant B: inline JSON.
claude -p "$prompt" --output-format stream-json --verbose --model haiku --max-turns 1 \
  --json-schema "$(cat "$out/schema.json")" \
  > "$out/b-inline.jsonl" 2> "$out/b-inline.stderr"
echo "exit=$?" > "$out/b-inline.exit"

# Variant C: no schema at all, as a control.
claude -p "$prompt" --output-format stream-json --verbose --model haiku --max-turns 1 \
  > "$out/c-no-schema.jsonl" 2> "$out/c-no-schema.stderr"
echo "exit=$?" > "$out/c-no-schema.exit"

{
  for v in a-file-path b-inline c-no-schema; do
    echo "=============================================================="
    echo "# Variant: $v  ($(cat "$out/$v.exit"))"
    echo
    echo "## stderr"
    head -5 "$out/$v.stderr"
    echo
    echo "## Every key on the result line — look for the structured output field"
    jq -r 'select(.type=="result") | keys[]' "$out/$v.jsonl" 2>/dev/null
    echo
    echo "## The result line itself, pretty-printed"
    jq -S 'select(.type=="result")' "$out/$v.jsonl" 2>/dev/null
    echo
  done
  echo "=============================================================="
  echo "# Which of the field names Spool guesses actually appear?"
  for key in structured_output structuredOutput structured_result structuredResult; do
    if grep -q "\"$key\"" "$out"/*.jsonl 2>/dev/null; then
      printf '%-22s PRESENT\n' "$key"
    else
      printf '%-22s absent\n' "$key"
    fi
  done
} > "$out/SUMMARY.txt" 2>&1

cat "$out/SUMMARY.txt"
