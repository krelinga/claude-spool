# Spool status UI — design

**Status:** Draft · **Date:** 2026-09-25 · **Component:** `clients/ui`

A small Vue web app for watching Spool and unblocking it: see what is queued,
running and finished, read a transcript, answer a job that stopped with
`needs_input`, retry a failure, and clear an expired login. It is an operator
console for one person, reached over the tailnet.

The owner is not a frontend developer, so this design puts as much weight on
**how the UI is built, tested and released** as on what it shows. The goal is
that a change to the UI works like a change to the backend: open a PR, CI
proves it works and shows screenshots, merge, and release-please does the rest.

Section references like §3.5 point at
[`backend/docs/design/backend-design-doc.md`](../../backend/docs/design/backend-design-doc.md).

---

## 1. Requirements

### Functional

| # | Requirement |
|---|---|
| U1 | Show the shared machinery at a glance: auth state, executor state (`ready`, `blocked_auth`, `blocked_usage`, `paused`, plus `blocked_until`), the running job, and total depth. |
| U2 | A **"needs you" inbox**: every `needs_input`, `failed` and `interrupted` job, with the action that fits each one on the same screen. |
| U3 | **Reply** to a `needs_input` job. Show Claude's `question`, take an answer, `POST /v1/jobs/{id}/reply`, then follow the child job it creates. |
| U4 | **Retry** a failed, cancelled or interrupted job, and **cancel** a queued or running one. |
| U5 | A cross-queue job feed, filterable by queue and status, with cursor paging. |
| U6 | A job detail page: input, summary, links, error, cost, turns and duration, the parent job, and a readable transcript. |
| U7 | Queue list with depth and paused state, plus pause and resume. Pause and resume the executor as well. |
| U8 | **Re-login** through `POST /v1/auth/login` and `/login/{attempt}`, plus the auth event history. |
| U9 | Updates live while the tab is open, without a manual refresh. |
| U10 | Usable on a phone as well as a desktop browser. |

### Non-functional

- **Easy to change safely** for someone who does not write frontend code
  daily. Types, tests and CI catch mistakes, not the owner's eye.
- **Releases hands-off,** following the repo's existing release-please pattern.
- **Thin.** Everything the UI shows comes straight from `/v1`. It holds no
  state of its own apart from the API token.
- **Never breaks the API.** A UI outage must not affect the Drafts actions or
  webhooks.

### Non-goals (v1)

- Submitting new jobs. That is what Drafts is for. It is easy to add later
  (one form driven by `GET /v1/queues`), but it isn't needed to monitor.
- Editing queues. They are config, and the API is read-only for them (§3.3).
- Push notifications. Webhooks already cover them.
- Multi-user support, accounts, or public internet exposure.

---

## 2. What the backend gives us, and what it doesn't

Checked against `backend/internal/api` at `backend/v0.1.0`.

| Need | Backend today | Consequence for the UI |
|---|---|---|
| Auth | Bearer token in `Authorization` only (`bearerToken` in `api.go`). | The browser must send a header. That rules out the native `EventSource` for SSE (see §4.3). |
| Cross-origin | No CORS headers. | Serve the UI **same-origin** with `/v1` (§5). No backend change needed. |
| Live events | `GET /v1/events` is SSE, but only for **terminal** job events plus service events. There is no event when a job is queued or starts. | Use SSE to trigger a refetch, and poll lightly as well (§4.3). |
| Live transcript | `?follow=1` is in the design (§3.7), but `getTranscript` doesn't implement it. It returns the NDJSON file as it stands. | Poll the transcript while a job is running. |
| Reply | `POST /jobs/{id}/reply {input}`, `needs_input` only, and it needs a `session_id`. Otherwise 409 `no_session`. | On `no_session`, offer retry instead. |
| Retry | `POST /jobs/{id}/retry`, no body. The child reruns the same input. | A failed job can't be retried with an extra note. See open question Q2. |
| Paging | `GET /jobs?queue=&status=&cursor=&limit=` → `{jobs, next_cursor}`. | "Load more" button, no page numbers. |
| Handled jobs | A parent **keeps** its `needs_input` or `failed` status after a reply or retry. The child only points back through `parent_job_id`, and the API has no way to list a job's children (`store.Children` exists but isn't exposed). | The inbox can't tell a handled job from an open one on its own. See §3.1a. |
| Errors | Flat `{error, message}` bodies: a machine code plus a readable message (for example `not_retryable`). | Show the backend's message in a toast. Don't reinvent the wording. |

