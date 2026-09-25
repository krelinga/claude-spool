# Spool backend

The Go service behind Spool: the HTTP API, the scheduler, the single global
executor that runs `claude -p`, and the auth manager that keeps one claude.ai
login alive. The behaviour is specified in
[`docs/design/backend-design-doc.md`](docs/design/backend-design-doc.md), and
what the Claude Code CLI actually does is recorded in
[`docs/design/spike.md`](docs/design/spike.md).

Releases publish `ghcr.io/krelinga/claude-spool/backend`, tagged with the full
version, the major.minor version, and `latest`, for `linux/amd64` only.

## Running it with Docker Compose

Everything a deployment needs lives in [`deploy/spool/`](../deploy/spool):

| File | What it is |
|---|---|
| `compose.yaml` | The service definition. |
| `config.yaml` | Tokens, webhooks, credential mode. You create it from `config.yaml.example`. It holds only the *names* of secrets, so it can live in git. |
| `queues.yaml` | The queues. Security and cost policy, reviewed as a diff, and hot-reloaded. |
| `.env` | The secrets themselves. Gitignored. |

The directory is mounted read-only at `/etc/spool`, and all state (the login,
`spool.db`, transcripts, per-job work dirs) is in the `spool_data` volume at
`/data`.

### 1. Write `config.yaml`

```sh
cd deploy/spool
cp config.yaml.example config.yaml
```

Then edit it:

- **`public_url`**: where the service is reachable from the phone, usually
  the Caddy hostname. The re-login link in an `auth.required` webhook is
  built from it.
- **`webhooks`**: point `url` at a real receiver, or delete the block. A
  receiver that doesn't exist retries for 24 hours and then shows up in
  `spool_webhook_dead_total`.
- **`tokens`**: the example has an unscoped `admin` token and a phone token
  scoped to `media`, `ideas` and `places`. Add or remove tokens as needed.

### 2. Write `.env`

Every `token_env` and `secret_env` that `config.yaml` names must be set and
non-empty, and API tokens and webhook secrets must be at least 16
characters. If one is missing,
Spool exits at startup and Docker keeps restarting it. For the example
config:

```sh
umask 077
cat > .env <<EOF
SPOOL_ADMIN_TOKEN=$(openssl rand -hex 32)
SPOOL_IOS_TOKEN=$(openssl rand -hex 32)
SPOOL_WEBHOOK_SECRET=$(openssl rand -hex 32)
EOF
```

The whole file is passed into the container, so a variable you add to
`config.yaml` later needs no change to `compose.yaml`. Compose also reads
these optional settings from the same file:

| Variable | Default | Effect |
|---|---|---|
| `SPOOL_IMAGE` | `ghcr.io/krelinga/claude-spool/backend:0.2` | The image to run. |
| `SPOOL_BIND` | `127.0.0.1` | Host address to publish on. Loopback assumes Caddy runs on the same host. |
| `SPOOL_PORT` | `8080` | Host port. |
| `SPOOL_MAX_BUDGET_USD` | unset | Overrides `default_max_budget_usd` (the per-job cap for queues that set none). |

**Never put `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or
`CLAUDE_CODE_OAUTH_TOKEN` in `.env`.** Each one outranks the claude.ai login
and silently turns off skills and connectors. In `claudeai_login` mode Spool
refuses to start if one is set.

### 3. Check, then start

```sh
docker compose run --rm spool spool --check   # validates both files and exits
docker compose up -d
docker compose ps                             # should reach (healthy)
```

`/v1/auth` will report `"state":"expired"` at this point. That is expected:
the volume has no login yet, so jobs are accepted and queue up until you log
in.

### 4. Log in once

The login is created inside the container and stays in the volume. **Never
copy a `.credentials.json` in from another machine**: two holders of the
same refresh token invalidate each other (design §3.2).

Log in through Spool's own API. This is the same flow the phone uses when
the login later expires:

```sh
TOKEN=$(grep SPOOL_ADMIN_TOKEN .env | cut -d= -f2)
curl -s -X POST localhost:8080/v1/auth/login -H "Authorization: Bearer $TOKEN"
# → {"attempt_id": "...", "url": "https://claude.ai/..."}
# Open the URL, approve, copy the code it shows, then:
curl -s -X POST localhost:8080/v1/auth/login/<attempt_id> \
  -H "Authorization: Bearer $TOKEN" -d '{"code":"<code>"}'
