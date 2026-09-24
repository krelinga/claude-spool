# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status

This repo currently contains **no implementation** — only `docs/design/spool-design-doc.md` and a devcontainer. There is no build, test, or lint command yet; add this section's commands as the toolchain lands.

The design doc is the source of truth for behavior. Read it before implementing anything; §6 (validation spike) lists open questions about Claude Code CLI behavior that must be answered empirically before the code that depends on them is written.

## Toolchain

The devcontainer (`.devcontainer/devcontainer.json`) provides Node LTS and Claude Code only. The design doc recommends **Go** (static binary, `context` for subprocess control, `creack/pty`, pure-Go SQLite) with TypeScript as an equally viable alternative — the choice has not been made and no Go toolchain is installed. Confirm the language with the user before scaffolding; adding a toolchain means editing the devcontainer feature list.

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
