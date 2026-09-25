#!/bin/bash
# Driver for live tests: any queue, against a real claude.ai login, in a
# throwaway container that shares nothing with the longevity experiments.
#
# The queues it serves are generated at `up` from deploy/spool/queues.yaml (or
# $SPOOL_QUEUES): every real queue, plus a read-only <queue>-dryrun twin of each
# made by ./dryrun. So a queue added to the real file is testable here with no
# extra config. `dryrun` uses the twin and writes nothing; `run` uses the real
# queue and writes, after asking.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
name=${SPOOL_CONTAINER:-spool-live-test}
volume=${SPOOL_VOLUME:-spool-live-test-data}
image=spool:live-test
# Empty means the Dockerfile's pin; set it only to try another CLI.
version=${CLAUDE_CODE_VERSION:-}
# Not 8080: that is deploy/real-run's, and the two can be up at once.
port=${SPOOL_PORT:-8081}
base="http://localhost:$port"
src=${SPOOL_QUEUES:-$repo/deploy/spool/queues.yaml}
gen="$here/generated"
token_file="$here/.token"

usage() {
  cat <<'USAGE'
Usage: deploy/live-test/run.sh <command>

  build                     Build the image (CLI pinned in backend/Dockerfile; override with CLAUDE_CODE_VERSION)
  up                        Generate queues and start on port 8081 with its own /data volume
  login                     Log in through Spool's own API (open the URL, paste the code)
  login-manual              Log in with `docker exec` instead
  auth                      Show credential state
  queues                    List the queues being served
  reload                    Regenerate queues from the source file and hot-reload them

  dryrun <queue> "<text>"   Run the read-only twin of <queue>: writes nothing
  run <queue> "<text>"      Run <queue> for real: this WRITES, and asks first
      Either takes --url <link> after the text, for queues with a url arg.
  reply <job> "<text>"      Answer a needs_input job (resumes the same session)

  job <id>                  Show one job in full
  jobs                      Recent jobs
  transcript <id>           The raw stream-json the run produced
  watch                     Live SSE event stream
  logs                      Container logs
  down                      Stop and remove the container (login volume kept)
  clean                     Remove the container, the login volume and the token

Environment: SPOOL_QUEUES (source queues file), SPOOL_PORT, SPOOL_CONTAINER,
SPOOL_VOLUME, CLAUDE_CODE_VERSION, SPOOL_TOKEN, SPOOL_MAX_BUDGET_USD (passed
through at `up`).
USAGE
}

load_token() {
  if [ -n "${SPOOL_TOKEN:-}" ]; then return; fi
  if [ -f "$token_file" ]; then
    SPOOL_TOKEN=$(cat "$token_file")
    return
  fi
  SPOOL_TOKEN=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
  printf '%s' "$SPOOL_TOKEN" > "$token_file"
  chmod 600 "$token_file"
  echo "Generated an API token in deploy/live-test/.token" >&2
}

api() {
  local method=$1 path=$2
  shift 2
  curl -sS -X "$method" "$base$path" \
    -H "Authorization: Bearer $SPOOL_TOKEN" \
    -H 'Content-Type: application/json' "$@"
}

running() { [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" = "true" ]; }

need_up() {
  if ! running; then
    echo "Container '$name' is not running. Run: deploy/live-test/run.sh up" >&2
    exit 1
  fi
  load_token
}

generate() {
  mkdir -p "$gen"
  (cd "$here/dryrun" && go run . "$src" "$gen/queues.yaml") || exit 1
}

warn_if_blocked() {
  local state
  state=$(api GET /v1/executor | jq -r '.state // empty')
  if [ "$state" != "ready" ]; then
    echo "NOTE: the executor is '$state'. The job will queue but not run yet." >&2
    [ "$state" = "blocked_auth" ] && echo "      Run: deploy/live-test/run.sh login" >&2
  fi
}

# The parts of a finished job worth reading first; `job <id>` has the rest.
report() {
  jq '{id, queue, status, summary, question: .outcome.question, would_write: .outcome.would_write,
       outcome: (.outcome // {} | del(.question, .would_write, .summary, .status)),
       cost_usd, num_turns, tool_calls, permission_denials, error_kind, error_message}
      | with_entries(select(.value != null and .value != {} and .value != []))'
}

wait_for() {
  local id=$1 status=""
  echo "Waiting for job $id ..." >&2
  for _ in $(seq 1 360); do
    status=$(api GET "/v1/jobs/$id" | jq -r .status)
    case "$status" in
      succeeded|failed|needs_input|cancelled|interrupted) break ;;
    esac
    sleep 2
  done
  api GET "/v1/jobs/$id" | report
  echo
  echo "Full job:   deploy/live-test/run.sh job $id"
  echo "Transcript: deploy/live-test/run.sh transcript $id"
  [ "$status" = "needs_input" ] && echo "Answer:     deploy/live-test/run.sh reply $id \"<answer>\""
  return 0
}

# submit <queue> <text> [--url <link>]
submit() {
  local queue=$1 text=$2
  shift 2
  local url=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --url) url=${2:?--url needs a value}; shift 2 ;;
      *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
  done
  warn_if_blocked
  local body resp id
  body=$(jq -n --arg input "$text" --arg url "$url" \
    '{input: $input} + (if $url == "" then {} else {args: {url: $url}} end)')
  resp=$(api POST "/v1/queues/$queue/jobs" -d "$body")
  id=$(echo "$resp" | jq -r '.id // empty')
  if [ -z "$id" ]; then
    echo "$resp" | jq . >&2
    exit 1
  fi
  wait_for "$id"
}

