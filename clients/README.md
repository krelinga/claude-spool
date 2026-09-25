# Clients

Things that consume the Spool API. Both are deliberately thin: they call
`GET /v1/queues` and `POST /v1/queues/{queue}/jobs` and little else. Anything
cleverer belongs in a queue's prompt or in a claude.ai skill, where it is
reviewed as config rather than shipped to a phone.

Nothing here is built yet.

| Path | What it will be |
|---|---|
| `drafts/` | Actions for the [Drafts](https://getdrafts.com) iOS app: send the current draft to a queue. |
| `web-login/` | A one-page wrapper for the re-login flow, so an expiry can be cleared from a browser. |
| `ui/` | A Vue status console: monitor jobs, reply to `needs_input`, retry, re-login. Designed in [`ui/DESIGN.md`](ui/DESIGN.md), which proposes folding `web-login/` into it. |

## Reaching the API

The service is LAN-only behind Caddy (design §3.8), and this setup assumes
**Tailscale** for anything off the LAN. Clients therefore point at a tailnet
address rather than a public endpoint, and there is no need for them to handle
being exposed to the internet.

## drafts/

Drafts can run a JavaScript action against an HTTP endpoint, which is enough for
the whole submit flow without a custom app.

Two details make it fit the backend better than a hand-rolled client would:

- **The draft's UUID is a natural `Idempotency-Key`.** Firing the action twice —
  a retap, a flaky tailnet — returns the original job instead of creating a
  second Notion page. The backend already enforces this per
  `(queue, idempotency_key)`; the client just has to supply a stable key, and
  Drafts hands it one.
- **`GET /v1/queues` drives the picker.** One action can list the queues and
  prompt for which to use, so adding a queue to `queues.yaml` makes it appear on
  the phone with no client change. That endpoint returns the safe subset — names,
  descriptions, arg schemas — which is exactly what a picker needs and nothing
  more.

The token belongs in a Drafts `Credential`, not in the action body. Actions live
in Drafts' own database, so the copies here are the source of truth and the
setup notes will cover exporting and importing them.

## web-login/

When the login expires, the executor blocks and one `auth.required` webhook
fires with a link. The design notes that "a tiny web page or an iOS screen can
wrap" the flow (§3.2); this is that page. It does two calls:

1. `POST /v1/auth/login` — shows the returned URL to open and approve.
2. `POST /v1/auth/login/{attempt}` — submits the pasted code.

Static HTML and a little JavaScript, served from anywhere on the tailnet. The
manual fallback remains `docker exec -it spool claude auth login`.
