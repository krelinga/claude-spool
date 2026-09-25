#!/bin/bash
# Driver for the first run against a real claude.ai login and a live Notion.
#
# The order matters. `dryrun` proves the skill loads, the connector is reachable
# and the tool names are right, without writing anything. Only once that passes
# is there any point in `add`, which creates a real Notion page.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
name=${SPOOL_CONTAINER:-spool-real}
image=spool:real
version=${CLAUDE_CODE_VERSION:-2.1.282}
port=${SPOOL_PORT:-8080}
base="http://localhost:$port"

usage() {
  cat <<'USAGE'
Usage: deploy/real-run/run.sh <command>

  build             Build the image (pinned CLAUDE_CODE_VERSION, default 2.1.282)
  up                Start the container on port 8080 with its own /data volume
  login             Log in THROUGH SPOOL's own API (exercises the PTY flow)
  login-manual      Log in with `docker exec` instead (the documented fallback)
  auth              Show credential state
  dryrun "<text>"   Run it past the read-only queue: no writes at all
  add "<text>"      Run it for real: this creates a Notion page
  job <id>          Show one job
  jobs              Recent jobs
  transcript <id>   The raw stream-json the run produced
  watch             Live SSE event stream
  logs              Container logs
  check-idle        Is the never-kept-warm login still alive? (§6 item 7)
  forget-login      Delete the credential, keep the job history
  down              Stop and remove the container (login volume kept)
  clean             Remove the container AND the login volume

A token is required, and is generated on first `up` into deploy/real-run/.token
(gitignored) if you do not set SPOOL_TOKEN yourself.
USAGE
}

token_file="$here/.token"

load_token() {
  if [ -n "${SPOOL_TOKEN:-}" ]; then return; fi
  if [ -f "$token_file" ]; then
    SPOOL_TOKEN=$(cat "$token_file")
    return
  fi
  # 32 hex chars is plenty for a LAN-only personal service.
  SPOOL_TOKEN=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
  printf '%s' "$SPOOL_TOKEN" > "$token_file"
  chmod 600 "$token_file"
  echo "Generated an API token in deploy/real-run/.token" >&2
}

api() {
  local method=$1 path=$2
  shift 2
  curl -sS -X "$method" "$base$path" \
    -H "Authorization: Bearer $SPOOL_TOKEN" \
    -H 'Content-Type: application/json' "$@"
}

pretty() { if command -v jq >/dev/null; then jq .; else cat; fi; }

