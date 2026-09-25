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
