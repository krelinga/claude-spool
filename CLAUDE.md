# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status

The §7 **MVP is implemented**: queues from YAML, submit/list/get/cancel, scheduler, executor, classifier, SQLite, bearer tokens, Dockerfile. Not yet built — auth manager (§7 step 3), webhooks/SSE/metrics (step 4), retry/reply and running-job cancel (step 5).

**The §6 validation spike has not been run.** Nothing here has touched a real `claude` binary; tests drive a fake CLI. `docs/design/spike.md` maps each open question to the one place in the tree that changes when it is answered. Do the spike before trusting any of it in anger.

The design doc is the source of truth for behavior.

## Commands

```
go build ./...                       # build
go test ./... -race                  # full suite (~12s; executor tests exercise real timeouts)
go test ./internal/executor -run TestTimeoutKillsRun -v
go vet ./... && gofmt -l .           # lint
go run ./cmd/spool --config deploy/spool/config.yaml --queues deploy/spool/queues.yaml --check
```

`--check` validates both config files and exits — the fastest way to confirm a `queues.yaml` edit. YAML parsing uses `KnownFields(true)`, so an unknown field is an error rather than a silent no-op.

## Layout

`cmd/spool` wires it together. Under `internal/`: `config` (both YAML files, the template renderer, the config hash), `store` (SQLite, job lifecycle), `sched` (weighted round-robin, pure), `claudecli` (**everything** touching the CLI's flags and output), `executor` (the single global runner), `api` (HTTP).

Dependency direction is one-way: `config` and `store` are leaves, `claudecli` imports both, `executor` imports all, `api` imports everything but `executor`'s internals (it sees only a two-method interface).

## Toolchain

**The language is Go.** The design doc listed TypeScript as an equally viable alternative (§3.8); that question is settled — don't reopen it.

The devcontainer (`.devcontainer/devcontainer.json`) provides:

- **Go** (`ghcr.io/devcontainers/features/go:1`, currently `latest`) with `gopls`, `dlv`, `staticcheck`, and `golangci-lint`. Pin to a specific minor once `go.mod` exists so the container and CI agree.
- **docker-in-docker** for building and running the Spool image (§3.8) and for the §6 spike, which needs a bare Debian container with a pinned CLI version.
- **Node LTS**, required by the Claude Code feature — not by Spool itself.
- **`sqlite3` CLI** via `postCreateCommand`, for inspecting `spool.db` and the `.backup` procedure (§4). It is a debugging tool, not a build dependency.

Changing features means editing `devcontainer.json`; `devcontainer-lock.json` updates itself on rebuild and should not be hand-edited.

**Go specifics:** `modernc.org/sqlite` with `CGO_ENABLED=0` for the static binary — don't reach for `mattn/go-sqlite3`. The store opens with `MaxOpenConns(1)`: single-user service, one job at a time, and it removes a class of SQLITE_BUSY races. The `claude auth login` flow will need a PTY (`creack/pty`) when the auth manager lands.

**Subprocess gotcha, learned the hard way:** `cmd.Wait()` closes `StdoutPipe`, so it must not be called until the stdout scanner has drained to EOF. Calling it concurrently truncates the stream and silently loses the `result` line the whole classifier depends on. `executor.execute` drains first, then waits, with signal escalation in a separate watcher.

## What Spool is

A single Docker container holding **one claude.ai login** that runs multiple named queues of prompts against it. Clients POST a job, get an ID immediately, and Spool runs jobs one at a time via `claude -p`, reporting outcomes through webhooks.

## Architecture invariants

These constraints drive most of the design and are easy to violate accidentally:

- **One global executor, one `claude` process at a time.** Not one per queue. The reason is credential safety: multiple holders of the same refresh token race and invalidate each other. Keep-alive pings and auth probes go through the *same* lock as job execution.
- **Auth mode is forced by the skills/connectors requirement.** Only a real `/login` session (stored in `CLAUDE_CONFIG_DIR=/data/claude`) gives synced claude.ai skills and connectors. Never set `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` in the container — either one outranks the login and silently disables skills and connectors. Never copy `.credentials.json` in from another machine.
- **Queues are logical; the executor is physical.** Queues own prompt template, tool allowlist, model, limits, notify list, retention, pause state, and history. The executor owns the login and the shared blocking states (`blocked_auth`, `blocked_usage`). Auth/usage failures block *everything*; queues keep accepting submissions regardless, and exactly one webhook fires per incident, not one per queue or job.
- **Queue definitions live in config (`queues.yaml`), never in the API or DB.** They are security policy (tool allowlists) and prompts — infrastructure-as-code, reviewed as diffs. The API exposes queues read-only so a phone client can never widen permissions. The DB holds only runtime state and job history.
- **At-most-once delivery.** Jobs have side effects (creating Notion pages), so duplicates are worse than failures. Automatic requeue happens *only* for auth/usage failures with zero tool calls in the stream. A container restart marks in-flight jobs `interrupted` and waits for a human. `retry` and `reply` create *new child jobs*, never re-run in place.
- **Two-layer success classification.** Layer 1 is run-level, read from the `stream-json` `result` message and event stream (auth, usage_limit, capability_missing, max_turns, timeout, cli_error). Layer 2 is task-level, from the `--json-schema` structured outcome (`status`, `summary`, `question`, `links`, plus per-queue `outcome_extension`). Both are needed; `is_error: false` alone does not mean the task succeeded.
- **Every job runs in a fresh empty working directory** (`/data/work/<job-id>`) so no stray `.claude/` or `.mcp.json` leaks into the run.
- **CLI version is pinned** with `DISABLE_AUTOUPDATER=1`. The result classifier and the PTY login-URL parser both scrape CLI output and are version-fragile; upgrades are deliberate and followed by watching auth metrics.

## Key mechanisms worth reading before touching

- **Re-auth flow** (§3.2): the login lifetime is unknown, so the design makes re-auth rare (4h keep-alive), visible early (probe + `auth.expiring`), harmless (block, don't drop), and quick (phone flow driving `claude auth login` under a PTY). `auth_events` exists specifically to *measure* the real cadence; the `long_lived_token` fallback mode (vendored skills + self-configured MCP) is a config switch, not a rewrite.
- **Scheduling** (§3.3): weighted round-robin across unpaused non-empty queues, FIFO within a queue, so one burst can't starve another queue.
- **Webhooks** (§3.6/§3.7): a transactional SQLite outbox with HMAC-SHA256 signing, `event_id` dedupe, and 24h backoff. The executor never waits on a receiver.

## Delivery order

Follow §7: spike → MVP (YAML queues, submit/list/get, scheduler + executor, classifier, SQLite, bearer tokens, Dockerfile; queues `media` and `adhoc`) → auth manager → reporting → ergonomics → frontends. Frontends and notification delivery are separate designs that consume this API — out of scope here.
