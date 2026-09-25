# Validation spike — results

All of §6 has now been run against **CLI 2.1.282** with a real claude.ai login
(Max subscription). Raw output is in `spike/out/` (gitignored); `spike/run.sh`
re-runs any of it.

The spike changed the design in one important way and corrected four wrong
assumptions. **The most consequential finding is the permission model** — read
that section before editing a queue's tool lists.

## The permission model is not what the design assumed

Design §3.1 says `dontAsk` + `--permission-prompts none` means "calls outside
the queue's allowlist are denied". That is **half right**, and the half that is
wrong is a security hole.

Measured behaviour:

| Flag | What it actually does |
|---|---|
| `--allowedTools` | **Pre-approves**. It does *not* restrict. A built-in the CLI considers safe still runs when absent from the list. |
| `--tools` | **Restricts** the built-in set. `--tools Read` removed Bash entirely; the model reported having no shell. This is the real allowlist. |
| `--disallowedTools` | Denies outright, and **wins over** `--allowedTools`. Works for MCP tools, which `--tools` does not cover. |
| `--permission-mode dontAsk` / `manual`, with `--permission-prompts none` | Denies anything that *would* prompt, and records it in `permission_denials`. Both modes denied a `Write`. |

The proof: with `--allowedTools Read --permission-mode dontAsk
--permission-prompts none`, asking for Bash `echo hello` **succeeded** — exit 0,
no denials, command ran. The same setup denied `Bash` when the command was
`echo hello > /tmp/file`. So the gate is "does this need approval", not "is this
in the allowlist", and harmless-looking built-ins pass it.

**Consequence for Spool:** a queue now has three lists, and `tools` is the one
that matters. `deploy/spool/queues.yaml` sets it for both queues; as shipped
before this, `adhoc` had a shell available for anything the CLI deemed safe.

```yaml
tools: [Skill, WebSearch, WebFetch]        # what may be used at all
allowed_tools: [Skill, mcp__claude_ai_Notion__notion-search, ...]  # pre-approval
disallowed_tools: [Bash, Write, Edit]      # defence in depth; beats allowed_tools
```

## Answered

| Question (§6) | Answer |
|---|---|
| **1. Connector tools in `-p`?** | **Yes.** Servers are named `claude.ai Notion`, `claude.ai Todoist`, … with a `source: "claudeai"` field, and contribute 45 Notion tools prefixed **`mcp__claude_ai_Notion__`** (e.g. `notion-search`, `notion-fetch`, `notion-create-pages`, `notion-update-page`). The design's `mcp__notion__*` guess was wrong. Statuses seen: `connected`, `pending`, `needs-auth`, `failed`. |
| **2. Synced skills in `-p`?** | **Yes**, and they are **namespaced**: `anthropic-skills:notion-media`, not `notion-media`. They appear in both `slash_commands[]` and a `skills[]` array. |
| **2b. Does `notion-media` exist?** | **Yes** — along with `notion-ideas` and `notion-places`. A namespaced slash command **expands at prompt level** and consumes no tool call, so `/anthropic-skills:notion-media {{input}}` works as a queue prompt. |
| **3. `structured_output` on the result line?** | **Yes — that exact key.** The first guess was right. It also appears as JSON text in `result`, so the fenced-block fallback would work too. **It costs turns**: the observed run needed `num_turns: 4`, and a run capped at 1 turn produced `error_max_turns` with no outcome at all. Queues therefore enforce `max_turns >= 5`. |
| **4. `claude auth login` under a PTY, no browser?** | **Yes.** It prints `Opening browser to sign in…`, then `If the browser didn't open, visit: <url>`, then the newline-free prompt `Paste code here if prompted > `, then `Login successful.` and exit 0. Three traps, all handled in `internal/claudecli/login.go`: the URL is emitted **twice** on one line (once inside an OSC 8 hyperlink escape, once as visible coloured text), the prompt has **no trailing newline** so a line-oriented reader hangs, and the code is read **without echo** so it never appears in the transcript — success can only be confirmed from the marker and exit status. |
| **5. Is `auth status` machine-readable?** | **Yes**, JSON by default (a `--json` flag is accepted but unnecessary): `{loggedIn, authMethod: "claude.ai", apiProvider, email, orgId, orgName, subscriptionType}`. Exit **0** logged in, **1** logged out. It is **local-only** — 64 ms, far too fast for a round trip. **There is no expiry field**, so the keep-alive request remains the only authoritative liveness check, exactly as the design assumed. |
| **6. Real failure shapes?** | Auth: a normal result line with `is_error: true`, `result: "Not logged in · Please run /login"`, `terminal_reason: "api_error"`, and — trap — `subtype: "success"`. Max turns: `subtype: "error_max_turns"`, `terminal_reason: "max_turns"`, `errors: ["Reached maximum number of turns (1)"]`, and **no `result` field at all**. Denials: `permission_denials: [{tool_name, tool_use_id, tool_input}]`. |

