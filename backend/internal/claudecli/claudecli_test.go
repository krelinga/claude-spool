package claudecli

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/claude-spool-be/backend/internal/config"
	"github.com/krelinga/claude-spool-be/backend/internal/store"
)

func collect(t *testing.T, lines ...string) *Collector {
	t.Helper()
	c := NewCollector()
	err := ScanEvents(strings.NewReader(strings.Join(lines, "\n")), nil, func(e *Event) error {
		c.Observe(e)
		return nil
	})
	if err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	return c
}

const initLine = `{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-5",` +
	`"tools":["Skill","WebSearch"],"slash_commands":["notion-media","help"],` +
	`"mcp_servers":[{"name":"notion","status":"connected"}]}`

func resultLine(t *testing.T, fields map[string]any) string {
	t.Helper()
	base := map[string]any{
		"type": "result", "subtype": "success", "session_id": "s1",
		"is_error": false, "num_turns": 4, "total_cost_usd": 0.12, "duration_ms": 4200,
	}
	for k, v := range fields {
		base[k] = v
	}
	b, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func okOutcome(status, summary string) map[string]any {
	return map[string]any{"structured_output": map[string]any{"status": status, "summary": summary}}
}

// --- environment: the invariant that protects skills and connectors ---

func TestEnvStripsCredentialOverrides(t *testing.T) {
	inv := Invocation{ConfigDir: "/data/claude", SyncSkills: true}
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-should-not-survive",
		"CLAUDE_CODE_OAUTH_TOKEN=tok-should-not-survive",
		"ANTHROPIC_AUTH_TOKEN=also-not",
		"CLAUDE_CONFIG_DIR=/somewhere/else",
	}
	env := inv.Env(base)
	joined := strings.Join(env, "\n")
	for _, banned := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		if strings.Contains(joined, banned) {
			t.Errorf("%s leaked into the subprocess env: %v", banned, env)
		}
	}
	if !contains(env, "CLAUDE_CONFIG_DIR=/data/claude") {
		t.Errorf("CLAUDE_CONFIG_DIR not forced to the configured dir: %v", env)
	}
	if strings.Count(joined, "CLAUDE_CONFIG_DIR=") != 1 {
		t.Errorf("CLAUDE_CONFIG_DIR set more than once: %v", env)
	}
	if !contains(env, "CLAUDE_CODE_SYNC_SKILLS=1") || !contains(env, "DISABLE_AUTOUPDATER=1") {
		t.Errorf("expected sync + pinned-version env: %v", env)
	}
	if !contains(env, "PATH=/usr/bin") {
		t.Error("unrelated env should be inherited")
	}
}

// ExtraEnv must not be a back door around the ban.
func TestExtraEnvCannotReintroduceBannedVars(t *testing.T) {
	inv := Invocation{ConfigDir: "/data/claude", ExtraEnv: []string{"ANTHROPIC_API_KEY=sneaky", "FOO=bar"}}
	env := inv.Env(nil)
	if strings.Contains(strings.Join(env, "\n"), "sneaky") {
		t.Errorf("ExtraEnv reintroduced a banned variable: %v", env)
	}
	if !contains(env, "FOO=bar") {
		t.Error("ExtraEnv should still pass unrelated vars")
	}
}

