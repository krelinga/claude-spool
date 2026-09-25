# First run against a real login and a live Notion

Everything else in this repo is verified against fakes and captured CLI output.
This is the one thing that cannot be: an actual job, through an actual
claude.ai login, writing to an actual Notion database.

It is staged so the write comes last.

```sh
deploy/real-run/run.sh build      # pins CLI 2.1.282, the version the spike measured
deploy/real-run/run.sh up         # starts on :8080, comes up blocked_auth
deploy/real-run/run.sh login      # <-- you: open the URL, paste the code

deploy/real-run/run.sh dryrun "Dune by Frank Herbert"   # writes nothing
deploy/real-run/run.sh add    "Dune by Frank Herbert"   # writes to Notion, asks first
```

## Why two queues

`media-dryrun` invokes the same skill with the same prompt, but every mutating
Notion tool the spike observed is in its `disallowed_tools`, and its system
prompt tells Claude to search and read only. It answers the questions that
actually carry risk:

- Does `/anthropic-skills:notion-media` expand and load under `-p`?
- Is the `claude.ai Notion` connector reachable, and connected rather than
  `pending`?
- Are the `mcp__claude_ai_Notion__*` tool names in `allowed_tools` right?
- Does the skill try to reach for a tool that is not on the list?

A denial shows up in the job's `permission_denials`, so a missing tool name is
visible rather than mysterious. Fix `queues.yaml`, then `SIGHUP` or wait ten
seconds for the reload — no restart needed.

Only once the dry run succeeds does `add` make sense. It confirms before
submitting, because it creates a real page.

## Notes

- The login is created **inside** the container, in the `spool-real-data`
  volume. Do not copy `.credentials.json` in from anywhere: two holders of one
  refresh token is the race the design exists to avoid.
- `run.sh login` drives Spool's own `/v1/auth/login` endpoint, which is the flow
  a phone would use, and the only part of the re-login path that has never met a
  real CLI. `login-manual` is the documented `docker exec` fallback if it fails.
- The API token is generated into `deploy/real-run/.token` (gitignored) on first
  `up`. Set `SPOOL_TOKEN` yourself to override.
- `run.sh clean` removes the container, the volume and the token — including the
  login, so you would log in again afterwards.
- This config has no webhook receivers, since the run is driven by hand. State
  comes from `run.sh auth`, `run.sh jobs`, and `run.sh watch` for the live
  stream.
