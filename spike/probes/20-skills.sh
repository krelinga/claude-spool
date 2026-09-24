#!/bin/bash
# Probe 2b: does a synced skill actually expand under -p?
#
# Pass the skill name to invoke, e.g.:  20-skills.sh /out notion-media
# With no name it lists what is available and stops, which also answers
# "does the notion-media skill exist on this account".
set -uo pipefail
out=${1:-/out}/20-skills
skill=${2:-}
mkdir -p "$out"

claude -p "reply ok" --output-format stream-json --verbose --model haiku --max-turns 1 \
  2>/dev/null | jq -r 'select(.subtype=="init") | .slash_commands[]?' > "$out/available.txt" || true

echo "# Slash commands / skills visible to an unattended -p run" > "$out/SUMMARY.txt"
cat "$out/available.txt" >> "$out/SUMMARY.txt"

if [ -z "$skill" ]; then
  echo >> "$out/SUMMARY.txt"
  echo "# No skill name given; re-run as: 20-skills.sh /out <skill-name>" >> "$out/SUMMARY.txt"
  cat "$out/SUMMARY.txt"
  exit 0
fi

# Does invoking it as a slash command in the prompt expand, or arrive as text?
claude -p "/${skill} this is a spike test, do not create or modify anything, just say what you would do" \
  --output-format stream-json --verbose --model haiku --max-turns 3 \
  > "$out/invoke-stream.jsonl" 2> "$out/invoke-stderr.txt"
echo "exit=$?" > "$out/invoke-exit.txt"

{
  echo
  echo "# Did /${skill} expand? (a Skill tool_use, or the text echoed back verbatim)"
  jq -c 'select(.type=="assistant") | .message.content[]? | select(.type=="tool_use") | {name, input}' \
    "$out/invoke-stream.jsonl" 2>/dev/null
  echo
  echo "# Final result text"
  jq -r 'select(.type=="result") | .result' "$out/invoke-stream.jsonl" 2>/dev/null
} >> "$out/SUMMARY.txt" 2>&1

cat "$out/SUMMARY.txt"
