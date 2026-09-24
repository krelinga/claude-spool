# Spike harness

Answers the open questions in `../docs/design/spike.md` by running a pinned
Claude Code CLI in a bare container and capturing what it actually does. Raw
output lands in `out/` (gitignored).

## The offline part is already done

`spike/run.sh offline` needs no login and has already been run against CLI
2.1.282; see the "Answered" table in `../docs/design/spike.md`. It found that
`--json-schema` takes inline JSON rather than a file path, which would have
broken every job.

## What needs you

```sh
spike/run.sh build         # pin with CLAUDE_CODE_VERSION=2.1.282 if you want to match
spike/run.sh up
spike/run.sh offline       # flags; no login needed

spike/run.sh login         # <-- you: open the URL, approve, paste the code back
spike/run.sh probe         # everything that needs a login
spike/run.sh skill notion-media    # does that skill exist and expand?

spike/run.sh keepalive     # leave running for weeks (probe 7)
spike/run.sh results       # what has been captured
```

Then hand over `spike/out/`.

The login is created **inside** the container and kept in the
`spool-spike-claude` volume. Never copy `.credentials.json` in from your laptop:
that makes a second holder of the same refresh token, which is exactly the
race the design avoids (§3.2).

`spike/run.sh down` keeps the login volume; `clean` destroys it.

## Notes

- Probe 50 (auth failure) is non-destructive: it points `CLAUDE_CONFIG_DIR` at
  an empty temp directory rather than touching your credentials.
- Probe 70 doubles as the error-shape capture: any failed check saves the full
  stream under `out/70-keepalive/failures/`. A genuine login expiry or usage
  limit is the one thing no probe can force.
- For the idleness question in §6 item 7, run a second container with
  `SPIKE_CONTAINER=spool-spike-idle spike/run.sh up` and never start keepalive
  on it.
