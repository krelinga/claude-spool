#!/bin/bash
# Probe 6a: what does a run with no usable login look like?
#
# Non-destructive by design: it points CLAUDE_CONFIG_DIR at an empty temp dir
# rather than touching the real credentials, so it cannot cost you a re-login.
#
# Caveat worth remembering when reading the output: "never logged in" and
# "login expired" may not produce the same string. This gives the shape of the
# former; only 70-keepalive.sh, running for weeks, catches the latter for real.
set -uo pipefail
out=${1:-/out}/50-auth-failure
mkdir -p "$out"

empty=$(mktemp -d)
trap 'rm -rf "$empty"' EXIT

CLAUDE_CONFIG_DIR="$empty" claude auth status \
  > "$out/status.txt" 2> "$out/status.stderr"
echo "exit=$?" >> "$out/status.txt"

CLAUDE_CONFIG_DIR="$empty" claude -p "reply ok" \
  --output-format stream-json --verbose --model haiku --max-turns 1 \
  > "$out/stream.jsonl" 2> "$out/stderr.txt"
echo "exit=$?" > "$out/exit.txt"

{
  echo "# claude auth status with no credentials"
  cat "$out/status.txt"
  echo "-- stderr --"
  cat "$out/status.stderr"
  echo
  echo "# A -p run with no credentials: $(cat "$out/exit.txt")"
  echo "-- stderr --"
  cat "$out/stderr.txt"
  echo
  echo "-- stream (in full; it is usually short) --"
  cat "$out/stream.jsonl"
  echo
  echo "# Does Spool's auth regex set match any of this?"
  for pat in "login.*expired" "authentication_failed" "not.*logged.*in" \
             "please.*run./login" "claude auth login" "oauth.*token.*expired" \
             "invalid.*api.*key" "401"; do
    if grep -qiE "$pat" "$out/stderr.txt" "$out/stream.jsonl" "$out/status.txt" 2>/dev/null; then
      printf 'MATCHES  %s\n' "$pat"
    else
      printf 'no match %s\n' "$pat"
    fi
  done
  echo
  echo "# If nothing matched, widen authPatterns in internal/claudecli/classify.go"
} > "$out/SUMMARY.txt" 2>&1

cat "$out/SUMMARY.txt"
