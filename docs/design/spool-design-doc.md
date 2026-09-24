# Spool — a self-hosted prompt queue for Claude

**Status:** Draft design · **Author:** Andy (with Claude) · **Date:** 2026-09-24

Spool is a single Docker container that holds one Claude login and runs **multiple named queues** on top of it. Each queue has its own prompt template, tool permissions, and tracking. A queue could be "add this to my Media list", "log this idea", "save this place", or "run this ad-hoc prompt". You send a job to a queue and move on. Spool runs jobs one at a time using your claude.ai skills and connectors, and it only interrupts you when something fails or needs you. That includes the one thing every queue shares: re-authenticating.

This document covers **only the API/service layer**. Frontends (an iOS app first) and notification delivery are separate designs that use this API.

---

## 1. Requirements

### Functional

| # | Requirement |
|---|---|
| F1 | Accept a job over HTTP and return right away with a job ID (no waiting on Claude). |
| F2 | Persist jobs durably. A container restart must not lose queued work. |
| F3 | Support **multiple named queues**. Each has its own prompt template, skill, model, tool permissions, limits, notification subscriptions, pause state, and job history. |
| F4 | Execute jobs **serially** (one Claude process at a time across the whole service), FIFO within a queue, fair across queues. |
| F5 | Jobs can use the **skills in the user's claude.ai account** and the **claude.ai connectors** (Notion first). |
| F6 | Record a per-job outcome: success or failure, a human-readable summary, an error category, cost/turns/duration, and a full transcript. |
| F7 | Report outcomes through an **abstract webhook**, subscribable per queue. |
| F8 | **Make re-auth cheap.** Detect it early, keep accepting jobs while signed out, and let the user re-login from a phone through the API. |
| F9 | Clients can list, inspect, cancel, retry, and reply to jobs, and pause or resume individual queues. |

### Non-functional

- **Scale:** one user, a handful of queues, tens of jobs a day in total, with occasional bursts.
- **Latency:** submission is instant. Execution latency doesn't matter.
- **Availability:** best-effort, but it must **never silently drop or silently double-run** a job.
- **Operability:** one container, one volume. Queues are defined in a config file (infrastructure-as-code). There's a Prometheus `/metrics` endpoint.
- **Security:** LAN-only behind Caddy/TLS, bearer tokens that can be scoped to queues, and least-privilege tool access per queue.

### Non-goals (v1)

- Parallel execution.
- Multi-user use.
- Scheduling or recurring jobs.
- Delivering push notifications directly.
- Creating or editing queues through the API (queues come from config).

---

## 2. High-level design

```
                ┌─────────────────────────────── spool container ────────────────────────────────┐
 iOS app ─┐     │                                                                                 │
 curl    ─┼─ Caddy ─► HTTP API ─► SQLite ◄───────────────────────────┐                            │
 others  ─┘     │       │   jobs · outbox · queue runtime · auth log │                            │
                │       │                                            │                            │
                │       │   ┌──────────── queues (from config) ──────┴───────┐                    │
                │       └──►│ media │ ideas │ places │ adhoc │ …             │                    │
                │           └──────────────────────┬─────────────────────────┘                    │
                │                     Scheduler: round-robin over queues that are                │
                │                     unpaused and non-empty                                      │
                │                                  ▼                                              │
                │   Auth manager ─────────► Executor (global, 1 job at a time) ── claude -p ─────┼─► Anthropic API
                │   • probe/keepalive       • gated by auth + usage state          (per job)     │   claude.ai connectors
                │   • login flow (PTY)      • stream → jobs/<id>.jsonl                             │
                │   • auth history          • classify → outcome                                  │
                │                                                                                 │
                │   Webhook sender (outbox) ─────────────────────────────────────────────────────┼─► receivers
                │   /data: spool.db · jobs/ · claude/ (ONE login, sessions, synced skills)        │
                └─────────────────────────────────────────────────────────────────────────────────┘
```

**The key split:** *queues* are logical. They carry the prompt, the policy, and the tracking. The *executor* is physical and there's exactly one of it. It owns the Claude login, runs one `claude` process at a time, and blocks for **all** queues when auth or usage limits say so. One login and one re-auth unblock everything.

**Components (one process, apart from the Claude subprocesses):**