Two fields the design did not know about, now used by the classifier:

- **`errors[]`** — the CLI's own failure text, and often the *only* place the
  reason appears, since a failed run may carry no `result`.
- **`terminal_reason`** — `completed` / `max_turns` / `api_error`. More
  trustworthy than `subtype`, which reads `success` on an auth failure.

## Corrections made to the code

| Was | Now |
|---|---|
| `--json-schema <file path>` | Inline JSON. A path is rejected outright, so every job would have failed. |
| `allowed_tools` treated as a restriction | `tools` restricts; `allowed_tools` pre-approves; `disallowed_tools` denies. |
| Connector match on exact lowercase name | Tolerant suffix match, so `notion` matches `claude.ai Notion` and `google calendar` matches `claude.ai Google Calendar`. |
| Skill match on exact name | Same tolerance, so `notion-media` matches `anthropic-skills:notion-media`. |
| Any non-connected connector → `capability_missing`, auto-pause | `pending` is now `capability_pending`: retried up to 3 times, **no auto-pause**. It is a startup race — a `pending` server contributes **zero** tools, verified, and reads `connected` on the next run. |
| Connector presence judged by status alone | Status **and** at least one tool carrying the server's derived prefix, since that is what Claude can actually call. |
| `max_turns >= 1` | `max_turns >= 5`, because structured output needs turns of its own. |

The authorization URL's scopes are worth noting, because they corroborate why
the design insists on a real login: `user:sessions:claude_code`,
`user:mcp_servers`, `user:file_upload`, `user:plugins`, `user:inference`,
`user:profile`, `org:create_api_key`. Connector and plugin access is part of
this grant, and an API key or `setup-token` credential does not carry it.

## Still open

- **§6 item 7 — re-auth cadence.** The longevity test is **now running** in the
  `spool-spike` container at the 4h interval, logging to
  `spike/out/70-keepalive/log.jsonl` (one JSON line per check: `logged_in`,
  exit codes, latency, `terminal_reason`). It also saves the full stream of any
  failed check under `failures/`, which is the only way to capture a real
  expiry or usage-limit shape — `usagePatterns` and `usageResetRe` in
  `internal/claudecli/classify.go` are still guesses until one occurs.

  It does not survive a container restart; `spike/run.sh keepalive-status`
  reports whether it is alive.
- **Whether `/anthropic-skills:notion-media` does the right thing.** Expansion
  is proven with a harmless skill; the media skill itself was not invoked,
  deliberately, to avoid writing to a live Notion database. Worth one manual
  dry-run before trusting the `media` queue.

## What is enforced regardless

- `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and `CLAUDE_CODE_OAUTH_TOKEN` are
  stripped from the subprocess environment, and `main` refuses to start in
  `claudeai_login` mode if one is set.
- Every job runs in a fresh empty working directory.
- `DISABLE_AUTOUPDATER=1`. The classifier reads CLI output, so version drift
  must be deliberate — and every finding here is pinned to **2.1.282**.
- `claude --help` is an **incomplete** flag list: `--max-turns` and
  `--append-system-prompt-file` both work but are unlisted. `spike/probes/00-flags.sh`
  tests by invocation, with an unknown-flag control case.
