package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "10m", want: 10 * time.Minute},
		{in: "180d", want: 180 * 24 * time.Hour},
		{in: "1.5d", want: 36 * time.Hour},
		{in: "30s", want: 30 * time.Second},
		{in: "30", bad: true},
		{in: "", bad: true},
		{in: "1d12h", bad: true},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseDuration(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", c.in, err)
			continue
		}
		if got.Duration() != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got.Duration(), c.want)
		}
	}
}

const goodQueues = `
queues:
  media:
    description: Add entries to Notion Media
    prompt: |
      /notion-media {{input}}
    args:
      url: { type: string, format: uri }
    allowed_tools: [Skill, WebSearch]
    model: sonnet
    timeout: 10m
    weight: 2
    retention: 180d
  adhoc:
    prompt: "{{input}}"
    allowed_tools: [Skill]
`

func TestParseQueues(t *testing.T) {
	qs, err := ParseQueues([]byte(goodQueues))
	if err != nil {
		t.Fatalf("ParseQueues: %v", err)
	}
	media, ok := qs.Get("media")
	if !ok {
		t.Fatal("media queue missing")
	}
	if media.Name != "media" {
		t.Errorf("Name = %q", media.Name)
	}
	if media.Weight != 2 {
		t.Errorf("Weight = %v, want 2", media.Weight)
	}
	if media.Timeout.Duration() != 10*time.Minute {
		t.Errorf("Timeout = %v", media.Timeout)
	}
	adhoc, _ := qs.Get("adhoc")
	if adhoc.MaxTurns != DefaultMaxTurns || adhoc.Weight != DefaultWeight {
		t.Errorf("defaults not applied: %+v", adhoc)
	}
	if media.ConfigHash == "" || media.ConfigHash == adhoc.ConfigHash {
		t.Errorf("config hashes not distinct: %q %q", media.ConfigHash, adhoc.ConfigHash)
	}
	if got := qs.Names(); len(got) != 2 || got[0] != "adhoc" {
		t.Errorf("Names() = %v, want sorted", got)
	}
}

// A template edit must change the hash, so job history stays accurate.
func TestQueueHashChangesWithPrompt(t *testing.T) {
	a, _ := ParseQueues([]byte(goodQueues))
	b, _ := ParseQueues([]byte(strings.Replace(goodQueues, "/notion-media {{input}}", "/notion-media v2 {{input}}", 1)))
	qa, _ := a.Get("media")
	qb, _ := b.Get("media")
	if qa.ConfigHash == qb.ConfigHash {
		t.Error("hash unchanged after prompt edit")
	}
}

// Same config parsed twice must hash identically, or every reload would look
// like a config change.
func TestQueueHashStable(t *testing.T) {
	a, _ := ParseQueues([]byte(goodQueues))
	b, _ := ParseQueues([]byte(goodQueues))
	qa, _ := a.Get("media")
	qb, _ := b.Get("media")
	if qa.ConfigHash != qb.ConfigHash {
		t.Errorf("hash unstable: %q != %q", qa.ConfigHash, qb.ConfigHash)
	}
}