None of the gaps blocks v1. §8 lists small, additive backend changes that make
the UI nicer, and each one can ship on its own.

---

## 3. Screens

One layout: a **status strip** across the top of every page, then the page.

```
┌──────────────────────────────────────────────────────────────────────────┐
│ ● Auth: ok (session 3d)   ● Executor: ready   ▶ media · 01J…  2m   Depth 4 │  ← status strip
├──────────────────────────────────────────────────────────────────────────┤
│  Needs you (3)  │  Jobs  │  Queues  │  Auth                              │
├──────────────────────────────────────────────────────────────────────────┤
│  ⚠ needs_input  places · "Which Joe's Pizza — Carmine St or Broadway?"   │
│     [ answer…                                             ] [Reply]      │
│  ✕ failed       media  · timeout after 10m                 [Retry] [View]│
│  ⏸ interrupted  ideas  · container restarted mid-run       [Retry] [View]│
└──────────────────────────────────────────────────────────────────────────┘
```

The strip turns loud (red, full width, with a **Re-login** button) when the
executor is `blocked_auth`. It turns amber, with a countdown to `blocked_until`
and a "Resume now" button, when it is `blocked_usage`. Those are the two states
where Spool is waiting on a human, so they must be impossible to miss.

| Route | Page | Data |
|---|---|---|
| `/` | **Needs you.** The default page. Needs-input jobs show the question and an inline reply box. Failed and interrupted jobs show the error and a Retry button. | `GET /jobs?status=needs_input`, `…failed`, `…interrupted` |
| `/jobs` | **Feed.** Table of recent jobs with queue and status filters (kept in the URL query so they can be bookmarked), and "Load more". | `GET /jobs` |
| `/jobs/:id` | **Detail.** Header with status, queue and actions (Cancel, Retry, Reply, each shown only when the status allows it). Then the summary, question, links, error, metrics, and a link to the parent job. Last comes the transcript. | `GET /jobs/{id}`, `GET /jobs/{id}/transcript` |
| `/queues` | **Queues.** One card per queue: description, depth, running job, last success or failure, 7-day success rate, paused (and why), and Pause/Resume. | `GET /queues` |
| `/auth` | **Auth.** State, account, session age, and a **Re-login** wizard (start → open this URL → paste the code → done). A "Check now" button calls `POST /auth/check`. Below that, the auth event history, which records the re-auth cadence. | `GET /auth`, `GET /auth/events`, `POST /auth/login…` |
| `/settings` | Paste or clear the API token. | local only |

### 3.1 The reply flow (U3)

This is the flow the UI most needs to get right.

1. The inbox, or the detail page, shows `outcome.question` prominently, with
   the original input and summary for context.
2. The owner types an answer and submits it. The UI calls
   `POST /v1/jobs/{id}/reply {"input": "…"}`.
3. On `202`, the UI navigates to the **child** job (the `202` body is
   `{id, status, queue, position}`). The child shows "Replying to 01J…" with a
   link back to the parent.
4. On `409 no_session`, the UI explains that the context can't be resumed and
   offers **Retry** instead. The message comes from the backend.
5. The child finishes, and its SSE event updates the page in place. If it asks
   *another* question, the loop repeats.

A job's action buttons come from one pure function,
`actionsFor(job): Action[]`. It mirrors `store.Retryable` and the reply rule
(`needs_input` with a `session_id`). It is unit-tested against every status, so
a button can never offer something the backend will reject.

### 3.1a Keeping the inbox honest

A job counts as *open* when its status is `needs_input`, `failed` or
`interrupted` **and** no other job names it as `parent_job_id`. v1 works this
out on the client. It fetches the most recent jobs (a few hundred at tens a
day covers weeks), collects the set of `parent_job_id`s, and hides those
parents. Open items older than 14 days fold into an "older" section rather than
nagging forever. This logic lives in one pure function, `openItems(jobs)`, with
unit tests.

It is a workaround. B6 (§8) moves it server-side, and the function then
collapses to a single query.

### 3.2 Transcript view

The transcript is NDJSON `stream-json` from the CLI. The viewer turns it into a
list of collapsible rows: assistant text, a tool call (tool name plus arguments,
collapsed), the tool result (truncated, and expandable), and the final result.
Unknown line types fall back to raw JSON, so a CLI upgrade that changes the
format degrades gracefully instead of breaking the page. The parser is a pure
function with unit tests fed from real transcripts captured by the spike.

