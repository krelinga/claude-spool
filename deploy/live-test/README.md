# Live tests

Try any queue in `deploy/spool/queues.yaml` against a real claude.ai login,
first read-only and then for real, in a throwaway container.

```sh
deploy/live-test/run.sh build
deploy/live-test/run.sh up          # :8081, comes up with no login
deploy/live-test/run.sh login       # <-- you: open the URL, paste the code

deploy/live-test/run.sh dryrun places "we should check out Kasama"   # writes nothing
deploy/live-test/run.sh run    places "we should check out Kasama"   # writes, asks first

deploy/live-test/run.sh down        # when done; the login volume is kept for next time
```

## Why it is separate from real-run

`deploy/real-run` holds arm 2 of the longevity experiment: a login that must
stay idle. Starting that container would warm it and spoil the result. And
borrowing the spike container's login would put a second holder on arm 1's
refresh token. So this has its own container (`spool-live-test`), its own
login volume (`spool-live-test-data`), its own port (8081) and its own image
tag, and shares nothing with either.

## Dry-run twins, generated

`up` and `reload` run `./dryrun` over the source queues file (default
`deploy/spool/queues.yaml`, or `$SPOOL_QUEUES`) and serve the result from
`generated/` (gitignored). It holds every real queue unchanged plus a
`<queue>-dryrun` twin of each, which:

- keeps the same prompt, skill and tools, so it tests the same plumbing;
- prepends a read-only preamble to the system prompt;
- disallows every known mutating tool, built-in or connector. A deny beats an
  allow, so this is the guarantee, and the prompt only the advice;
- adds a `would_write` outcome field, where it reports what it would have
  written.

So a queue added to the real file is testable here with no extra config: edit
`queues.yaml`, then `run.sh reload`.

The generator refuses a queue that allows a tool from a connector it does not
know, rather than guess which of its tools write. Enabling one means adding its
mutating tools to `mutatingTools` in `dryrun/main.go`. `cd dryrun && go test`
checks every twin of the real file.

## Reading a result

`dryrun`, `run` and `reply` wait for the job and print the parts worth reading
first: status, summary, the question if it needs input, `would_write`, cost,
and any `permission_denials`. A denial of a read-only tool means the queue's
`allowed_tools` is missing it; a denial of a writing tool in a dry run is the
twin doing its job. `run.sh job <id>` has everything, and
`run.sh transcript <id>` the raw stream.

A `needs_input` job is answered with `run.sh reply <id> "<answer>"`, which
resumes the same session, exactly as the phone will.

## Notes

- No keep-alive and no webhooks: a test container is up for an hour and
  driven by hand.
- `.token` is generated on first `up` and is gitignored.
- `run.sh clean` also removes the login, so you would log in again afterwards.
