package claudecli

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/claude-spool-be/internal/config"
	"github.com/krelinga/claude-spool-be/internal/store"
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
		MaxTurns: 30, Model: "sonnet", JSONSchemaFile: "/run/schema.json",
		SystemPromptFile: "/run/system.md",
	}
	args := inv.Args()
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-p do a thing", "--output-format stream-json", "--verbose",
		"--permission-mode dontAsk", "--permission-prompts none",
		"--allowedTools Skill,WebSearch", "--max-turns 30", "--model sonnet",
		"--json-schema /run/schema.json", "--append-system-prompt-file /run/system.md",
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

func TestMissingCapabilities(t *testing.T) {
	c := collect(t, initLine, resultLine(t, okOutcome("succeeded", "ok")))

	if got := c.MissingCapabilities(config.Requires{
		Skills: []string{"notion-media"}, Connectors: []string{"notion"},
	}); got != nil {
		t.Errorf("satisfied requirements reported missing: %v", got)
	}
	// A leading slash in config must not matter.
	if got := c.MissingCapabilities(config.Requires{Skills: []string{"/notion-media"}}); got != nil {
		t.Errorf("slash-prefixed skill not matched: %v", got)
	}
	got := c.MissingCapabilities(config.Requires{
		Skills: []string{"notion-ideas"}, Connectors: []string{"todoist"},
	})
	if len(got) != 2 {
		t.Fatalf("MissingCapabilities = %v, want 2", got)
	}
}

// A connector that is present but not connected is missing for our purposes.
func TestDisconnectedConnectorCountsAsMissing(t *testing.T) {
	c := collect(t, `{"type":"system","subtype":"init","session_id":"s1",`+
		`"mcp_servers":[{"name":"notion","status":"failed"}]}`)
	if got := c.MissingCapabilities(config.Requires{Connectors: []string{"notion"}}); len(got) != 1 {
		t.Errorf("MissingCapabilities = %v, want the failed connector", got)
	}
}

// With no init event there is nothing to check against; that is a different
// failure and must not be reported as a capability problem.
func TestMissingCapabilitiesWithoutInit(t *testing.T) {
	c := NewCollector()
	if got := c.MissingCapabilities(config.Requires{Skills: []string{"x"}}); got != nil {
		t.Errorf("MissingCapabilities = %v, want nil", got)
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
				return Run{Collector: c, Missing: []string{"connector notion"}}
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