```

If the API flow fails, the fallback is to log in directly inside the
container and then ask Spool to re-probe:

```sh
docker compose exec spool claude auth login
curl -s -X POST localhost:8080/v1/auth/check -H "Authorization: Bearer $TOKEN"
```

`GET /v1/auth` should now report `"state":"ok"`. From then on the auth
manager sends a keep-alive every 4 hours. When the login does expire, the
executor blocks, queues keep accepting jobs, and one `auth.required`
webhook fires with the re-login link.

### 5. Put Caddy in front

The service speaks plain HTTP and is meant to be reached only through Caddy
on the LAN, and over Tailscale from the phone. A minimal site block:

```caddy
spool.lan {
	reverse_proxy 127.0.0.1:8080
}
```

Caddy flushes `text/event-stream` responses on its own, so `/v1/events` (SSE)
needs nothing extra. `/metrics` is unauthenticated on purpose so that
Prometheus can scrape it. It exposes counts and states, never job content,
but keep it on the LAN side of the proxy.

## Running a local build

To try unreleased changes, build the image from this directory and point
Compose at it:

```sh
docker build -t spool:local backend        # from the repo root
echo SPOOL_IMAGE=spool:local >> deploy/spool/.env
docker compose -f deploy/spool/compose.yaml up -d
```

The Claude Code CLI version comes from the `CLAUDE_CODE_VERSION` default in
the [`Dockerfile`](Dockerfile), the only pin in the repo. Override it for one
build with `--build-arg CLAUDE_CODE_VERSION=...`, but read the repo root's
CLAUDE.md on upgrading first. The classifier and the login parser both scrape
CLI output.

## Operating it

- **Logs**: `docker compose logs -f spool`. They are JSON lines, rotated at
  3 × 10 MB.
- **Changing queues**: edit `queues.yaml`. It is re-read within 10 seconds,
  or at once with `docker compose kill -s HUP spool`. Run `--check` first. A
  file that fails to parse is logged and the old queues stay live.
- **Changing `config.yaml` or `.env`**: neither is hot-reloaded. After a
  `config.yaml` edit, `docker compose restart spool`. After a `.env` edit,
  `docker compose up -d`, because `restart` doesn't re-read the env file.
- **Upgrading**: change the tag in `SPOOL_IMAGE` (or the default in
  `compose.yaml`), then `docker compose pull && docker compose up -d`. The
  login and history survive in the volume. Stopping the container doesn't
  wait for a running job: that job is marked `interrupted` on the next start
  and waits for a human to `retry` it. Choose a quiet moment.
- **Backups**: design §4 asks for nightly copies of `spool.db`. The image has
  no `sqlite3`, so the simplest consistent backup is a short stop:

  ```sh
  docker compose stop
  docker run --rm -v spool_data:/data:ro -v "$PWD":/out debian:bookworm-slim \
    tar -C /data -czf /out/spool-data.tgz .
  docker compose start
  ```

  **Encrypt the archive.** `claude/` inside it is a live credential.
- **Cost**: every claude.ai connector on the account offers its tools to every
  run, and that context dominates cost. Keep the `disallowed_tools` deny
  lists in `queues.yaml` current whenever a connector is enabled on claude.ai.
  See the repo root's CLAUDE.md for the numbers.

This deployment's volume is `spool_data`. The longevity experiment's volumes
(`spool-real-data`, `spool-spike-claude`) are separate, so running this
doesn't disturb them. Don't point this Compose project at them.

## Development

From this directory:

```sh
go build ./...
go test ./... -race
go vet ./... && gofmt -l .
go run ./cmd/spool --config ../deploy/spool/config.yaml --queues ../deploy/spool/queues.yaml --check
```

[`CLAUDE.md`](CLAUDE.md) has the layout, the architecture invariants, and the
CLI facts that were measured rather than assumed.