**Transcripts are untrusted content.** They hold whatever web pages and
connectors returned. They are always rendered as text and never as HTML, and
the lint config enforces that (§6.1).

---

## 4. Architecture

### 4.1 Stack

Each choice favours "boring, popular, well-typed, and Claude Code knows it
well" over novelty.

| Concern | Choice | Why |
|---|---|---|
| Framework | **Vue 3** + `<script setup>` + **TypeScript** | As asked. `<script setup>` is the smallest, most readable form. |
| Build | **Vite**, scaffolded with `npm create vue@latest` (TS, Router, Vitest, Playwright, ESLint, Prettier) | The official scaffold wires up every tool in §6 in one step, with configs the Vue docs describe. |
| Components | **PrimeVue** with the Aura theme | Ready-made DataTable, Tag, Dialog, Toast, Timeline and Accordion. Almost no hand-written CSS, which is where a non-frontend developer loses the most time. |
| Server state | **TanStack Query for Vue** (`@tanstack/vue-query`) | Caching, polling, refetch on focus, and invalidation after mutations, all declared per query. This replaces a hand-rolled store, which is the part of a UI that usually rots. |
| Routing | `vue-router` | From the scaffold. |
| API types | **zod** schemas in `src/api/schema.ts`, with types derived through `z.infer` | One definition gives compile-time types *and* a runtime check. When the backend changes a shape, it fails loudly at the API boundary instead of rendering `undefined`. It is also what the contract test in §6.3 checks against. |
| Global state | none; the token lives in a `useToken()` composable backed by `localStorage` | Pinia isn't needed when the server is the source of truth. |

The node version comes from the devcontainer's Node LTS feature, recorded in
`clients/ui/.nvmrc` and `package.json` `engines` so CI matches.
`package-lock.json` is committed, and CI uses `npm ci`.

### 4.2 Layout

```
clients/ui/
  CLAUDE.md            commands and conventions for Claude Code in this dir
  DESIGN.md            this file
  package.json         version is bumped by release-please
  Dockerfile           build static files, serve with Caddy
  Caddyfile
  src/
    api/
      client.ts        fetch wrapper: base URL, bearer header, error → ApiError
      schema.ts        zod schemas + types (Job, Queue, Executor, Auth, Envelope)
      queries.ts       every useQuery / useMutation, one place
      events.ts        SSE reader (fetch + ReadableStream)
    lib/
      actions.ts       actionsFor(job): the button rules
      transcript.ts    NDJSON → view rows
    components/        StatusStrip, JobRow, ReplyBox, TranscriptView, …
    pages/             NeedsYou, Jobs, JobDetail, Queues, Auth, Settings
  tests/
    unit/              *.spec.ts: pure functions, components, with MSW
    e2e/               Playwright specs against a real backend and a fake CLI
```

### 4.3 Live updates (U9)

Two mechanisms, each simple, and each covering for the other.

1. **SSE as a nudge.** `events.ts` opens `GET /v1/events` with `fetch` (so it
   can send the bearer header), reads the stream, and parses the
   `event:`/`data:` frames. That takes about 40 lines and has unit tests, so no
   dependency is needed. Each event **invalidates** the matching queries (for
   example, `job.*` invalidates `['jobs']` and `['job', id]`, and `auth.*`
   invalidates `['auth']`). TanStack then refetches whatever is on screen. The
   UI never patches state from event payloads, so it can't drift from the
   server. The reader reconnects with backoff, and the status strip shows a
   small "live" or "reconnecting" dot.
2. **Polling as the floor.** The executor status polls every 10 s, and a running
   job and its transcript poll every 3 s. Polling happens only while the tab is
   visible (TanStack's default). That covers what SSE doesn't carry: queued →
   running transitions, and transcript growth.

When the backend gains `job.started` and `?follow=1` (§8), the intervals can
grow. No page code changes, because only `queries.ts` knows about polling.

### 4.4 Mutations

Every action is a `useMutation` in `queries.ts`. On success it invalidates the
affected queries and shows a toast. On failure it shows the backend's `message`.
Buttons disable while a request is in flight, so a double tap can't send a
duplicate. Reply and retry send an `Idempotency-Key` (a UUID generated when the
form opens), so a flaky tailnet can't create two children. *(This needs the
backend to honour the key on reply and retry. Today it only does so on submit.
See §8.)*

### 4.5 Auth and security

- **Token.** The UI asks for a token on first load and keeps it in
  `localStorage`. Add a dedicated token to `config.yaml`, `name: ui`,
  `queues: ["*"]`, so it can be revoked without touching the phone or the admin
  token. Anyone who can open the page still needs the token.
