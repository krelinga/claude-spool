# Spool

A self-hosted prompt queue for Claude. One container holds one claude.ai login
and runs multiple named queues on top of it: send a job, get an ID back
immediately, and Spool runs it unattended with your claude.ai skills and
connectors, reporting the outcome.

Design: [`docs/design/spool-design-doc.md`](docs/design/spool-design-doc.md).
Open questions: [`docs/design/spike.md`](docs/design/spike.md).

## Status

MVP. Implemented: config-defined queues, job submission and history, the
weighted round-robin scheduler, the single global executor, result
classification, SQLite persistence, scoped bearer tokens, and the container
image.

Not yet implemented: the auth manager and phone re-login flow, webhooks, SSE,
Prometheus metrics, and retry/reply.

**The validation spike has not been run.** Every assumption about the Claude
Code CLI's flags and output format is still unverified; see
`docs/design/spike.md`.

## Running it

```sh
cp deploy/spool/config.yaml.example deploy/spool/config.yaml
export SPOOL_ADMIN_TOKEN=... SPOOL_IOS_TOKEN=...
go run ./cmd/spool --config deploy/spool/config.yaml --queues deploy/spool/queues.yaml --check
```

The container needs a login created **inside** it — never copy
`.credentials.json` in from elsewhere, which would make a second holder of the
same refresh token:

```sh
docker build -t spool .
docker run -d --name spool -v spool-data:/data -v "$PWD/deploy/spool:/etc/spool:ro" \
  -e SPOOL_ADMIN_TOKEN -e SPOOL_IOS_TOKEN -p 8080:8080 spool
docker exec -it spool claude auth login
```

## Using it

```sh
TOKEN=$SPOOL_ADMIN_TOKEN

# What can I send to?
curl -s localhost:8080/v1/queues -H "Authorization: Bearer $TOKEN"

# Send something. Returns immediately.
curl -s -X POST localhost:8080/v1/queues/media/jobs \
  -H "Authorization: Bearer $TOKEN" -H 'Idempotency-Key: share-1' \
  -H 'Content-Type: application/json' \
  -d '{"input":"Dune by Frank Herbert","args":{"url":"https://example.com"}}'

curl -s localhost:8080/v1/jobs -H "Authorization: Bearer $TOKEN"
curl -s localhost:8080/v1/jobs/$ID/transcript -H "Authorization: Bearer $TOKEN"
curl -s localhost:8080/v1/executor -H "Authorization: Bearer $TOKEN"
```

Queues come from `queues.yaml` and are read-only over the API: they carry the
prompts and the tool allowlists, so widening them is a reviewed commit, not an
API call. Tokens can be scoped to a subset of queues.

## Behaviour worth knowing

- **One job at a time, across all queues.** One login means one holder of the
  refresh token; parallelism would race it.
- **At-most-once.** Jobs have side effects, so a job interrupted by a restart
  waits for a human rather than re-running. Automatic requeue happens only for
  auth or usage failures that made no tool calls.
- **Auth and usage failures block everything and drop nothing.** Queues keep
  accepting submissions while the executor is blocked.
- **A clean exit is not success.** Claude also has to report a structured
  outcome saying the task itself worked.