func TestSyncSkillsOff(t *testing.T) {
	env := Invocation{ConfigDir: "/d"}.Env(nil)
	if contains(env, "CLAUDE_CODE_SYNC_SKILLS=1") {
		t.Error("sync should be off when not requested")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestArgs(t *testing.T) {
	inv := Invocation{
		Prompt: "do a thing", AllowedTools: []string{"Skill", "WebSearch"},
		MaxTurns: 30, Model: "sonnet", JSONSchema: `{"type":"object"}`,
		SystemPromptFile: "/run/system.md",
	}
	args := inv.Args()
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-p do a thing", "--output-format stream-json", "--verbose",
		"--permission-mode dontAsk", "--permission-prompts none",
		"--allowedTools Skill,WebSearch", "--max-turns 30", "--model sonnet",
		`--json-schema {"type":"object"}`, "--append-system-prompt-file /run/system.md",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	// A job with no resume must not pass an empty --resume.
	if strings.Contains(joined, "--resume") {
		t.Errorf("unexpected --resume: %v", args)
	}
}

// --- stream parsing ---

func TestCollectorCountsToolCalls(t *testing.T) {
	c := collect(t,
		initLine,
		`{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[`+
			`{"type":"text","text":"looking"},`+
			`{"type":"tool_use","id":"t1","name":"mcp__notion__notion-search"},`+
			`{"type":"tool_use","id":"t2","name":"WebSearch"}]}}`,
		resultLine(t, okOutcome("succeeded", "done")),
	)
	if c.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", c.ToolCalls)
	}
	if c.SessionID != "s1" {
		t.Errorf("SessionID = %q", c.SessionID)
	}
	if c.Init == nil || c.Result == nil {
		t.Fatal("init and result should both be captured")
	}
}

func TestCollectorRecordsPermissionDenials(t *testing.T) {
	c := collect(t,
		initLine,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1",`+
			`"is_error":true,"content":"Permission to use Bash was denied"}]}}`,
		resultLine(t, okOutcome("succeeded", "done")),
	)
	if len(c.Denials) != 1 || c.Denials[0] != "Bash" {
		t.Errorf("Denials = %v, want [Bash]", c.Denials)
	}
}

func TestScanEventsWritesTranscriptAndSurvivesJunk(t *testing.T) {
	var transcript strings.Builder
	var seen int
	in := initLine + "\nnot json at all\n" + resultLine(t, okOutcome("succeeded", "ok"))
	err := ScanEvents(strings.NewReader(in), &transcript, func(e *Event) error {
		seen++
		return nil
	})
	if err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	if seen != 2 {
		t.Errorf("parsed %d events, want 2 (junk line skipped)", seen)
	}
	// The transcript is verbatim: the junk line is preserved for debugging.
	if !strings.Contains(transcript.String(), "not json at all") {
		t.Error("transcript should keep unparseable lines")
	}
	if strings.Count(transcript.String(), "\n") != 3 {
		t.Errorf("transcript line count = %q", transcript.String())
	}
}

// --- capability checking ---

// realInitLine is the init event shape captured from CLI 2.1.282 in the spike:
// connectors are named "claude.ai <Name>" and skills are namespaced.
const realInitLine = `{"type":"system","subtype":"init","session_id":"s1",` +
	`"slash_commands":["code-review","anthropic-skills:notion-media","anthropic-skills:notion-ideas"],` +
	`"skills":["code-review","anthropic-skills:notion-media","anthropic-skills:notion-ideas"],` +
	`"mcp_servers":[` +
	`{"name":"claude.ai Notion","status":"connected","source":"claudeai"},` +
	`{"name":"claude.ai Google Calendar","status":"needs-auth","source":"claudeai"},` +
	`{"name":"claude.ai Todoist","status":"connected","source":"claudeai"}],` +
	`"tools":["Skill","WebSearch","mcp__claude_ai_Notion__notion-search",` +
	`"mcp__claude_ai_Notion__notion-create-pages","mcp__claude_ai_Todoist__find-tasks"]}`

// A queue may declare the short name; the CLI's prefixes must not break it.
func TestCapabilityMatchingToleratesCLIPrefixes(t *testing.T) {
	c := collect(t, realInitLine)
	for _, req := range []config.Requires{
		{Skills: []string{"notion-media"}, Connectors: []string{"notion"}},
		{Skills: []string{"anthropic-skills:notion-media"}, Connectors: []string{"claude.ai Notion"}},
		{Skills: []string{"/notion-media"}, Connectors: []string{"Notion"}},
	} {
		if caps := c.CheckCapabilities(req); !caps.OK() {
			t.Errorf("%+v reported unsatisfied: missing=%v pending=%v", req, caps.Missing, caps.Pending)
		}
	}
}

func TestCapabilityMatchingIsNotOverlyLoose(t *testing.T) {
	c := collect(t, realInitLine)
	caps := c.CheckCapabilities(config.Requires{
		Skills: []string{"notion"}, Connectors: []string{"linear"},
	})
	// "notion" must not match "anthropic-skills:notion-media" on a prefix, and
	// a connector that is not configured at all is missing.
	if len(caps.Missing) != 2 {
		t.Errorf("Missing = %v, want both the skill and the connector", caps.Missing)
	}
}

// needs-auth is a real configuration problem: the connector exists but cannot
// be used, and no amount of retrying will help.
func TestNeedsAuthConnectorIsMissing(t *testing.T) {
	c := collect(t, realInitLine)
	caps := c.CheckCapabilities(config.Requires{Connectors: []string{"google calendar"}})
	if len(caps.Missing) != 1 || len(caps.Pending) != 0 {
		t.Errorf("caps = %+v, want it reported missing", caps)
	}
	if !strings.Contains(caps.Missing[0], "needs-auth") {
		t.Errorf("the reason should be named: %q", caps.Missing[0])
	}
}

// pending is a race, reported separately so the executor can retry instead of
// pausing the queue. Captured in the spike: a pending server lists no tools.
func TestPendingConnectorIsTransient(t *testing.T) {
	c := collect(t, `{"type":"system","subtype":"init","session_id":"s1",`+
		`"mcp_servers":[{"name":"claude.ai Notion","status":"pending","source":"claudeai"}],`+
		`"tools":["Skill"]}`)
	caps := c.CheckCapabilities(config.Requires{Connectors: []string{"notion"}})
	if len(caps.Pending) != 1 || len(caps.Missing) != 0 {
		t.Errorf("caps = %+v, want pending only", caps)
	}
	if caps.OK() {
		t.Error("a pending connector is not OK to run against")
	}
}

// A connector that says connected but contributes no tools is useless to the
// job, so it counts as missing.
func TestConnectedButNoToolsIsMissing(t *testing.T) {
	c := collect(t, `{"type":"system","subtype":"init","session_id":"s1",`+
		`"mcp_servers":[{"name":"claude.ai Notion","status":"connected"}],`+
		`"tools":["Skill","WebSearch"]}`)
	caps := c.CheckCapabilities(config.Requires{Connectors: []string{"notion"}})
	if len(caps.Missing) != 1 || !strings.Contains(caps.Missing[0], "no tools") {
		t.Errorf("caps = %+v", caps)
	}
}

// The tool-name prefix is derived from the server name; this is how tool
// presence is checked.
func TestMCPToolPrefix(t *testing.T) {
	cases := map[string]string{
		"claude.ai Notion":               "mcp__claude_ai_Notion__",
		"claude.ai Adobe for creativity": "mcp__claude_ai_Adobe_for_creativity__",
		"plugin:engineering:slack":       "mcp__plugin_engineering_slack__",
	}
	for name, want := range cases {
		if got := (MCPServer{Name: name}).ToolPrefix(); got != want {
			t.Errorf("ToolPrefix(%q) = %q, want %q", name, got, want)
		}
	}
}

// With no init event there is nothing to check against; that is a different
// failure and must not be reported as a capability problem.
func TestCheckCapabilitiesWithoutInit(t *testing.T) {
	c := NewCollector()
	if caps := c.CheckCapabilities(config.Requires{Skills: []string{"x"}}); !caps.OK() {
		t.Errorf("caps = %+v, want OK", caps)
	}
}

// --- classification, layer 1 ---

func TestClassifyRunLevelFailures(t *testing.T) {
	cases := []struct {
		name string
		run  func() Run
		want store.ErrorKind
	}{
		{
			name: "login expired in result text",
			run: func() Run {
				c := collect(t, initLine, resultLine(t, map[string]any{
					"is_error": true, "subtype": "error_during_execution",
					"result": "Login expired. Please run /login to continue.",
				}))
				return Run{Collector: c}
			},
			want: store.ErrKindAuth,
		},
		{
			name: "auth failure only on stderr",
			run: func() Run {
				c := collect(t, initLine)
				return Run{Collector: c, Stderr: "authentication_failed: token rejected",
					ExitErr: errors.New("exit status 1")}
			},
			want: store.ErrKindAuth,
		},
		{
			name: "usage limit",
			run: func() Run {
				c := collect(t, initLine, resultLine(t, map[string]any{
					"is_error": true, "result": "Claude AI usage limit reached",
				}))
				return Run{Collector: c}
			},
			want: store.ErrKindUsageLimit,
		},
		{
			name: "max turns",
			run: func() Run {
				c := collect(t, initLine, resultLine(t, map[string]any{
					"is_error": true, "subtype": "error_max_turns", "result": "reached max turns",
				}))
				return Run{Collector: c}
			},
			want: store.ErrKindMaxTurns,
		},
		{
			name: "timeout beats a missing result",
			run: func() Run {
				return Run{Collector: collect(t, initLine), TimedOut: true}
			},
			want: store.ErrKindTimeout,
		},
		{
			name: "capability missing beats everything",
			run: func() Run {
				c := collect(t, initLine, resultLine(t, map[string]any{
					"is_error": true, "result": "Login expired",
				}))
				return Run{Collector: c, Caps: Capabilities{Missing: []string{"connector notion"}}}
			},
			want: store.ErrKindCapabilityMissing,
		},
		{
			name: "no result event",
			run: func() Run {
				return Run{Collector: collect(t, initLine)}
			},
			want: store.ErrKindCLI,
		},
		{
			name: "non-zero exit",
			run: func() Run {
				return Run{Collector: collect(t, initLine), ExitErr: errors.New("exit status 2"),
					Stderr: "something broke"}
			},
			want: store.ErrKindCLI,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.run())
			if got.Result.Status != store.StatusFailed {
				t.Errorf("Status = %v, want failed", got.Result.Status)
			}
			if got.Result.ErrorKind != tc.want {
				t.Errorf("ErrorKind = %v, want %v (msg %q)", got.Result.ErrorKind, tc.want, got.Result.ErrorMessage)
			}
			if got.Result.ErrorMessage == "" {
				t.Error("failures should carry a message")
			}
		})
	}
}