cmd=${1:-}
case "$cmd" in
  build)
    # Its own tag, so rebuilding for a test never changes the image the
    # longevity experiments' commands run.
    docker build ${version:+--build-arg "CLAUDE_CODE_VERSION=$version"} -t "$image" "$repo/backend"
    ;;
  up)
    load_token
    generate
    docker rm -f "$name" >/dev/null 2>&1
    # The login is created inside the container and lives in its own volume:
    # never copied in, which would make a second holder of a refresh token that
    # the spike or real-run container already holds (design §3.2).
    docker run -d --name "$name" \
      -p "$port:8080" \
      -v "$volume:/data" \
      -v "$here/config.yaml:/etc/spool/config.yaml:ro" \
      -v "$gen:/etc/spool/generated:ro" \
      -e "SPOOL_TOKEN=$SPOOL_TOKEN" \
      ${SPOOL_MAX_BUDGET_USD:+-e "SPOOL_MAX_BUDGET_USD=$SPOOL_MAX_BUDGET_USD"} \
      "$image" spool --config /etc/spool/config.yaml --queues /etc/spool/generated/queues.yaml >/dev/null
    for _ in $(seq 1 60); do
      curl -sf "$base/healthz" >/dev/null 2>&1 && break
      sleep 0.5
    done
    if ! curl -sf "$base/healthz" >/dev/null 2>&1; then
      echo "Did not become healthy. Logs:" >&2
      docker logs "$name" 2>&1 | tail -30 >&2
      exit 1
    fi
    echo "Up on $base. Auth: $(api GET /v1/auth | jq -r .state)"
    echo "Next, if not logged in: deploy/live-test/run.sh login"
    ;;
  login)
    need_up
    start=$(api POST /v1/auth/login)
    attempt=$(echo "$start" | jq -r '.attempt_id // empty')
    url=$(echo "$start" | jq -r '.url // empty')
    if [ -z "$attempt" ] || [ -z "$url" ]; then
      echo "Could not start a login:" >&2
      echo "$start" | jq . >&2
      exit 1
    fi
    echo "1. Open this URL, approve, and copy the code it shows:"
    echo
    echo "   $url"
    echo
    printf '2. Paste the code here: '
    read -r code
    api POST "/v1/auth/login/$attempt" -d "$(jq -n --arg code "$code" '{code: $code}')" | jq .
    ;;
  login-manual)
    need_up
    docker exec -it "$name" claude auth login
    ;;
  auth)    need_up; api GET /v1/auth | jq . ;;
  queues)  need_up; api GET /v1/queues | jq -r '.queues[] | "\(.name)\t\(.description // "")"' | column -t -s $'\t' ;;
  reload)
    need_up
    generate
    # The directory is bind-mounted, so the container already sees the new
    # file; SIGHUP makes it reload now rather than on its next poll.
    docker kill -s HUP "$name" >/dev/null && echo "Reloaded."
    ;;
  dryrun)
    [ $# -lt 3 ] && { usage >&2; exit 1; }
    need_up
    submit "$2-dryrun" "${@:3}"
    ;;
  run)
    [ $# -lt 3 ] && { usage >&2; exit 1; }
    need_up
    printf 'This runs the real %s queue and WRITES. Continue? [y/N] ' "$2"
    read -r confirm
    [ "$confirm" = "y" ] || [ "$confirm" = "Y" ] || { echo "Not submitted."; exit 0; }
    submit "$2" "${@:3}"
    ;;
  reply)
    [ $# -lt 3 ] && { usage >&2; exit 1; }
    need_up
    resp=$(api POST "/v1/jobs/$2/reply" -d "$(jq -n --arg input "$3" '{input: $input}')")
    id=$(echo "$resp" | jq -r '.id // empty')
    [ -z "$id" ] && { echo "$resp" | jq . >&2; exit 1; }
    wait_for "$id"
    ;;
  job)        need_up; api GET "/v1/jobs/${2:?job id required}" | jq . ;;
  jobs)       need_up; api GET "/v1/jobs?limit=20" | jq -r '.jobs[] | [.id, .queue, .status, (.summary // "")] | @tsv' | column -t -s $'\t' ;;
  transcript) need_up; api GET "/v1/jobs/${2:?job id required}/transcript" ;;
  watch)      need_up; curl -sN "$base/v1/events" -H "Authorization: Bearer $SPOOL_TOKEN" ;;
  logs)       docker logs "$name" 2>&1 | tail -"${2:-100}" ;;
  down)       docker rm -f "$name" >/dev/null 2>&1 && echo "Removed $name (login volume kept)." ;;
  clean)
    docker rm -f "$name" >/dev/null 2>&1
    docker volume rm "$volume" >/dev/null 2>&1
    rm -f "$token_file"
    rm -rf "$gen"
    echo "Removed the container, its volume, the token and the generated queues."
    ;;
  ""|-h|--help|help) usage ;;
  *) echo "Unknown command: $cmd" >&2; usage; exit 1 ;;
esac