1. **HTTP API** handles jobs, queues, auth, SSE, and metrics.
2. **Scheduler** picks the next job fairly across runnable queues.
3. **Executor** runs `claude -p` for a job, streams, times out or cancels, and classifies the result.
4. **Auth manager** owns the credential lifecycle: probing, keep-alive, the re-login flow, and history (§3.2).
5. **Webhook sender** is a transactional outbox with retry.

---

## 3. Deep dive

### 3.1 How Claude is invoked

**Auth method is forced by requirement F5.** Per the Claude Code docs, claude.ai **skills sync** and **claude.ai connectors load** only when the session authenticates with a claude.ai login made through `/login` or `claude auth login`. They don't work with `ANTHROPIC_API_KEY`, `apiKeyHelper`, or the 1-year `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token`, which "can only make model requests". ([auth](https://code.claude.com/docs/en/authentication), [skills](https://code.claude.com/docs/en/skills), [MCP](https://code.claude.com/docs/en/mcp))

So Spool keeps a real login in `CLAUDE_CONFIG_DIR=/data/claude` (`.credentials.json`, mode 0600). Don't set `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` in the container, because either one outranks the login and would quietly turn off skills and connectors. Don't pass `--bare`, and don't set the "disable nonessential traffic" vars, because skill sync needs feature-flag fetching.

**Executor: the `claude -p` subprocess** (not the Agent SDK in-process). This gives process isolation, clean SIGINT/SIGTERM handling, and a free choice of language. It's also plainly "Claude Code, run headless by its subscriber". The Agent SDK docs say third-party developers may not *offer* claude.ai login in their products; that's aimed at distributed products, but the CLI route avoids the question.

**Invocation per job**, built from the job plus its queue's config:

```bash
cd /data/work/<job-id>                      # fresh empty dir; no stray .claude/ or .mcp.json
claude -p "<rendered prompt>" \
  --output-format stream-json --verbose \
  --json-schema "<base outcome schema ⊕ queue extension>" \
  --append-system-prompt-file /data/run/<job-id>/system.md  # unattended.md + queue.system_prompt
  --permission-mode dontAsk --permission-prompts none \
  --allowedTools "<queue.allowed_tools>" \
  --max-turns <queue.max_turns> \
  [--model <job.model ?? queue.model>]
# env: CLAUDE_CONFIG_DIR=/data/claude  CLAUDE_CODE_SYNC_SKILLS=1  DISABLE_AUTOUPDATER=1
```

- **`dontAsk` + `--permission-prompts none`:** calls outside the queue's allowlist are denied (not left hanging) and recorded in `permission_denials`.
- **`unattended.md`** (shared by all queues): *"You're running unattended from a queue. Nobody can answer questions. Don't guess on ambiguous requests that create or modify data; report `needs_input` with a specific question. Check whether something already exists before you create it."*

### 3.2 Login lifetime and re-auth

#### What's known about how long a login lasts

