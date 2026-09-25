#!/bin/bash
# Host-side driver for the validation spike (design §6).
#
# Everything the spike learns lands in backend/spike/out/ as raw CLI output. Nothing
# here is summarised or interpreted on the way out: the point is to see what
# the CLI actually does.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
image=spool-spike
name=${SPIKE_CONTAINER:-spool-spike}
version=${CLAUDE_CODE_VERSION:-latest}
out="$here/out"

usage() {
  cat <<'USAGE'
Usage: backend/spike/run.sh <command>

  build            Build the spike image (pins CLAUDE_CODE_VERSION, default latest)
  up               Start the container with a persistent /data/claude volume
  login            Interactive claude.ai login inside the container  <-- you must do this
  capture-login    Same, but records the PTY output for probe 4
  offline          Probes needing no login: flags (probe 0)
  probe            All login-requiring probes: 1, 2, 3, 5, 6a, 6b
  skill <name>     Probe 2b against a specific skill, e.g. notion-media
  keepalive        Start the longevity test in the background (probe 7)
  keepalive-log    Tail the longevity log
  results          List what has been captured
  shell            Shell inside the container
  down             Stop and remove the container (the login volume survives)
  clean            Remove the container AND the login volume

Order: build, up, offline, login, probe, then leave keepalive running.
USAGE
}