// Only auth and usage block the whole executor; the rest fail one job.
func TestBlockingKinds(t *testing.T) {
	for _, k := range []store.ErrorKind{store.ErrKindAuth, store.ErrKindUsageLimit} {
		if !k.Blocking() {
			t.Errorf("%v should block the executor", k)
		}
	}
	for _, k := range []store.ErrorKind{
		store.ErrKindMaxTurns, store.ErrKindTimeout, store.ErrKindCLI,
		store.ErrKindCapabilityMissing, store.ErrKindInvalidOutcome,
	} {
		if k.Blocking() {
			t.Errorf("%v should not block the executor", k)
		}
	}
}

func TestUsageResetParsing(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	c := collect(t, initLine, resultLine(t, map[string]any{
		"is_error": true,
		"result":   "Claude AI usage limit reached|" + strconv.FormatInt(reset, 10),
	}))
	got := Classify(Run{Collector: c})
	if got.Result.ErrorKind != store.ErrKindUsageLimit {
		t.Fatalf("ErrorKind = %v", got.Result.ErrorKind)
	}
	if got.ResetAt == nil {
		t.Fatal("ResetAt not parsed")
	}
	if got.ResetAt.Unix() != reset {
		t.Errorf("ResetAt = %v, want %v", got.ResetAt.Unix(), reset)
	}

	// A nonsense epoch must not pin the executor open for years.
	c2 := collect(t, initLine, resultLine(t, map[string]any{
		"is_error": true, "result": "usage limit reached|99999999999",
	}))
	if got := Classify(Run{Collector: c2}); got.ResetAt != nil {
		t.Errorf("implausible reset accepted: %v", got.ResetAt)
	}
}