| Credential | Lifetime | Source |
|---|---|---|
| Access token (inside a `/login` session) | about **8 hours**. Refreshed automatically with a stored refresh token. | GitHub issue reports ([#72017](https://github.com/anthropics/claude-code/issues/72017), [#60503](https://github.com/anthropics/claude-code/issues/60503)) |
| The `/login` session as a whole | **Finite, but the length isn't published.** The CLI warns "Your login expires in 3 days · run /login to renew", so there's a fixed expiry it knows about. | [auth docs](https://code.claude.com/docs/en/authentication#renew-an-expiring-login) |
| `claude setup-token` token | **1 year**, but no skills or connectors | [auth docs](https://code.claude.com/docs/en/authentication#generate-a-long-lived-token) |

**Other ways a login can die early:**

- A password change.
- A sign-out or revocation from elsewhere.
- Being idle long enough that the refresh token ages out.
- **Refresh bugs.** There are open reports on macOS and Linux of the refresh token not being used, which forces a re-login about every day.
- Two holders of the same refresh token racing each other.

**Bottom line:** the cadence can't be known ahead of time. It could be weeks, or it could be daily if a refresh bug bites. So the design makes re-auth **rare, visible early, harmless, and quick**, and it **measures** the real cadence. If the measurement is bad, there's a documented fallback mode (below).

#### Design

**1. Make it rare**

- **Keep-alive.** Every 4 hours (configurable), when no job has run recently, the auth manager runs a minimal real request: `claude -p "reply ok" --model haiku --max-turns 1`. This keeps the access token refreshing well inside its window, so an idle weekend doesn't let the session go stale. It costs a trivial amount of usage.
- **One writer.** The executor runs only one `claude` process at a time, and keep-alive and probes go through the same lock. Nothing inside the container races to rotate the refresh token. This is one reason the executor is global even with multiple queues.
- **Never copy credentials in or out.** Always create the container's login fresh, inside the container. Copying `.credentials.json` from another machine creates a second holder of the same refresh token.
- **Pinned CLI version.** Refresh bugs are version-specific. Upgrade on purpose and watch the auth metrics afterwards.

**2. Make it visible early**

- **Probe.** `claude auth status` returns JSON and exits with 1 when logged out. It's cheap and run often. The keep-alive is the authoritative check.
- **Expiry warning.** If the CLI emits its "expires in N days" warning (spike: check whether it shows up in `-p` stream output or in `auth status` JSON), fire an `auth.expiring` webhook so you can re-login at a convenient time rather than when something breaks.
- The `/v1/auth` endpoint and metrics expose state, last success, and session age.

**3. Make it harmless**

- Auth failure puts the **executor** in `blocked_auth`. All queues keep **accepting** jobs. Nothing is dropped.
- The in-flight job is requeued automatically only if it made no tool calls (§3.5).
- **One** `auth.required` webhook fires per incident, not one per queue or per job.
- When auth comes back, the executor unblocks by itself and `auth.restored` fires.

**4. Make it quick (a 30-second phone task)**

`claude auth login` is a non-TUI subcommand. The auth manager runs it under a PTY, pulls out the authorization URL, and feeds the pasted code back in:

```
POST /v1/auth/login            → 201 { "attempt_id": "...", "url": "https://claude.ai/oauth/…", "expires_at": "…" }
   (user opens URL on phone, approves, copies the code the page shows)
POST /v1/auth/login/{attempt}  { "code": "…" }  → 200 { "state": "ok", "account": "…" }  → executor unblocks
```

- The `auth.required` webhook payload includes a link to start this flow, so the notification → approve → paste loop happens on the phone. A tiny web page or an iOS screen can wrap it.
- `docker exec -it spool claude auth login` stays as the manual fallback.
- Spike risk: PTY-scraping the URL is brittle across CLI versions. It gets the same "pinned version plus one well-tested parser" treatment as the result classifier.

**5. Measure it.** Every auth transition (`ok → expired`, `login_completed`) goes into an `auth_events` table. Metrics:

- `spool_auth_state`
- `spool_auth_session_age_seconds`
- `spool_auth_relogins_total`
- a histogram of session lifetimes

After a month you'll know the real cadence.

#### Fallback mode, if re-auth turns out to be frequent

Spool supports two **credential modes**, and the choice doesn't affect queue definitions or clients:

| | `claudeai_login` (default) | `long_lived_token` (fallback) |
|---|---|---|
| Model auth | `/login` session | `CLAUDE_CODE_OAUTH_TOKEN` from `setup-token`, 1 year |
| Skills | Synced from claude.ai automatically | **Vendored:** copied into `/data/claude/skills/`, ideally from a git repo you own. They can be seeded from a one-time sync's `skills/synced/` copies. |
| Notion etc. | claude.ai connectors | **Self-configured MCP servers:** Notion's MCP server added with `claude mcp add`, authorized once with `claude mcp login --no-browser`, or a local Notion MCP server with an integration token |
| Re-auth | Unknown cadence (measure it) | Model token: yearly. MCP OAuth: its own cadence; an integration token doesn't expire. |
| Cost | None | Skills stop syncing automatically, and connector tool names differ, so queue allowlists and skills may need edits |

Each queue declares what it **needs** (`requires: [skills: [notion-media], connectors: [notion]]`), and the executor checks this against the `system/init` event before letting Claude run. A mode switch that breaks a queue fails loudly with `error_kind=capability_missing` instead of letting Claude improvise without its tools.

### 3.3 Queues

A queue is a **named, config-defined job type**. Clients send *input*, and the queue supplies everything else.

```yaml
# /etc/spool/queues.yaml — hot-reloaded on change; each job records the config hash it ran with
queues:
  media:
    description: Add or update entries in the Notion Media database
    prompt: |
      /notion-media {{input}}
    system_prompt: |                       # appended after unattended.md
      Prefer the edition/format details given. If the user says they own it, also record a Copy.
    requires: { skills: [notion-media], connectors: [notion] }
    allowed_tools: [Skill, WebSearch, WebFetch, "mcp__<notion>__notion-search",
                    "mcp__<notion>__notion-fetch", "mcp__<notion>__notion-create-pages",
                    "mcp__<notion>__notion-update-page"]
    outcome_extension:                     # extra fields merged into the JSON schema
      notion_url: { type: string }
      media_type: { enum: [book, film, tv, game, album, podcast, comic, essay, ttrpg] }
    model: sonnet
    max_turns: 30
    timeout: 10m
    weight: 1                              # scheduler share
    notify: [job.failed, job.needs_input]  # per-queue webhook subscription
    retention: 180d

  ideas:
    prompt: "/notion-ideas {{input}}"
    …

  adhoc:                                   # free-form escape hatch
    prompt: "{{input}}"
    allowed_tools: [Skill, WebSearch, WebFetch]   # deliberately narrow
    notify: [job.succeeded, job.failed, job.needs_input]
```

**Templating.** `{{input}}` is the free-text input. `{{args.<name>}}` are optional structured args that a queue can declare with a JSON schema, e.g. `args: { url: {type: string, format: uri} }`, so the iOS share sheet sends data, not prose. Templates are logic-free (Mustache-style substitution only). Complex prompt logic belongs in the skill.

**Why config and not API-managed?** Queues are code: they hold prompts and security policy (tool allowlists). A YAML file in git fits your infrastructure-as-code preference, gets reviewed diffs, and keeps the phone from being able to widen permissions. The API exposes queues **read-only** (`GET /v1/queues`), which gives frontends everything they need to render a "pick a queue" UI dynamically.

**Versioning.** Each job stores `queue_config_hash` and the fully rendered prompt, so history stays accurate after you edit a template. A job queued under the old config runs with the *new* config. That's simpler, and fine for a personal tool. If a queue is removed while it still has jobs, those jobs are marked `failed(queue_removed)` at startup/reload and a webhook fires.

**Scheduling across queues.** The scheduler uses weighted round-robin over queues that are unpaused and have queued jobs, and it's FIFO within a queue. A burst of 20 media adds can't starve one `adhoc` prompt behind it. An optional per-job `priority` still applies within a queue.

**Independent tracking** means each queue has its own:

- job list and stats (`GET /v1/queues/{q}`: depth, last success/failure, 7-day success rate)
- pause/resume
- webhook subscriptions
- retention
- metrics series (`queue` label)

The only thing queues share is the executor's state (auth and usage limits), which is exactly the part that *should* be shared.

**Token scoping.** An API token can be limited to certain queues (e.g. an `ios-share` token can only submit to `media`, `ideas`, and `places`). This matters because `adhoc` is effectively "run anything Claude's allowlist permits".

### 3.4 Knowing whether a job succeeded

There are two layers.

**Layer 1: run-level**, taken from the final `result` message and the event stream:

| Signal | Classification | Effect |
|---|---|---|
| `is_error: false`, structured outcome present | → layer 2 | |
| `Login expired` / `authentication_failed` | `auth` | Requeue if no tool calls. Executor → `blocked_auth`. |
| `api_retry` events with `rate_limit`, then failure | `usage_limit` | Requeue if no tool calls. Executor → `blocked_usage` until the reset time or backoff. |
| a required skill or connector is missing from `system/init` | `capability_missing` | Fail. Queue auto-paused and webhook sent (a config or mode problem, not a one-off). |
| `subtype: error_max_turns` | `max_turns` | Fail. |
| wall-clock timeout | `timeout` | SIGINT, 10 s grace, SIGTERM. Fail. |
| other `is_error` / non-zero exit | `cli_error` | Fail. |

**Layer 2: task-level**, from `--json-schema`. The base schema is `status` (`succeeded|failed|needs_input`), `summary` (one line, notification-ready), `question`, and `links`. Queues can add fields through `outcome_extension`, which get stored in `outcome`.

> Spike: confirm `structured_output` appears on the `result` line of `stream-json`. Fallback: tell Claude to end with a fenced JSON block and parse that.

### 3.5 Job lifecycle and retry semantics

```
                ┌──────────── cancel ───────────┐
                │                               ▼
  POST ──► queued ──schedule──► running ──► succeeded
             ▲                    │  │ ├──► failed        ──┐
             │  auth/usage and    │  │ ├──► needs_input   ──┤ POST /retry or /reply
             └── tool_calls == 0 ─┘  │ └──► cancelled     ──┤  ⇒ NEW child job (same queue)
                                     └─ restart ─► interrupted ─┘
```

**At-most-once by default**, because these prompts have side effects:

- A job that was `running` when the container died becomes `interrupted` and waits for a human.
- Automatic requeue happens only for auth or usage failures with **zero tool calls** in the stream.
- `retry` creates a new child job. `reply` answers a `needs_input` job by running the child with `--resume <session_id>` so Claude keeps its context.

### 3.6 Data model (SQLite, WAL)

Queue *definitions* live in config. The DB holds runtime state and history.

```sql
CREATE TABLE jobs (
  id TEXT PRIMARY KEY,                       -- ULID
  queue TEXT NOT NULL,
  queue_config_hash TEXT NOT NULL,
  status TEXT NOT NULL,                      -- queued|running|succeeded|failed|needs_input|cancelled|interrupted
  priority INTEGER NOT NULL DEFAULT 0,
  input TEXT, args TEXT,                     -- as submitted
  rendered_prompt TEXT,                      -- filled at start of run
  model TEXT, labels TEXT, client_ref TEXT,
  submitted_by TEXT NOT NULL,                -- API token name
  idempotency_key TEXT,
  parent_job_id TEXT REFERENCES jobs(id),
  resume_session TEXT,
  run_attempts INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, started_at INTEGER, finished_at INTEGER,
  -- outcome
  error_kind TEXT, error_message TEXT,
  outcome TEXT, summary TEXT,                -- structured_output JSON + its summary
  session_id TEXT, num_turns INTEGER, cost_usd REAL, duration_ms INTEGER,
  tool_calls INTEGER NOT NULL DEFAULT 0,
  permission_denials TEXT,
  UNIQUE (queue, idempotency_key)
);
CREATE INDEX jobs_sched ON jobs(queue, status, priority DESC, id);

CREATE TABLE queue_runtime (                 -- per-queue mutable state
  queue TEXT PRIMARY KEY,
  paused INTEGER NOT NULL DEFAULT 0,
  paused_reason TEXT,                        -- manual | capability_missing | …
  rr_credit REAL NOT NULL DEFAULT 0          -- weighted round-robin bookkeeping
);

CREATE TABLE executor_state (                -- single row: shared blocking state
  id INTEGER PRIMARY KEY CHECK (id = 1),
  state TEXT NOT NULL,                       -- ready|blocked_auth|blocked_usage|paused
  blocked_until INTEGER, reason TEXT
);

CREATE TABLE auth_events (                   -- measures the real re-auth cadence
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL,
  kind TEXT NOT NULL,                        -- probe_ok|expired|expiring_warning|login_started|login_completed|login_failed
  detail TEXT
);

CREATE TABLE webhook_outbox (
  id INTEGER PRIMARY KEY, event TEXT NOT NULL, queue TEXT, job_id TEXT,
  payload TEXT NOT NULL, url TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL, state TEXT NOT NULL
);
```

Transcripts go in `/data/jobs/<id>.jsonl`, pruned according to each queue's `retention`.

### 3.7 API

All endpoints are under `/v1` and use bearer tokens. Tokens are named, can be scoped to queues, and each one can be revoked on its own.

**Queues**

| Method & path | Purpose |
|---|---|
| `GET /queues` | Definitions (safe subset: name, description, args schema, outcome fields) plus runtime stats. Drives frontend pickers. |
| `GET /queues/{q}` | One queue: depth, running job, last success/failure, 7-day success rate, paused state. |
| `POST /queues/{q}/jobs` | **Submit.** `{input, args?, labels?, priority?, model?, client_ref?}` plus an `Idempotency-Key` header → `202 {id, status, position}`. |
| `GET /queues/{q}/jobs?status=&since=&cursor=` | That queue's history. |
| `POST /queues/{q}/pause` · `/resume` | Per-queue control. |

**Jobs** (IDs are global, so notifications can deep-link without knowing the queue)

| Method & path | Purpose |
|---|---|
| `GET /jobs?queue=&status=&cursor=` | Cross-queue feed (the "everything" inbox). |
| `GET /jobs/{id}` | Full record. |
| `GET /jobs/{id}/transcript[?follow=1]` | NDJSON, or an SSE live tail. |
| `POST /jobs/{id}/cancel` · `/retry` · `/reply` | As in §3.5. |

**Executor and auth**

| Method & path | Purpose |
|---|---|
| `GET /executor` | `{state, reason, blocked_until, running_job, total_depth}` |
| `POST /executor/pause` · `/resume` | Global control. `resume` also clears `blocked_usage` early. |
| `GET /auth` | `{state, account, last_ok_at, session_age, expires_warning?, mode}` |
| `POST /auth/login` · `POST /auth/login/{attempt}` | Phone-friendly re-login flow (§3.2). |
| `POST /auth/check` | Force a keep-alive probe now. |

**Other:** `GET /events` (SSE of webhook events), `GET /healthz`, `GET /metrics`.

**Webhooks** (configured globally, filtered per queue via `notify`):

- Job events: `job.succeeded`, `job.failed`, `job.needs_input`, `job.interrupted`, `job.cancelled`. These carry `queue`.
- Service events: `auth.required`, `auth.expiring`, `auth.restored`, `executor.blocked_usage`, `queue.auto_paused`. These are always sent to every receiver.
- Each event is signed with HMAC-SHA256, has an `event_id` for dedupe, and is retried with backoff for 24 h. Example:

```json
{ "event": "auth.required", "event_id": "…", "at": "…",
  "auth": { "state": "expired", "since": "…", "queued_jobs": 7,
            "login_url": "https://spool.lan/v1/auth/login" } }
```

### 3.8 Container and deployment

- **Image:** `debian:bookworm-slim` + Claude Code at a **pinned version** (`DISABLE_AUTOUPDATER=1`) + the `spool` binary + `tini`. Runs as a non-root user.
- **Volume `/data`:** `spool.db`, `jobs/`, `claude/` (the one login, sessions, synced or vendored skills, MCP config), `work/`.
- **Config:** `/etc/spool/config.yaml` (tokens, webhooks, credential mode, keep-alive interval) and `queues.yaml` (hot-reloaded), both mounted from a git-tracked directory.
- **Network:** plain HTTP inside, with Caddy + real cert on the host (same as Drydock).
- **Language:** Go is recommended (static binary, `context` for subprocess control, `creack/pty` for the login flow, pure-Go SQLite). TypeScript is equally viable.

---

## 4. Scale and reliability

- **Load:** tens of jobs a day across all queues, a few minutes each, fits easily in one serial executor. The binding limit is **subscription usage**, which is shared by all queues and is why usage blocking is global.
- **Durability:** a job is committed before the `202`. Take nightly `sqlite3 .backup` copies. Encrypt backups, because `claude/` holds a live credential.

**Failure modes**

| Failure | Behavior |
|---|---|
| Login expires | Executor `blocked_auth`. All queues keep accepting. **One** `auth.required` webhook with a re-login link. Unblocks automatically after login. |
| Login about to expire | `auth.expiring` webhook, if the CLI exposes the warning (spike). |
| Usage limit | Executor `blocked_usage` until reset or backoff. One webhook. |
| Restart mid-job | Job `interrupted` + webhook. Other work continues. |
| Queue misconfigured (e.g. skill missing) | `capability_missing` → that queue auto-pauses and a webhook fires. Other queues are unaffected. |
| Claude hangs | Per-queue timeout → SIGINT → SIGTERM. |
| Webhook receiver down | Outbox retries for 24 h. The executor never waits on it. |

**Metrics**

| Area | Metrics |
|---|---|
| Per queue | `spool_jobs_total{queue,status,error_kind}`, `spool_queue_depth{queue}`, `spool_queue_paused{queue}`, `spool_job_duration_seconds{queue}`, `spool_oldest_queued_age_seconds{queue}` |
| Shared | `spool_executor_state{state}`, `spool_auth_state`, `spool_auth_session_age_seconds`, `spool_auth_relogins_total`, `spool_auth_session_lifetime_seconds` (histogram), `spool_webhook_dead_total` |

**Grafana alerts:**

- `executor_state{state="blocked_auth"} == 1` for 1 hour (a backstop in case the webhook path is also broken)
- `oldest_queued_age > 2h`
- `rate(spool_auth_relogins_total[7d]) > 3` ("re-auth is too frequent; consider the fallback mode")

---

## 5. Trade-offs

| Decision | Chosen | Alternative | Why |
|---|---|---|---|
| Credential | `claudeai_login` + keep-alive + phone re-login, **measured** | `long_lived_token` + vendored skills and self-managed MCP | Only the login gives synced skills and connectors. The cost is an unknown re-auth cadence, so the fallback is built in and switching is a config change. |
| Queues vs executor | Many logical queues, **one** global executor | An executor per queue | One login means one refresh-token holder: no rotation races, one usage budget, one re-auth. Parallelism isn't a requirement. |
| Queue definitions | YAML in git, read-only API | CRUD over the API | Prompts and tool allowlists are code and security policy. The phone shouldn't be able to widen them. |
| Cross-queue order | Weighted round-robin | Global FIFO | A burst in one queue can't starve the others. |
| Prompt shape | Queue template + `input`/`args` | Clients send whole prompts | Fast, short submissions from share sheets, with consistent prompts. `adhoc` still allows free-form. |
| Executor | `claude -p` subprocess | Agent SDK in-process | Isolation, clean kill semantics, language freedom, and a clear first-party usage story. |
| Delivery | At-most-once + manual retry/reply | Auto-retry | Side effects. Duplicate Notion entries are worse than a failure notice. |
| Storage | Embedded SQLite | Postgres/Redis | The single-container constraint rules them out. |
| Notifications | Signed webhook + outbox | Built-in push | Keeps Spool frontend-agnostic. |

---

## 6. Validation spike (do this first)

Use a bare Debian container with a pinned CLI version:

1. **Connectors in `-p` mode.** Do the Notion tools appear in `system/init`, and what are their exact names?
2. **Synced skills in `-p`** with `CLAUDE_CODE_SYNC_SKILLS=1`. Does `/notion-media …` expand? What's the invocation name?
3. **`structured_output` on the stream-json `result` line.**
4. **`claude auth login` under a PTY.** Is the URL printed to stdout? Is the code accepted on stdin? Does it work with no browser in the container?
5. **`claude auth status`.** Is it local-only, or does it validate or refresh with the server? Does the "expires in N days" warning appear anywhere machine-readable?
6. **Error shapes.** Capture real `Login expired` and usage-limit results and build the classifier from them.
7. **Longevity (runs in the background while you build the MVP).** Leave the spike container running with a 4-hour keep-alive cron and log `auth status` and refreshes. This gives the first real data point on re-auth cadence before the service exists. Run a second container *without* keep-alive to see whether idleness matters.

## 7. Delivery plan

1. Spike (§6), with the longevity test left running.
2. **MVP:** queues from YAML, submit/list/get, scheduler + executor, classifier, SQLite, bearer tokens, Dockerfile. Two queues: `media` and `adhoc`.
3. **Auth manager:** keep-alive, `blocked_auth`, `auth_events`, the `/auth` endpoints, and the PTY login flow.
4. **Reporting:** webhook outbox with per-queue `notify`, `/events` SSE, `/metrics`, Grafana alerts.
5. **Ergonomics:** retry, reply/resume, cancel of running jobs, idempotency, `args` schemas, token scoping.
6. **Frontends** (separate designs): the iOS app (queue picker driven by `GET /queues`, share-sheet extension, re-login screen) and a notification adapter.

## 8. What to revisit as it grows

- **Credential mode**, once `spool_auth_session_lifetime_seconds` has a month of data.
- **Limited concurrency.** Only if a queue needs it. Parallel `claude` processes sharing one login need the refresh-race question answered first.
- **Scheduled or recurring jobs** as a client of the API, or as a `schedule:` block on a queue.
- **Queue chaining.** A job's outcome feeds another queue (e.g. `needs_input` → a Todoist task).
- **Per-queue credential overrides.** E.g. one queue on the long-lived token with vendored skills, while the others use the login.