- **XSS is the real risk,** because an XSS bug would expose the token. The
  mitigations: no `v-html` anywhere (a lint error), transcripts rendered as
  text, and a strict `Content-Security-Policy` from the UI's Caddy
  (`default-src 'self'`, with no inline scripts, which a Vite build doesn't
  need).
- LAN and tailnet only, the same as the API (§3.8).

---

## 5. Hosting: same origin, separate container

```
 browser ──► host Caddy (spool.lan, TLS)
               ├─ /v1/*, /healthz, /metrics ──► spool backend :8080   (unchanged)
               └─ everything else ────────────► spool-ui :8080        (static files)
```

- **The UI ships as its own image,** `ghcr.io/krelinga/claude-spool/ui`. It
  holds the Vite build served by Caddy (`caddy:2-alpine`, about 50 MB), with SPA
  fallback to `index.html`, long cache headers on hashed assets, and the CSP.
- **Host Caddy splits the paths.** The browser sees one origin, so there is no
  CORS and no backend change. `/v1` never passes through the UI container, so
  a broken UI can't take down Drafts or webhooks. The SSE route needs
  `flush_interval -1` so Caddy doesn't buffer it.
- **Why not embed the UI in the Go binary** (`go:embed`)? It would be one
  container, but it would tie UI releases to backend releases and put Node in
  the backend build. Independent releases are the repo's release model (the
  `/v1` contract makes them safe), so the UI gets its own image like any other
  component.
- **In local dev,** `vite` proxies `/v1` to `SPOOL_BACKEND` (default
  `http://localhost:8080`), which reproduces the same-origin setup without Caddy.

---

## 6. Testing

The testing plan has one job: **let the owner trust a green PR without reading
Vue code closely.** It has four layers. Each is cheaper and faster than the one
below it, and each catches something the others can't.

| Layer | Tool | Catches | Runs |
|---|---|---|---|
| 1. Static | `vue-tsc`, ESLint, Prettier | Type errors, wrong prop names, unsafe patterns, formatting | seconds; every PR |
| 2. Unit and component | Vitest + Vue Test Utils + **MSW** | Logic bugs in `actionsFor`, the transcript parser, the SSE parser; components rendering each state | seconds; every PR |
| 3. Contract | Vitest over **backend golden fixtures** | The backend changing a JSON shape the UI relies on | seconds; every PR touching either side |
| 4. End-to-end | **Playwright** against the real `spool` binary with a **fake `claude`** | The whole flow working in a real browser: reply, retry, cancel, live update | ~2 min; every PR |

`npm run check` runs layers 1–3 locally. `npm run test:e2e` runs layer 4.
CI runs both.

### 6.1 Static

The scaffold's ESLint flat config, plus these rules, set to *error*:
`vue/no-v-html` (security, §4.5), `@typescript-eslint/no-explicit-any`, and
`@typescript-eslint/no-floating-promises` (catches a mutation that is fired but
never awaited). TypeScript runs in `strict` mode. These are the cheapest
guardrails, and they catch most of what a reviewer who doesn't know the
framework would miss.

### 6.2 Unit and component

- **Pure functions** get table tests: `actionsFor` over every
  `status × session_id` combination, `transcript.ts` over captured transcripts,
  and the SSE frame parser over split and partial chunks.
- **Components** render against **MSW** (Mock Service Worker), which intercepts
  `fetch` and answers with fixtures. Each page has one test per interesting
  state: loading, empty, error, and each executor state for the strip. The
  fixtures are the golden files from §6.3, not hand-written JSON, so they can't
  silently diverge from the backend.

### 6.3 Contract: golden fixtures from the backend

This is what makes independent releases safe in practice, not just in
principle.

- **Backend side:** `api_test.go` gains a `-update` flag. With it, a handful of
  tests write the real handler responses (a job in each status, `/queues`,
  `/executor` in each state, `/auth`, an SSE envelope) to
  `backend/internal/api/testdata/golden/*.json`. Without it, they compare the
  responses against those files. Volatile fields (IDs, timestamps) are
  normalised.
- **UI side:** one Vitest file parses every golden file with its zod schema.
- **The effect:** a backend PR that changes a response shape must regenerate the
  golden files, which shows up plainly in the diff. The UI's contract test then
  runs in the same PR and fails if the UI can't read the new shape. A breaking
  `/v1` change is caught before merge, whichever side made it.

### 6.4 End-to-end with a fake CLI

