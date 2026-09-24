# Validation spike — what is answered, what is still open

Design doc §6 lists seven things about the Claude Code CLI that have to be
confirmed empirically. **The offline probes have been run against CLI 2.1.282**
and already corrected three wrong assumptions. The rest need a claude.ai login
inside the spike container, which only the account holder can do.

Harness: `spike/run.sh` (see `spike/README.md`). Raw output lands in
`spike/out/`, which is gitignored — hand that directory over for interpretation
rather than summarising it.

## Answered — CLI 2.1.282, no login required

| Question | Answer | Consequence |
|---|---|---|
| Does `--json-schema` take a file path? | **No. Inline JSON only.** A path fails with `--json-schema is not valid JSON: JSON Parse error: Unrecognized token '/'`. | Spool passes the schema inline (`Invocation.JSONSchema`). Every job would have failed under the original file-path assumption. |
| Does `--permission-mode dontAsk` exist? | **Yes.** Choices are `acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan`. | As designed. |
| Does `--permission-prompts none` deny rather than hang? | **Yes,** and the help says so outright: "nobody: anything that would prompt is denied automatically; the permission mode still decides everything else". | The unattended model holds. Still worth confirming with a live denial (probe 6b). |
| Does `--append-system-prompt-file` exist? | **Yes,** and it takes a path — a missing file errors with "Append system prompt file not found". It is *not listed in `--help`*. | As designed. |
| Does `--max-turns` exist? | **Yes,** also unlisted in `--help`. | As designed. |
| Are unknown flags rejected? | **Yes** — exit 1, `error: unknown option` on stderr. | A wrong flag fails loudly rather than being silently ignored. |
| Is `claude auth status` machine-readable? | **Yes**, JSON by default, no flag needed: `{loggedIn, authMethod, apiProvider, projectsDirectory, configDirectory}`. Exit **1** when logged out. | The auth manager can use it directly. Whether it validates with the server, and whether an expiry warning appears when logged *in*, is still open. |
| What does an auth failure look like in `stream-json`? | A **normal result line**: `is_error: true`, `result: "Not logged in · Please run /login"`, `terminal_reason: "api_error"`, and — note — `subtype: "success"`. The preceding assistant event carries `"error":"authentication_failed"`. | The classifier sees a result event and matches it as `auth`. Captured verbatim as a test fixture. **`subtype` is not a reliable error signal**; `is_error` is. |
| Does the result line report permission denials? | **Yes**, a `permission_denials` field (empty array on a clean run). | The classifier reads it directly, keeping tool_result text inference only as a fallback. The shape when non-empty is still unknown. |

**Lesson worth keeping:** `claude --help` is an incomplete list of flags. Test
by invocation. `spike/probes/00-flags.sh` does this, with an unknown-flag
control case so the probe cannot silently prove nothing.

## Still open — needs a login in the container

| # | Question (§6) | Current assumption | Where it lives |
|---|---|---|---|
| 1 | Do Notion connector tools appear in `system/init`, and what are their exact names? | `mcp_servers[]` carries `{name, status}`; usable when status is `connected`/`ok`/`ready`. On a logged-out run `mcp_servers` is `[]`, so this is untested. | `internal/claudecli/stream.go` (`MCPServer.Connected`), `classify.go` (`MissingCapabilities`) |
| 2 | Do synced skills appear under `-p`, and under what name? | Checked against both `slash_commands[]` and a `skills[]` array — **the init event does carry both**, so the two-place check is right. Whether *claude.ai-synced* skills land there is untested. | `internal/claudecli/classify.go` (`MissingCapabilities`) |
| 2b | Does `/notion-media` exist on the account at all? | Assumed. If absent, the `media` queue fails `capability_missing` and auto-pauses on first run. | `deploy/spool/queues.yaml` |
| 3 | Does structured output appear on the result line, and under what key? | Tries four spellings, then a fenced ```json block, then a bare object. A logged-out run produces no structured output, so untested. | `internal/claudecli/classify.go` (`ExtractOutcome`) |
| 4 | Does `claude auth login` work under a PTY with no browser? | Not implemented — auth manager is §7 step 3. `spike/run.sh capture-login` records the flow. | — |
| 5 | Does `auth status` hit the network? Is the "expires in N days" warning machine-readable? | Partially answered above; the expiry field can only appear when logged in. | — |
| 6 | What does a real *expired* login, and a real usage limit, look like? | Regexes match the captured "Not logged in" case. Expiry and rate-limit strings are still guesses. | `internal/claudecli/classify.go` (`authPatterns`, `usagePatterns`, `usageResetRe`) |
| 7 | What is the real re-auth cadence? | Not measured. `spike/probes/70-keepalive.sh` logs it and captures failures as they happen; `auth_events` is still unused. | `internal/store/store.go` |

## What is enforced regardless of the spike

- `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and `CLAUDE_CODE_OAUTH_TOKEN` are
  **stripped** from the subprocess environment, and `main` refuses to start in
  `claudeai_login` mode if one is set. Either would silently disable skills and
  connectors (§3.1).
- Every job runs in a fresh empty working directory.
- `DISABLE_AUTOUPDATER=1` always. The classifier scrapes CLI output, so version
  drift must be deliberate — and note that this spike pinned 2.1.282.