// --- classification, layer 2 ---

func TestClassifyTaskLevel(t *testing.T) {
	cases := []struct {
		name       string
		outcome    map[string]any
		wantStatus store.JobStatus
	}{
		{"succeeded", map[string]any{"status": "succeeded", "summary": "Added Dune"}, store.StatusSucceeded},
		{"failed", map[string]any{"status": "failed", "summary": "Could not find it"}, store.StatusFailed},
		{"needs input", map[string]any{"status": "needs_input", "summary": "Which edition?",
			"question": "Which edition?"}, store.StatusNeedsInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := collect(t, initLine, resultLine(t, map[string]any{"structured_output": tc.outcome}))
			got := Classify(Run{Collector: c})
			if got.Result.Status != tc.wantStatus {
				t.Errorf("Status = %v, want %v", got.Result.Status, tc.wantStatus)
			}
			if got.Result.Summary != tc.outcome["summary"] {
				t.Errorf("Summary = %q", got.Result.Summary)
			}
			// A task-level failure is not a run-level error.
			if got.Result.ErrorKind != "" {
				t.Errorf("ErrorKind = %q, want empty", got.Result.ErrorKind)
			}
			if len(got.Result.Outcome) == 0 {
				t.Error("structured outcome not stored")
			}
		})
	}
}