The executor tests already use a bash stand-in for `claude`
(`fakeClaude` in `executor_test.go`). Promote it to a real, reusable fake,
`backend/internal/testing/fakeclaude/` (a tiny Go `main`). It emits valid
`stream-json`, and **the job's input chooses the scenario**:

| Input contains | Fake behaviour |
|---|---|
| `[ok]` | a couple of tool calls, then `status: succeeded` |
| `[ask]` | `status: needs_input`, a `question`, and a `session_id` |
| resumed session (`--resume`) | `succeeded`, echoing the reply into `summary` |
| `[fail]` | `is_error`, which the classifier treats as a `cli_error` failure |
| `[slow]` | streams one line a second for 60 s (for the cancel and live-transcript tests) |

Playwright's `globalSetup` builds `spool` and the fake, writes a temporary
`config.yaml` and `queues.yaml` (two queues, one token, `claude.binary` pointing
at the fake), starts the backend, and starts `vite preview` with the `/v1` proxy.
The specs then drive the real UI:

1. Submit `[ask]` through the API. The inbox shows the question. Reply. The
   browser lands on the child, which turns `succeeded` without a reload (SSE).
2. `[fail]`, then Retry. A child appears, and the error is shown.
3. `[slow]`, then open the job. The transcript grows. Cancel. The job becomes
   `cancelled`.
