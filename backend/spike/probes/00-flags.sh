#!/bin/bash
# Probe 0: does the command line Spool builds actually exist?
#
# Needs no login, so run it first.
#
# Tests each flag by INVOKING it, not by grepping --help. That matters: on CLI
# 2.1.282 the help output omits both --max-turns and --append-system-prompt-file
# even though both are accepted. An unknown flag exits 1 with
# "error: unknown option" on stderr, which is the signal we look for.
set -uo pipefail
out=${1:-/out}/00-flags
mkdir -p "$out"

claude --version > "$out/version.txt" 2>&1
claude --help    > "$out/help.txt" 2>&1
claude auth --help > "$out/auth-help.txt" 2>&1 || true

# accepts <flag> [value] -- prints ACCEPTED or REJECTED.
# Without a login every run stops at the auth check, which is *after* argument
# parsing, so "not logged in" means the flag itself was fine.
accepts() {
  local flag=$1 value=${2:-} stderr
  stderr=$(claude -p x "$flag" ${value:+"$value"} 2>&1 >/dev/null)
  if grep -qi "unknown option\|unknown argument" <<<"$stderr"; then
    echo "REJECTED"
  elif grep -qi "invalid\|not valid\|not found\|allowed choices\|choices:" <<<"$stderr"; then
    echo "ACCEPTED-BUT-BAD-VALUE: $(head -1 <<<"$stderr")"
  else
    echo "ACCEPTED"
  fi
}

{
  echo "# CLI version"
  cat "$out/version.txt"
  echo
  echo "# Flags Spool passes, tested by invocation"
  printf '%-36s %s\n' "--output-format stream-json"      "$(accepts --output-format stream-json)"
  printf '%-36s %s\n' "--verbose"                        "$(accepts --verbose)"
  printf '%-36s %s\n' "--permission-mode dontAsk"        "$(accepts --permission-mode dontAsk)"
  printf '%-36s %s\n' "--permission-prompts none"        "$(accepts --permission-prompts none)"
  printf '%-36s %s\n' "--allowedTools Skill"             "$(accepts --allowedTools Skill)"
  printf '%-36s %s\n' "--max-turns 3"                    "$(accepts --max-turns 3)"
  printf '%-36s %s\n' "--model haiku"                    "$(accepts --model haiku)"
  printf '%-36s %s\n' "--resume abc"                     "$(accepts --resume abc)"
  echo
  echo "# --json-schema: inline JSON vs a file path"
  echo '{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}' > "$out/schema.json"
  printf '%-36s %s\n' "inline JSON"  "$(accepts --json-schema "$(cat "$out/schema.json")")"
  printf '%-36s %s\n' "file path"    "$(accepts --json-schema "$out/schema.json")"
  echo "  (On 2.1.282 the file path is REJECTED: '--json-schema is not valid JSON'."
  echo "   Spool therefore passes the schema inline.)"
  echo
  echo "# --append-system-prompt-file: does it exist, and does it read the file?"
  echo "spike test system prompt" > "$out/system.md"
  printf '%-36s %s\n' "existing file" "$(accepts --append-system-prompt-file "$out/system.md")"
  printf '%-36s %s\n' "missing file"  "$(accepts --append-system-prompt-file /nonexistent.md)"
  echo "  (A 'file not found' error on the missing case proves the flag is real"
  echo "   and takes a path, even though --help does not list it.)"
  echo
  echo "# Control: an unknown flag must be REJECTED, or this probe proves nothing"
  printf '%-36s %s\n' "--totally-bogus-flag" "$(accepts --totally-bogus-flag)"
  echo
  echo "# Permission mode choices, from --help"
  grep -A4 -- '--permission-mode' "$out/help.txt"
  echo
  echo "# --permission-prompts semantics, from --help"
  grep -A7 -- '--permission-prompts' "$out/help.txt"
  echo
  echo "# claude auth subcommands"
  cat "$out/auth-help.txt"
} > "$out/SUMMARY.txt" 2>&1

cat "$out/SUMMARY.txt"