// A clean exit with no usable outcome is a failure, not a success: `is_error:
// false` alone does not mean the task worked (§3.4).
func TestCleanRunWithoutOutcomeFails(t *testing.T) {
	for _, tc := range []struct{ name, result string }{
		{"no outcome at all", "I did the thing!"},
		{"invalid status", `{"status":"maybe","summary":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := collect(t, initLine, resultLine(t, map[string]any{"result": tc.result}))
			got := Classify(Run{Collector: c})
			if got.Result.Status != store.StatusFailed || got.Result.ErrorKind != store.ErrKindInvalidOutcome {
				t.Errorf("got %v/%v, want failed/invalid_outcome", got.Result.Status, got.Result.ErrorKind)
			}
		})
	}
}

// The documented fallback: a fenced JSON block in the result text.
func TestOutcomeFromFencedBlock(t *testing.T) {
	c := collect(t, initLine, resultLine(t, map[string]any{
		"result": "Here you go:\n```json\n{\"status\":\"succeeded\",\"summary\":\"Added Dune\"}\n```\n",
	}))
	got := Classify(Run{Collector: c})
	if got.Result.Status != store.StatusSucceeded || got.Result.Summary != "Added Dune" {
		t.Errorf("fenced fallback failed: %+v", got.Result)
	}
}

// The field name is unconfirmed, so alternates must work too.
func TestOutcomeFieldAliases(t *testing.T) {
	for _, key := range []string{"structured_output", "structuredOutput", "structured_result"} {
		c := collect(t, initLine, resultLine(t, map[string]any{
			key: map[string]any{"status": "succeeded", "summary": "ok"},
		}))
		if got := Classify(Run{Collector: c}); got.Result.Status != store.StatusSucceeded {
			t.Errorf("%s not recognised: %+v", key, got.Result)
		}
	}
}

func TestClassifyCarriesRunStats(t *testing.T) {
	c := collect(t,
		initLine,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Skill"}]}}`,
		resultLine(t, okOutcome("succeeded", "ok")),
	)
	got := Classify(Run{Collector: c}).Result
	if got.NumTurns != 4 || got.CostUSD != 0.12 || got.DurationMS != 4200 || got.ToolCalls != 1 {
		t.Errorf("stats not carried through: %+v", got)
	}
	if got.SessionID != "s1" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
}

// --- schema ---

