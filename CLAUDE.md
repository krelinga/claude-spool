# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

Spool is a self-hosted prompt queue for Claude: one container holds one
claude.ai login and runs multiple named queues of prompts against it. Send a job,
get an ID back immediately, and it runs unattended with your claude.ai skills and
connectors, reporting outcomes through signed webhooks.

This is a monorepo. Each component has its own CLAUDE.md with the detail that
matters there, and Claude Code reads the nearest one — so when working inside
`backend/`, that file applies and this one stays out of the way.

| Path | What it is |
|---|---|
| `backend/` | The Go service: API, scheduler, executor, auth manager. Complete. |
| `backend/spike/` | Harness that measures real Claude Code CLI behaviour. |
| `clients/` | API consumers — Drafts actions, and a small re-login web page. |
| `deploy/` | Operational config: `queues.yaml`, config examples, the real-run driver. |

## Where the design lives

`backend/docs/design/backend-design-doc.md` is the source of truth, and the
section references (§3.2, §3.4 …) scattered through the code point at it. It is
named for the backend because that is what it specifies; the clients are
deliberately thin and not designed there.

`backend/docs/design/spike.md` records what the CLI actually does, measured
rather than assumed, pinned to a CLI version. Read it before trusting anything
about flags, output shapes, or cost.

## Conventions across the repo

- **Queues are config, not code.** `deploy/spool/queues.yaml` carries the
  prompts, the tool restrictions and the budget caps. It is security policy and
  cost policy, reviewed as a diff, and the API exposes it read-only.
- **The clients are thin on purpose.** They call `GET /v1/queues` and
  `POST /v1/queues/{q}/jobs`; anything cleverer belongs in a queue's prompt or in
  a claude.ai skill, not in a client.
- **Reaching the API from a phone is Tailscale's job.** The service is
  LAN-only behind Caddy, so clients assume a tailnet rather than a public
  endpoint.

## Toolchain

The devcontainer covers everything here: Go for the backend, Node and a browser
for the clients, Docker for the image and the spike. Nothing in this repo needs
macOS — the iOS side is Drafts actions, which are JSON and JavaScript.

- **Go** (`ghcr.io/devcontainers/features/go:1`) with `gopls`, `dlv`,
  `staticcheck`, `golangci-lint`. Pin to a minor so the container and CI agree.
- **docker-in-docker** for building the image and running the spike container.
- **Node LTS**, required by the Claude Code feature.
- **`sqlite3` CLI** for inspecting `spool.db`.

Changing features means editing `.devcontainer/devcontainer.json`;
`devcontainer-lock.json` regenerates on rebuild and is not hand-edited.

## State as of 2026-09-25, and what to do next

Written as a handoff: the devcontainer this was built in is being deleted, so
this section is the only surviving memory of it. Everything below was verified,
not assumed.

**Done.** Design doc §7 steps 2–5 — the whole API side. Config-defined queues,
the scheduler, the single global executor, the two-layer classifier, SQLite,
scoped bearer tokens, the container image, reporting (signed webhook outbox, SSE,
metrics), the auth manager (keep-alive, expiry detection, PTY re-login, cadence
history), and the ergonomics verbs (retry, reply via `--resume`, cancelling a
running job, hot reload, retention pruning).

**Proven against reality, not fakes.** Two real books were added to the live
Notion Media database through a real claude.ai login, driven through Spool's own
`/v1/auth/login` PTY flow. The §6 spike is fully answered against CLI **2.1.282**;
read `backend/docs/design/spike.md` before trusting anything about CLI flags,
output shapes, or cost.

**Next, in the order that makes sense:**

1. Restart the longevity experiments (below). They measure the one thing still
   unknown, and they only accumulate while running.
2. Build the clients. `clients/README.md` has the plan. Worth deciding first
   which queues you want on the phone: the `notion-media`, `notion-ideas` and
   `notion-places` skills all exist on the account, but only `media` has a queue
   in `deploy/spool/queues.yaml`.

### Restarting the longevity experiments

Both were stopped before the devcontainer was deleted, and **both lost their
state with it** — the Docker volumes `spool-spike-claude` and `spool-real-data`
lived inside that container's Docker daemon, as did the captured spike output in
`backend/spike/out/` (gitignored). Nothing important was lost: every *finding* is
recorded in `backend/docs/design/spike.md`, and the time series had only 7 entries
over ~2.5 hours. They start from zero.

They answer §6 item 7, which is the last open question in the design and the
input to the §8 decision about whether to stay on `claudeai_login` or move to the
`long_lived_token` fallback. **They only accumulate while running, so restart them
early.**

```sh
# Arm 1 — kept warm. Measures the real re-auth cadence, and captures the
# genuine expiry and usage-limit error strings, which the classifier still
# only guesses at (authPatterns / usagePatterns in
# backend/internal/claudecli/classify.go).
backend/spike/run.sh build          # pins CLI 2.1.282
backend/spike/run.sh up
backend/spike/run.sh login          # interactive: open the URL, paste the code
backend/spike/run.sh keepalive      # 4h interval
backend/spike/run.sh keepalive-status   # check on it occasionally

# Arm 2 — never warmed, the control. Does idleness alone end a session?
deploy/real-run/run.sh build
deploy/real-run/run.sh up
deploy/real-run/run.sh login        # interactive, through Spool's own API
deploy/real-run/run.sh down         # stop it and leave the login idle
# Then, after days or weeks, without starting anything:
deploy/real-run/run.sh check-idle
```

If the idle login dies while the warmed one lives, the 4-hourly keep-alive is
load-bearing and the design was right to insist on it. If both live, it is belt
and braces and the interval could be relaxed. Either answer is worth having.

Caveats worth knowing before restarting:

- **The keep-alive does not survive a container restart.** It runs via
  `docker exec`, so stopping the container kills it silently.
  `backend/spike/run.sh keepalive-status` is the check, and `up` and `results`
  both warn when a previously recorded run has died.
- **Moving `backend/spike/` breaks a running container**, because the mounts are
  absolute host paths. Recreate with `up` afterwards; the login is in a named
  volume and survives, and the log file moves with the directory.
- Each check costs one small Haiku request, so roughly six a day.
- `deploy/real-run/.token` is generated on first `up` and is gitignored.

### The thing most likely to surprise a future session

Cost is dominated by **context, not work**. A job that called two Notion tools
and produced 948 output tokens cost **$1.03**, because every claude.ai connector
on the account offers its tools to every run — 202 of them, Adobe alone
contributing 107. Denying the connectors a queue does not declare in `requires`
brought the same job to **$0.43**. Both queue files already do this, and carry
`max_budget_usd` as a guard.

Keep those deny lists current: enabling a new connector on claude.ai silently
costs every queue. And note that `--allowedTools` does **not** reduce the offered
set — only `--disallowedTools` does, with wildcards on the server prefix.
