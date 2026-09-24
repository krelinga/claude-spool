# Validation spike — open questions and where they land in code

Design doc §6 lists seven things about the Claude Code CLI that have to be
confirmed empirically. None of them are answered yet. The code carries a
best-effort implementation of each, written so that a wrong guess is a small,
local edit rather than a redesign.

Run the spike against **a pinned CLI version in a bare Debian container**, then
work down this table. Each row names the one place in the tree that changes.

| # | Question (§6) | Current assumption | Where it lives |
|---|---|---|---|
| 1 | Do Notion connector tools appear in `system/init`, and what are their exact names? | `mcp_servers[]` carries `{name, status}`; a server counts as usable when status is `connected`/`ok`/`ready`. Tool names go in a queue's `allowed_tools` verbatim. | `internal/claudecli/stream.go` (`MCPServer.Connected`), `internal/claudecli/classify.go` (`MissingCapabilities`) |
| 2 | Do synced skills work under `-p` with `CLAUDE_CODE_SYNC_SKILLS=1`, and what is the invocation name? | Skills appear in `slash_commands[]` (leading `/` optional), or in a `skills[]` array. Queues invoke them as `/name` in the prompt template. | `internal/claudecli/classify.go` (`MissingCapabilities`) |
| 3 | Does `structured_output` appear on the stream-json `result` line? | Tries `structured_output`, `structuredOutput`, `structured_result`, `structuredResult`, then falls back to a fenced ```json block in the result text, then to a bare JSON object. | `internal/claudecli/classify.go` (`ExtractOutcome`) |
| 4 | Does `claude auth login` work under a PTY with no browser? | Not implemented — auth manager is §7 step 3. | — |
| 5 | Is `claude auth status` local-only? Is the "expires in N days" warning machine-readable? | Not implemented — §7 step 3. | — |
| 6 | What do real `Login expired` and usage-limit results look like? | Regex sets built from the documented strings and GitHub issue reports. Usage resets are parsed from a trailing `\|<epoch>` and ignored if implausible. | `internal/claudecli/classify.go` (`authPatterns`, `usagePatterns`, `usageResetRe`) |
| 7 | Longevity: what is the real re-auth cadence? | Not measured. `auth_events` table exists and is unused. | `internal/store/store.go` (schema) |

## Also unverified: the command line itself

The flags in `internal/claudecli/invocation.go` (`Invocation.Args`) come
straight from design §3.1 and have not been run against a real CLI. Two in
particular are worth checking first:

- **`--json-schema`** — Spool passes a **file path**. If the flag wants inline
  JSON, change `Args` and the executor's schema-file write.
- **`--permission-mode dontAsk` / `--permission-prompts none`** — confirm both
  spellings exist, and confirm a disallowed tool is *denied* rather than left
  hanging. The whole unattended model depends on it.

Everything else about the CLI surface is confined to `internal/claudecli`; no
other package builds a command line or parses CLI output.

## What is already enforced regardless of the spike

- `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and `CLAUDE_CODE_OAUTH_TOKEN` are
  **stripped** from the subprocess environment, not merely left unset. Either
  one outranks the claude.ai login and would silently disable skills and
  connectors (§3.1). Covered by a test.
- Every job runs in a fresh empty working directory.
- `DISABLE_AUTOUPDATER=1` is always set: the classifier scrapes CLI output, so
  version drift must be deliberate.