func TestBuildOutcomeSchema(t *testing.T) {
	qs, err := config.ParseQueues([]byte(`
queues:
  media:
    prompt: "{{input}}"
    outcome_extension:
      notion_url: { type: string }
      media_type: { enum: [book, film] }
`))
	if err != nil {
		t.Fatal(err)
	}
	q, _ := qs.Get("media")
	b, err := BuildOutcomeSchema(q)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Type       string                    `json:"type"`
		Required   []string                  `json:"required"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	for _, want := range []string{"status", "summary", "question", "links", "notion_url", "media_type"} {
		if _, ok := schema.Properties[want]; !ok {
			t.Errorf("schema missing %q: %s", want, b)
		}
	}
	if len(schema.Required) != 2 {
		t.Errorf("Required = %v", schema.Required)
	}
}

// A queue must not be able to redefine a base field out from under the
// classifier.
func TestOutcomeExtensionCannotShadowBaseFields(t *testing.T) {
	qs, _ := config.ParseQueues([]byte(`
queues:
  bad:
    prompt: "{{input}}"
    outcome_extension:
      status: { type: string }
`))
	q, _ := qs.Get("bad")
	if _, err := BuildOutcomeSchema(q); err == nil {
		t.Error("expected an error when shadowing the base status field")
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	qs, _ := config.ParseQueues([]byte("queues:\n  a:\n    prompt: x\n    system_prompt: \"Prefer the edition given.\"\n"))
	q, _ := qs.Get("a")
	got := BuildSystemPrompt(q)
	if !strings.HasPrefix(got, UnattendedPreamble) {
		t.Error("shared unattended preamble must come first")
	}
	if !strings.Contains(got, "Prefer the edition given.") {
		t.Error("queue system prompt missing")
	}

	qs2, _ := config.ParseQueues([]byte("queues:\n  a:\n    prompt: x\n"))
	q2, _ := qs2.Get("a")
	if BuildSystemPrompt(q2) != UnattendedPreamble {
		t.Error("a queue without its own system prompt should get just the preamble")
	}
}

// The CLI reports denials itself; prefer that over inferring from text.
func TestDenialsFromResultLine(t *testing.T) {
	c := collect(t, initLine, resultLine(t, map[string]any{
		"permission_denials": []any{"Bash", "Write"},
		"structured_output":  map[string]any{"status": "succeeded", "summary": "ok"},
	}))
	got := Classify(Run{Collector: c}).Result.PermissionDenials
	if len(got) != 2 || got[0] != "Bash" {
		t.Errorf("PermissionDenials = %v, want [Bash Write]", got)
	}
}

// The shape when non-empty is unconfirmed, so objects must work too.
func TestDenialsFromResultLineObjects(t *testing.T) {
	c := collect(t, initLine, resultLine(t, map[string]any{
		"permission_denials": []any{map[string]any{"tool_name": "Bash"}},
		"structured_output":  map[string]any{"status": "succeeded", "summary": "ok"},
	}))
	got := Classify(Run{Collector: c}).Result.PermissionDenials
	if len(got) != 1 || got[0] != "Bash" {
		t.Errorf("PermissionDenials = %v, want [Bash]", got)
	}
}

// An empty list (what CLI 2.1.282 sends on a clean run) must not invent one.
func TestEmptyDenialsList(t *testing.T) {
	c := collect(t, initLine, resultLine(t, map[string]any{
		"permission_denials": []any{},
		"structured_output":  map[string]any{"status": "succeeded", "summary": "ok"},
	}))
	if got := Classify(Run{Collector: c}).Result.PermissionDenials; len(got) != 0 {
		t.Errorf("PermissionDenials = %v, want none", got)
	}
}

// Captured verbatim from CLI 2.1.282 with no credentials (spike probe). The
// classifier must call this auth, not cli_error: the run produced a normal
// result line with subtype "success" despite is_error being true.
func TestClassifyRealNotLoggedInResult(t *testing.T) {
	const line = `{"is_error":true,"num_turns":1,"subtype":"success",` +
		`"result":"Not logged in · Please run /login","type":"result",` +
		`"terminal_reason":"api_error","total_cost_usd":0,"duration_ms":39,` +
		`"session_id":"89380dcc","permission_denials":[]}`
	c := collect(t, line)
	got := Classify(Run{Collector: c, ExitErr: errors.New("exit status 1")})
	if got.Result.ErrorKind != store.ErrKindAuth {
		t.Errorf("ErrorKind = %q, want auth (message %q)", got.Result.ErrorKind, got.Result.ErrorMessage)
	}
	if got.Result.Status != store.StatusFailed {
		t.Errorf("Status = %v", got.Result.Status)
	}
}

// Captured verbatim from CLI 2.1.282: hitting the turn limit reports it three
// ways, and carries no `result` field at all.
func TestClassifyRealMaxTurnsResult(t *testing.T) {
	const line = `{"type":"result","subtype":"error_max_turns","session_id":"f6",` +
		`"is_error":true,"num_turns":2,"stop_reason":"end_turn","terminal_reason":"max_turns",` +
		`"errors":["Reached maximum number of turns (1)"],"permission_denials":[]}`
	c := collect(t, line)
	got := Classify(Run{Collector: c, ExitErr: errors.New("exit status 1")})
	if got.Result.ErrorKind != store.ErrKindMaxTurns {
		t.Errorf("ErrorKind = %q, want max_turns", got.Result.ErrorKind)
	}
	// The CLI's own wording is more useful than ours.
	if !strings.Contains(got.Result.ErrorMessage, "maximum number of turns") {
		t.Errorf("ErrorMessage = %q, should quote errors[]", got.Result.ErrorMessage)
	}
}

// Captured verbatim: structured_output is the real field name, and the same
// JSON also appears as text in `result`.
func TestClassifyRealStructuredOutput(t *testing.T) {
	const line = `{"type":"result","subtype":"success","session_id":"f6","is_error":false,` +
		`"num_turns":4,"total_cost_usd":0.0671561,"duration_ms":8502,"terminal_reason":"completed",` +
		`"permission_denials":[],` +
		`"result":"{\"status\":\"succeeded\",\"summary\":\"Spike test completed successfully\"}",` +
		`"structured_output":{"status":"succeeded","summary":"Spike test completed successfully"}}`
	c := collect(t, line)
	got := Classify(Run{Collector: c}).Result
	if got.Status != store.StatusSucceeded {
		t.Fatalf("Status = %v (%s)", got.Status, got.ErrorMessage)
	}
	if got.Summary != "Spike test completed successfully" {
		t.Errorf("Summary = %q", got.Summary)
	}
	if got.NumTurns != 4 || got.CostUSD == 0 {
		t.Errorf("stats = %+v", got)
	}
}

// Captured verbatim: a denied tool reports {tool_name, tool_use_id, tool_input}.
func TestClassifyRealPermissionDenial(t *testing.T) {
	const line = `{"type":"result","subtype":"success","session_id":"f6","is_error":false,` +
		`"num_turns":3,"terminal_reason":"completed",` +
		`"permission_denials":[{"tool_name":"Write","tool_use_id":"toolu_014Z",` +
		`"tool_input":{"file_path":"/tmp/x","content":"hello"}}],` +
		`"structured_output":{"status":"failed","summary":"could not write the file"}}`
	c := collect(t, line)
	got := Classify(Run{Collector: c}).Result
	if len(got.PermissionDenials) != 1 || got.PermissionDenials[0] != "Write" {
		t.Errorf("PermissionDenials = %v, want [Write]", got.PermissionDenials)
	}
	// A denied tool does not by itself fail the run: Claude reported the task
	// outcome, and that is what decides.
	if got.Status != store.StatusFailed || got.ErrorKind != "" {
		t.Errorf("got %v/%q, want failed with no run-level error kind", got.Status, got.ErrorKind)
	}
}

func TestArgsIncludesToolRestrictions(t *testing.T) {
	args := strings.Join(Invocation{
		Prompt: "x", Tools: []string{"Skill", "WebSearch"},
		AllowedTools:    []string{"Skill", "mcp__claude_ai_Notion__notion-search"},
		DisallowedTools: []string{"Bash"}, MaxTurns: 30,
	}.Args(), " ")
	for _, want := range []string{
		"--tools Skill,WebSearch",
		"--allowedTools Skill,mcp__claude_ai_Notion__notion-search",
		"--disallowedTools Bash",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
}

func TestArgsIncludesBudgetCap(t *testing.T) {
	args := strings.Join(Invocation{Prompt: "x", MaxTurns: 30, MaxBudgetUSD: 0.25}.Args(), " ")
	if !strings.Contains(args, "--max-budget-usd 0.25") {
		t.Errorf("budget cap not passed: %s", args)
	}
	// Zero means unset, not "spend nothing".
	if args := strings.Join(Invocation{Prompt: "x"}.Args(), " "); strings.Contains(args, "--max-budget-usd") {
		t.Errorf("unexpected budget flag: %s", args)
	}
}