func TestParseQueuesRejects(t *testing.T) {
	cases := map[string]string{
		"no prompt":      "queues:\n  a:\n    description: x\n",
		"undeclared arg": "queues:\n  a:\n    prompt: \"{{args.nope}}\"\n",
		"unknown ref":    "queues:\n  a:\n    prompt: \"{{bogus}}\"\n",
		"bad name":       "queues:\n  Bad-Name:\n    prompt: x\n",
		"bad weight":     "queues:\n  a:\n    prompt: x\n    weight: -1\n",
		"unknown field":  "queues:\n  a:\n    prompt: x\n    nonsense: 1\n",
		"bad notify":     "queues:\n  a:\n    prompt: x\n    notify: [job.exploded]\n",
		"bad arg type":   "queues:\n  a:\n    prompt: x\n    args:\n      u: { type: date }\n",
		"empty":          "queues: {}\n",
	}
	for name, src := range cases {
		if _, err := ParseQueues([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRender(t *testing.T) {
	qs, err := ParseQueues([]byte(goodQueues))
	if err != nil {
		t.Fatal(err)
	}
	q, _ := qs.Get("media")
	got := q.Render(q.Prompt, "Dune", map[string]any{"url": "https://example.com"})
	if want := "/notion-media Dune\n"; got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
	// A declared-but-absent arg renders empty rather than leaving a literal
	// placeholder in the prompt.
	got = q.Render("{{args.url}}|{{input}}", "x", nil)
	if got != "|x" {
		t.Errorf("Render with missing arg = %q", got)
	}
}

func TestValidateArgs(t *testing.T) {
	qs, _ := ParseQueues([]byte(goodQueues))
	q, _ := qs.Get("media")
	if err := q.ValidateArgs(map[string]any{"url": "https://example.com"}); err != nil {
		t.Errorf("valid args rejected: %v", err)
	}
	if err := q.ValidateArgs(nil); err != nil {
		t.Errorf("optional arg required: %v", err)
	}
	for name, args := range map[string]map[string]any{
		"relative uri": {"url": "/not-absolute"},
		"wrong type":   {"url": 42.0},
		"unknown arg":  {"nope": "x"},
	} {
		if err := q.ValidateArgs(args); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

const goodConfig = `
listen: ":8080"
data_dir: /data
claude:
  config_dir: /data/claude
tokens:
  - name: admin
    token: "0123456789abcdef0123"
    queues: ["*"]
  - name: ios-share
    token: "fedcba98765432100000"
    queues: [media]
`

func TestParseConfig(t *testing.T) {
	c, err := Parse([]byte(goodConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Claude.CredentialMode != ModeLogin {
		t.Errorf("CredentialMode = %q, want default login", c.Claude.CredentialMode)
	}
	if !c.Claude.SyncSkills {
		t.Error("SyncSkills should default on under claudeai_login")
	}
	if c.DBPath() != "/data/spool.db" {
		t.Errorf("DBPath = %q", c.DBPath())
	}

	tok, ok := c.Lookup("fedcba98765432100000")
	if !ok || tok.Name != "ios-share" {
		t.Fatalf("Lookup = %v, %v", tok, ok)
	}
	if !tok.AllowsQueue("media") {
		t.Error("ios-share should reach media")
	}
	if tok.AllowsQueue("adhoc") {
		t.Error("scoped token must not reach adhoc")
	}
	admin, _ := c.Lookup("0123456789abcdef0123")
	if !admin.AllowsQueue("adhoc") {
		t.Error("wildcard token should reach every queue")
	}
	if _, ok := c.Lookup("wrong"); ok {
		t.Error("bad secret accepted")
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string]string{
		"no tokens":   "listen: \":80\"\n",
		"short token": "tokens:\n  - name: a\n    token: short\n    queues: [\"*\"]\n",
		"no queues":   "tokens:\n  - name: a\n    token: \"0123456789abcdef0123\"\n",
		"bad mode":    "claude:\n  credential_mode: magic\ntokens:\n  - name: a\n    token: \"0123456789abcdef0123\"\n    queues: [\"*\"]\n",
		"dup name":    "tokens:\n  - name: a\n    token: \"0123456789abcdef0123\"\n    queues: [\"*\"]\n  - name: a\n    token: \"0123456789abcdef0124\"\n    queues: [\"*\"]\n",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestTokenEnv(t *testing.T) {
	t.Setenv("SPOOL_TEST_TOKEN", "env-secret-0123456789")
	c, err := Parse([]byte("tokens:\n  - name: a\n    token_env: SPOOL_TEST_TOKEN\n    queues: [\"*\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Lookup("env-secret-0123456789"); !ok {
		t.Error("token_env secret not honoured")
	}
}

func TestMaxBudgetDefaultAndEnv(t *testing.T) {
	c, err := Parse([]byte(goodConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Claude.DefaultMaxBudgetUSD != 1.00 {
		t.Errorf("default = %v, want 1.00", c.Claude.DefaultMaxBudgetUSD)
	}

	fromFile := goodConfig + "claude:\n  default_max_budget_usd: 0.75\n"
	fromFile = strings.Replace(fromFile, "claude:\n  config_dir: /data/claude\n", "", 1)
	c, err = Parse([]byte(fromFile))
	if err != nil {
		t.Fatal(err)
	}
	if c.Claude.DefaultMaxBudgetUSD != 0.75 {
		t.Errorf("from file = %v, want 0.75", c.Claude.DefaultMaxBudgetUSD)
	}

	// The environment wins over the file.
	t.Setenv(MaxBudgetEnv, "2.5")
	c, err = Parse([]byte(fromFile))
	if err != nil {
		t.Fatal(err)
	}
	if c.Claude.DefaultMaxBudgetUSD != 2.5 {
		t.Errorf("from env = %v, want 2.5", c.Claude.DefaultMaxBudgetUSD)
	}

	for _, bad := range []string{"lots", "-1", "0"} {
		t.Setenv(MaxBudgetEnv, bad)
		if _, err := Parse([]byte(goodConfig)); err == nil {
			t.Errorf("%s=%q: expected error", MaxBudgetEnv, bad)
		}
	}
}