running() { [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" = "true" ]; }

need_up() {
  if ! running; then
    echo "Container '$name' is not running. Run: deploy/real-run/run.sh up" >&2
    exit 1
  fi
}

# Submitting to a queue that is paused or an executor that is blocked would
# silently sit in the queue; say so instead.
warn_if_blocked() {
  local state
  state=$(api GET /v1/executor | grep -o '"state":"[^"]*"' | head -1 | cut -d'"' -f4)
  if [ "$state" != "ready" ]; then
    echo "NOTE: the executor is '$state'. The job will queue but not run yet." >&2
    if [ "$state" = "blocked_auth" ]; then
      echo "      Run: deploy/real-run/run.sh login" >&2
    fi
  fi
}

submit() {
  local queue=$1 text=$2
  need_up; load_token; warn_if_blocked
  local body
  body=$(printf '%s' "$text" | python3 -c 'import sys;print("{\"input\":" + __import__("json").dumps(sys.stdin.read()) + "}")' 2>/dev/null) \
    || body=$(printf '{"input":"%s"}' "$(printf '%s' "$text" | sed 's/\\/\\\\/g; s/"/\\"/g')")
  local resp id
  resp=$(api POST "/v1/queues/$queue/jobs" -d "$body")
  echo "$resp" | pretty
  id=$(echo "$resp" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
  [ -z "$id" ] && exit 1

  echo
  echo "Waiting for job $id ..."
  local status=""
  for _ in $(seq 1 180); do
    status=$(api GET "/v1/jobs/$id" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
    case "$status" in
      succeeded|failed|needs_input|cancelled|interrupted) break ;;
    esac
    sleep 2
  done
  echo
  api GET "/v1/jobs/$id" | pretty
  echo
  echo "Transcript: deploy/real-run/run.sh transcript $id"
}

cmd=${1:-}
case "$cmd" in
  build)
    docker build --build-arg "CLAUDE_CODE_VERSION=$version" -t "$image" "$repo"
    ;;
  up)
    load_token
    docker rm -f "$name" >/dev/null 2>&1
    # The login is created inside the container and lives in its own volume:
    # never copied in from elsewhere, which would make a second holder of the
    # same refresh token (design §3.2).
    docker run -d --name "$name" \
      -p "$port:8080" \
      -v spool-real-data:/data \
      -v "$here:/etc/spool:ro" \
      -e "SPOOL_TOKEN=$SPOOL_TOKEN" \
      "$image"
    for _ in $(seq 1 60); do
      curl -sf "$base/healthz" >/dev/null 2>&1 && break
      sleep 0.5
    done
    if ! curl -sf "$base/healthz" >/dev/null 2>&1; then
      echo "Did not come up. Logs:" >&2
      docker logs "$name" 2>&1 | tail -20 >&2
      exit 1
    fi
    echo "Up on $base"
    api GET /v1/auth | pretty
    echo
    echo "Next: deploy/real-run/run.sh login"
    ;;
  login)
    need_up; load_token
    echo "Starting a login through Spool's own API (this is the flow a phone would use)."
    start=$(api POST /v1/auth/login)
    echo "$start" | pretty
    url=$(echo "$start" | grep -o '"url":"[^"]*"' | head -1 | cut -d'"' -f4 | sed 's/\\u0026/\&/g')
    attempt=$(echo "$start" | grep -o '"attempt_id":"[^"]*"' | head -1 | cut -d'"' -f4)
    if [ -z "$url" ] || [ -z "$attempt" ]; then
      echo "Could not start a login." >&2
      exit 1
    fi
    echo
    echo "1. Open this URL, approve, and copy the code it shows:"
    echo
    echo "   $url"
    echo
    printf '2. Paste the code here: '
    read -r code
    echo
    api POST "/v1/auth/login/$attempt" -d "$(printf '{"code":"%s"}' "$code")" | pretty
    ;;
  login-manual)
    need_up
    echo "The documented fallback: logging in directly inside the container."
    docker exec -it "$name" claude auth login
    load_token
    api POST /v1/auth/check | pretty
    ;;
  auth)      need_up; load_token; api GET /v1/auth | pretty ;;
  dryrun)
    [ $# -lt 2 ] && { echo "Usage: run.sh dryrun \"<text>\"" >&2; exit 1; }
    submit media-dryrun "$2"
    ;;
  add)
    [ $# -lt 2 ] && { echo "Usage: run.sh add \"<text>\"" >&2; exit 1; }
    echo "This writes to your live Notion database."
    printf 'Type yes to continue: '
    read -r confirm
    [ "$confirm" = "yes" ] || { echo "Cancelled."; exit 0; }
    submit media "$2"
    ;;
  job)       need_up; load_token; api GET "/v1/jobs/${2:?job id required}" | pretty ;;
  jobs)      need_up; load_token; api GET "/v1/jobs?limit=20" | pretty ;;
  transcript)
    need_up; load_token
    api GET "/v1/jobs/${2:?job id required}/transcript"
    ;;
  watch)     need_up; load_token; curl -sN "$base/v1/events" -H "Authorization: Bearer $SPOOL_TOKEN" ;;
  logs)
    # An unquoted empty $2 would be passed to docker as an argument.
    if [ -n "${2:-}" ]; then
      docker logs "$2" "$name" 2>&1 | tail -50
    else
      docker logs "$name" 2>&1 | tail -50
    fi
    ;;
  check-idle)
    # The idleness arm of §6 item 7. This login is never kept warm, while the
    # spike container's is pinged every 4h. Running this after days or weeks
    # answers whether idleness alone ends a session — which decides whether the
    # keep-alive in the design is load-bearing or merely belt and braces.
    #
    # It needs no running container and starts nothing: one `auth status` and one
    # minimal request against the volume.
    echo "Last login recorded in this volume:"
    docker run --rm -v spool-real-data:/data "$image" \
      sh -c 'stat -c "  %y  %n" /data/claude/.credentials.json 2>/dev/null || echo "  (no credential present)"'
    echo
    echo "auth status (local check):"
    docker run --rm -v spool-real-data:/data -e CLAUDE_CONFIG_DIR=/data/claude "$image" \
      claude auth status 2>&1 | sed 's/^/  /'
    echo
    echo "A real request, which is the authoritative check:"
    docker run --rm -v spool-real-data:/data -e CLAUDE_CONFIG_DIR=/data/claude \
      -e DISABLE_AUTOUPDATER=1 "$image" \
      claude -p "reply with the single word ok" --model haiku --max-turns 1 2>&1 | sed 's/^/  /'
    ;;
  forget-login)
    # Removes the credential but keeps the job history and transcripts.
    docker run --rm -v spool-real-data:/data "$image" \
      sh -c 'rm -f /data/claude/.credentials.json' \
      && echo "Credential removed. Job history and transcripts kept; log in again with 'up' then 'login'."
    ;;
  down)      docker rm -f "$name" >/dev/null 2>&1 && echo "Removed $name (login volume kept)." ;;
  clean)
    docker rm -f "$name" >/dev/null 2>&1
    docker volume rm spool-real-data >/dev/null 2>&1
    rm -f "$token_file"
    echo "Removed the container, its volume, and the local token."
    ;;
  ""|-h|--help|help) usage ;;
  *) echo "Unknown command: $cmd" >&2; usage; exit 1 ;;
esac