running() { [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" = "true" ]; }

# The bracket keeps grep from matching its own command line.
keepalive_running() { docker exec "$name" sh -c 'ps ax | grep -q "[7]0-keepalive.sh"' 2>/dev/null; }

need_up() {
  if ! running; then
    echo "Container '$name' is not running. Run: backend/spike/run.sh up" >&2
    exit 1
  fi
}

# Warn rather than fail: a probe run with a credential override in the
# environment would silently prove the wrong thing.
check_env() {
  for v in ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_OAUTH_TOKEN; do
    if [ -n "${!v:-}" ]; then
      echo "WARNING: $v is set in your shell. It is NOT passed into the container," >&2
      echo "         but do not let it leak in, or skills and connectors will be off." >&2
    fi
  done
}

cmd=${1:-}
case "$cmd" in
  build)
    docker build --build-arg "CLAUDE_CODE_VERSION=$version" -t "$image" "$here"
    ;;
  up)
    check_env
    mkdir -p "$out"
    # The container runs as its own uid, so the bind-mounted output directory
    # has to be writable by it. This is a gitignored scratch directory.
    chmod 0777 "$out"
    docker rm -f "$name" >/dev/null 2>&1
    # The login lives in a named volume so it survives container restarts, and
    # is created inside the container: never copied in from elsewhere, which
    # would make a second holder of the same refresh token (§3.2).
    docker run -d --name "$name" \
      -v spool-spike-claude:/data/claude \
      -v "$out:/out" \
      -v "$here/probes:/probes:ro" \
      "$image"
    echo "Started. CLI version: $(docker exec "$name" claude --version 2>&1)"
    if [ -f "$out/70-keepalive/log.jsonl" ]; then
      echo "NOTE: a longevity run was recorded before, and does not survive a"
      echo "      container restart. Restart it with: backend/spike/run.sh keepalive"
    fi
    echo "Next: backend/spike/run.sh offline   (then: login)"
    ;;
  login)
    need_up
    echo "A URL will be printed. Open it, approve, and paste the code back here."
    docker exec -it "$name" claude auth login
    ;;
  capture-login)
    need_up
    # Created inside the container, not on the host: a host-made directory is
    # owned by the host user and the container runs as its own uid, so `script`
    # would fail to write — after you had already completed the login.
    docker exec "$name" mkdir -p /out/04-login-pty
    if ! docker exec "$name" sh -c 'touch /out/04-login-pty/.wtest && rm -f /out/04-login-pty/.wtest'; then
      echo "Cannot write to /out/04-login-pty inside the container." >&2
      exit 1
    fi
    if keepalive_running; then
      # One writer at a time: a keep-alive request racing the login would be
      # exactly the refresh-token race the design avoids (§3.2).
      echo "Pausing the longevity test for the duration of the login."
      docker exec "$name" pkill -f 70-keepalive.sh >/dev/null 2>&1
      restart_keepalive=1
    fi
    echo "Recording the login flow for probe 4 (is the URL on stdout? is the code read from stdin?)."
    docker exec -it "$name" script -q -c "claude auth login" /out/04-login-pty/transcript.txt
    echo
    if [ -s "$out/04-login-pty/transcript.txt" ]; then
      echo "Saved $(wc -c < "$out/04-login-pty/transcript.txt") bytes to backend/spike/out/04-login-pty/transcript.txt"
    else
      echo "WARNING: the transcript is empty — nothing was captured." >&2
    fi
    if [ "${restart_keepalive:-0}" = "1" ]; then
      docker exec "$name" mkdir -p /out/70-keepalive
      docker exec -d "$name" sh -c \
        "nohup bash /probes/70-keepalive.sh /out 14400 >> /out/70-keepalive/nohup.log 2>&1"
      sleep 3
      keepalive_running && echo "Longevity test resumed." || echo "WARNING: longevity test did not resume." >&2
    fi
    ;;
  offline)
    need_up
    docker exec "$name" bash /probes/00-flags.sh /out
    ;;
  probe)
    need_up
    if ! docker exec "$name" claude auth status >/dev/null 2>&1; then
      echo "Not logged in inside the container. Run: backend/spike/run.sh login" >&2
      exit 1
    fi
    for p in 10-init 20-skills 30-structured-output 40-auth-status 50-auth-failure 60-permission-denial; do
      echo
      echo "################ $p ################"
      docker exec "$name" bash "/probes/$p.sh" /out || echo "(probe $p exited non-zero; output kept)"
    done
    echo
    echo "Captured under backend/spike/out/. Hand that directory over for interpretation."
    ;;
  skill)
    need_up
    docker exec "$name" bash /probes/20-skills.sh /out "${2:-}"
    ;;
  keepalive)
    need_up
    if keepalive_running; then
      echo "Longevity test is already running."
      exit 0
    fi
    # The redirect below is evaluated by the shell before the script runs, so
    # the directory has to exist first. It used to be created by the script
    # itself, which meant the whole command failed silently under `exec -d`.
    docker exec "$name" mkdir -p /out/70-keepalive
    docker exec -d "$name" sh -c \
      "nohup bash /probes/70-keepalive.sh /out ${2:-14400} >> /out/70-keepalive/nohup.log 2>&1"
    # `exec -d` reports nothing about what happened, so check rather than claim.
    sleep 3
    if keepalive_running; then
      echo "Longevity test started (interval ${2:-14400}s). Check: backend/spike/run.sh keepalive-status"
      echo "Leave it running for weeks; it also captures real failure shapes."
    else
      echo "Longevity test failed to start. Last output:" >&2
      docker exec "$name" sh -c 'tail -20 /out/70-keepalive/nohup.log' 2>&1 >&2
      exit 1
    fi
    ;;
  keepalive-status)
    need_up
    if keepalive_running; then
      echo "running"
    else
      echo "NOT running (start it with: backend/spike/run.sh keepalive)"
    fi
    if [ -f "$out/70-keepalive/log.jsonl" ]; then
      echo "checks recorded: $(wc -l < "$out/70-keepalive/log.jsonl")"
      echo "last check:"
      tail -1 "$out/70-keepalive/log.jsonl"
    else
      echo "no checks recorded yet"
    fi
    if [ -d "$out/70-keepalive/failures" ]; then
      echo "failures captured: $(find "$out/70-keepalive/failures" -name '*-stream.jsonl' | wc -l)"
    fi
    ;;
  keepalive-stop)
    need_up
    docker exec "$name" pkill -f 70-keepalive.sh && echo "stopped." || echo "was not running."
    ;;
  keepalive-log)
    tail -f "$out/70-keepalive/log.jsonl"
    ;;
  results)
    if [ ! -d "$out" ]; then echo "Nothing captured yet."; exit 0; fi
    if [ -f "$out/70-keepalive/log.jsonl" ] && ! keepalive_running; then
      echo "WARNING: the longevity test has stopped. Restart: backend/spike/run.sh keepalive"
      echo
    fi
    find "$out" -name SUMMARY.txt | sort | while read -r f; do
      echo "=== ${f#"$out"/} ==="
    done
    echo
    echo "Full tree:"
    find "$out" -type f | sort | sed "s|$out|spike/out|"
    ;;
  shell)   need_up; docker exec -it "$name" bash ;;
  down)    docker rm -f "$name" >/dev/null 2>&1 && echo "Removed $name (login volume kept)." ;;
  clean)
    docker rm -f "$name" >/dev/null 2>&1
    docker volume rm spool-spike-claude >/dev/null 2>&1
    echo "Removed container and login volume."
    ;;
  ""|-h|--help|help) usage ;;
  *) echo "Unknown command: $cmd" >&2; usage; exit 1 ;;
esac