4. Pause a queue, then resume it. Pause the executor. The strip changes.
5. Each page at a phone viewport (Chromium's iPhone emulation): nothing
   overflows, and the reply box is usable.

The login PTY flow is left out of e2e in v1, because faking `claude auth login`
convincingly takes real work. The login wizard gets MSW component tests
instead, and it can be exercised by hand against `deploy/live-test/`.

**Screenshots for the reviewer.** Every e2e run saves a full-page screenshot of
each page and state. CI uploads them as a build artifact, along with Playwright
traces on failure. So a UI PR can be reviewed by *looking at it*, without
checking it out. This is deliberately **not** pixel-diff snapshot testing.
Font and antialiasing differences make that flaky, and the flakiness would
teach everyone to ignore it. It could be revisited once the UI settles.

Chromium only. It's a one-user console, and a cross-browser matrix would triple
the run time for almost no benefit.

---

## 7. CI and release

Everything follows the existing patterns. The only actions used are GitHub's
own (`actions/checkout`, `setup-node`, `setup-go`, `upload-artifact`) and the
`docker/*` actions `release-please.yml` already uses, per the repo's preference
for inline steps over community actions.

### 7.1 On every PR: `.github/workflows/ui.yml`

```yaml
on:
  pull_request:
    paths: ["clients/ui/**", "backend/**", ".github/workflows/ui.yml"]
jobs:
  check:      # setup-node (from .nvmrc, npm cache) → npm ci → npm run check
  e2e:        # + setup-go → npx playwright install --with-deps chromium
              # → npm run test:e2e → upload screenshots (always) + traces (on failure)
  image:      # docker build clients/ui (no push), so the Dockerfile can't rot
```

It triggers on `backend/**` as well, because a backend change can break the
UI (§6.3). *Note:* if these checks are ever made **required** for merging, the
path filter would leave them pending on PRs that don't match. At that point,
switch to an always-run workflow that skips its steps early.

(The backend has no PR test workflow today. `go test ./...` belongs in one, and
this is a good moment to add `backend.yml` alongside, but it is independent of
the UI.)

### 7.2 Release: the same release-please flow

- Add to `release-please-config.json`:

  ```json
  "clients/ui": { "release-type": "node", "component": "ui" }
  ```

  and `"clients/ui": "0.0.0"` to the manifest. The `node` release type bumps
  `package.json` and writes `clients/ui/CHANGELOG.md`. Tags follow the repo's
  `<path>/vX.Y.Z` form, so check that the first release PR produces
  `clients/ui/v0.1.0`, and set `"component": "clients/ui"` if it doesn't.
- Add a `ui-image` job to `release-please.yml`, a copy of `backend-image` gated
  on `clients/ui--release_created`, that pushes
  `ghcr.io/krelinga/claude-spool/ui` with the version, `major.minor` and `latest`
  tags. The npm build runs on the build platform, so an `linux/arm64` image
  would cost almost nothing here, but it stays amd64 for consistency with the
  backend.
- **Nothing else changes for the owner.** PR titles are conventional commits
  (`feat: show the transcript live`). Merging the release PR is the release.

### 7.3 Dependencies: Dependabot

A frontend has far more dependencies than the Go backend, and they move fast.
`.github/dependabot.yml` for `npm` in `/clients/ui`, weekly, with all minor and
patch updates **grouped into one PR**. Major versions get their own PRs.
Dependabot's `commit-message.prefix: "fix(deps)"` and
`prefix-development: "chore(deps)"` keep the titles valid for `pr-title`, so
runtime updates cut a patch release and dev-tool updates release nothing.
The e2e suite is what makes merging these safe. Once it has a track record,
grouped minor and patch PRs that pass CI can be set to auto-merge.

### 7.4 Deploying

Run the image next to the backend on the same Docker network, and add the path
split from §5 to the host Caddyfile. `deploy/spool/` gets a `Caddyfile.example`
snippet, and `config.yaml.example` gets the `ui` token. Upgrading means pulling
a new tag. The UI holds no state, so rollback is just the previous tag.

### 7.5 Working on it with Claude Code

`clients/ui/CLAUDE.md` lists the commands (`npm run dev`, `check`, `test:e2e`),
the rule that every API call goes through `queries.ts`, and the no-`v-html`
rule. To see a change without reading code, the `run` skill (or Playwright
through Claude Code) can start the dev server against a fake backend and take
screenshots. That is the same loop CI runs.

---

## 8. Small backend changes, in priority order

All are additive to `/v1`, so each is a `feat:` or `test:` PR against
`backend/` that ships on its own. Only the first two block anything, and they
only block the *tests*.

| # | Change | Why | Needed by |
|---|---|---|---|
| B1 | Golden fixtures in `api_test.go` with `-update` | The contract test, §6.3 | UI phase 1 CI |
| B2 | Reusable `fakeclaude` binary | e2e, §6.4 | UI phase 2 CI |
| B3 | Honour `Idempotency-Key` on `/reply` and `/retry` | Double-submit safety from a phone, §4.4 | nice to have |
| B4 | SSE-only `job.queued` and `job.started` events (broker, not webhook outbox) | Faster, lighter live updates, §4.3 | nice to have |
| B5 | `GET /jobs/{id}/transcript?follow=1` as the design specifies | A live transcript without polling | nice to have |
| B6 | `children` on `GET /jobs/{id}`, and an `open=1` filter on `GET /jobs` (terminal-but-actionable jobs with no child) | Inbox accuracy without the client workaround in §3.1a, and parent → child links | strongly wanted by phase 2 |

---

## 9. Delivery plan

The release pipeline comes **first**, before any real screen, so the hard,
unfamiliar part is proven while the app is trivial.

1. **Phase 0: pipeline.** Scaffold with `create-vue`, add PrimeVue, the
   Dockerfile and Caddyfile, `ui.yml` (check and image, with e2e as one smoke
   test that the page loads), release-please wiring, and Dependabot. Merge with a
   `feat:` title, merge the release PR, and pull the published image. Done
   when `ghcr.io/krelinga/claude-spool/ui:0.1.0` serves a page behind
   host Caddy.
2. **Phase 1: read-only monitoring.** Token settings, API client and zod
   schemas, the status strip, the Jobs feed, Job detail with the transcript,
   Queues, and SSE with polling. Plus B1 and the contract test.
3. **Phase 2: acting.** The Needs-you inbox, reply, retry, cancel, and pause and
   resume. Plus B2, B6 and the full e2e suite.
4. **Phase 3: auth.** The Auth page and re-login wizard, with MSW tests.
   Retire the planned `web-login/` client (Q1).
5. **Phase 4: polish.** B3 to B5 as they're wanted, and job submission if it
   turns out to be wanted after all.

---

## 10. Open questions

- **Q1. Fold `web-login/` into this UI?** Recommended. The Auth page is
  exactly the page `clients/README.md` plans, and one tailnet URL is simpler
  than two. The `auth.required` webhook's `login_url` would then point at
  `https://spool.lan/auth`.
- **Q2. Retry with an extra note?** Today only `needs_input` jobs can take new
  input. A job that *failed* because the prompt lacked information can only be
  retried unchanged, or resubmitted from Drafts. An optional
  `{"input": "..."}` on `/retry` (appended to the original input, or resumed
  like a reply when there is a session) would cover it. That is a backend
  decision about retry semantics (§3.5), so it's flagged here rather than
  assumed.
- **Q3. PrimeVue or Vuetify?** Both are fine. PrimeVue is lighter and its
  DataTable fits the feed well. Pick one in phase 0 and don't revisit.
