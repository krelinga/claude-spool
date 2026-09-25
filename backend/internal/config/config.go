package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CredentialMode selects how the executor authenticates to Claude (§3.2).
// Only ModeLogin gives synced claude.ai skills and connectors; the fallback
// exists so switching is a config change rather than a rewrite.
type CredentialMode string

const (
	ModeLogin          CredentialMode = "claudeai_login"
	ModeLongLivedToken CredentialMode = "long_lived_token"
)

type Config struct {
	Listen  string        `yaml:"listen"`
	DataDir string        `yaml:"data_dir"`
	Claude  ClaudeConfig  `yaml:"claude"`
	Tokens  []TokenConfig `yaml:"tokens"`

	Webhooks []WebhookConfig `yaml:"webhooks"`
	// WebhookRetryWindow is how long a delivery keeps being retried before it
	// is marked dead (§3.7 says 24h).
	WebhookRetryWindow Duration `yaml:"webhook_retry_window"`
	// PublicURL is the externally reachable base URL, used to build the
	// re-login link in an auth.required notification so it is actionable from
	// a phone.
	PublicURL string `yaml:"public_url"`
}

// WebhookConfig is one receiver. Receivers are global; which job events reach
// them is filtered by each queue's notify list, while service events always go
// to all of them (§3.7).
type WebhookConfig struct {
	Name      string `yaml:"name"`
	URL       string `yaml:"url"`
	Secret    string `yaml:"secret"`
	SecretEnv string `yaml:"secret_env"`
	// Events optionally narrows this receiver further. Empty means "whatever
	// the queues subscribe it to, plus all service events".
	Events []string `yaml:"events"`

	secret string
}

// HMACSecret returns the resolved signing secret.
func (w *WebhookConfig) HMACSecret() string { return w.secret }

// Wants reports whether this receiver accepts an event type, per its own
// Events filter. Per-queue subscription is applied separately.
func (w *WebhookConfig) Wants(event string) bool {
	if len(w.Events) == 0 {
		return true
	}
	for _, e := range w.Events {
		if e == event {
			return true
		}
	}
	return false
}

type ClaudeConfig struct {
	// Binary is the claude executable; pinned by the image, not by config.
	Binary string `yaml:"binary"`
	// ConfigDir becomes CLAUDE_CONFIG_DIR: the one login lives here.
	ConfigDir      string         `yaml:"config_dir"`
	CredentialMode CredentialMode `yaml:"credential_mode"`
	DefaultModel   string         `yaml:"default_model"`
	// DefaultMaxBudgetUSD caps what one job may spend when its queue sets no
	// max_budget_usd of its own. SPOOL_MAX_BUDGET_USD overrides it, so the cap
	// can be tuned per deployment without editing a file.
	DefaultMaxBudgetUSD float64 `yaml:"default_max_budget_usd"`
	// SyncSkills sets CLAUDE_CODE_SYNC_SKILLS=1. Meaningless under
	// long_lived_token, where skills are vendored instead.
	SyncSkills bool `yaml:"sync_skills"`
	// KeepaliveInterval is how often to make a minimal real request when no job
	// has run, keeping the access token refreshing well inside its window so an
	// idle weekend does not let the session go stale (§3.2). Zero disables it.
	KeepaliveInterval Duration `yaml:"keepalive_interval"`
	// LoginAttemptTTL bounds how long an unfinished re-login may hold the
	// claude lock waiting for its code.
	LoginAttemptTTL Duration `yaml:"login_attempt_ttl"`
}

// TokenConfig is a named bearer token, optionally scoped to a set of queues.
// Scoping matters because `adhoc` is effectively "run anything the allowlist
// permits", and a share-sheet token should not reach it (§3.3).
type TokenConfig struct {
	Name string `yaml:"name"`
	// Token is the literal secret. TokenEnv names an environment variable to
	// read it from instead, so a git-tracked config.yaml need not hold secrets.
	Token    string   `yaml:"token"`
	TokenEnv string   `yaml:"token_env"`
	Queues   []string `yaml:"queues"`

	secret string
}

// Paths derived from DataDir. The layout is fixed by the design (§3.8).
func (c *Config) DBPath() string        { return filepath.Join(c.DataDir, "spool.db") }
func (c *Config) TranscriptDir() string { return filepath.Join(c.DataDir, "jobs") }
func (c *Config) WorkDir() string       { return filepath.Join(c.DataDir, "work") }
func (c *Config) RunDir() string        { return filepath.Join(c.DataDir, "run") }

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.applyEnv(); err != nil {
		return nil, err
	}
	if err := c.resolveTokens(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "/data"
	}
	if c.Claude.Binary == "" {
		c.Claude.Binary = "claude"
	}
	if c.Claude.ConfigDir == "" {
		c.Claude.ConfigDir = filepath.Join(c.DataDir, "claude")
	}
	if c.Claude.CredentialMode == "" {
		c.Claude.CredentialMode = ModeLogin
	}
	if c.Claude.CredentialMode == ModeLogin {
		c.Claude.SyncSkills = true
		if c.Claude.KeepaliveInterval == 0 {
			c.Claude.KeepaliveInterval = Duration(4 * time.Hour)
		}
	}
	if c.Claude.DefaultMaxBudgetUSD == 0 {
		// Measured, not guessed: a cold-cache job on a Notion queue costs
		// $0.35-0.50, almost all of it context, so a lower cap fails normal jobs.
		c.Claude.DefaultMaxBudgetUSD = 1.00
	}
	if c.Claude.LoginAttemptTTL == 0 {
		c.Claude.LoginAttemptTTL = Duration(10 * time.Minute)
	}
	if c.WebhookRetryWindow == 0 {
		c.WebhookRetryWindow = Duration(24 * time.Hour)
	}
}

// MaxBudgetEnv overrides claude.default_max_budget_usd when set.
const MaxBudgetEnv = "SPOOL_MAX_BUDGET_USD"

// applyEnv applies environment overrides, which win over the file.
func (c *Config) applyEnv() error {
	if v := os.Getenv(MaxBudgetEnv); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("%s=%q: not a number", MaxBudgetEnv, v)
		}
		c.Claude.DefaultMaxBudgetUSD = f
	}
	return nil
}

func (c *Config) resolveTokens() error {
	for i := range c.Tokens {
		t := &c.Tokens[i]
		switch {
		case t.Token != "" && t.TokenEnv != "":
			return fmt.Errorf("token %q: set token or token_env, not both", t.Name)
		case t.TokenEnv != "":
			v := os.Getenv(t.TokenEnv)
			if v == "" {
				return fmt.Errorf("token %q: env %s is empty", t.Name, t.TokenEnv)
			}
			t.secret = v
		default:
			t.secret = t.Token
		}
	}
	for i := range c.Webhooks {
		w := &c.Webhooks[i]
		switch {
		case w.Secret != "" && w.SecretEnv != "":
			return fmt.Errorf("webhook %q: set secret or secret_env, not both", w.Name)
		case w.SecretEnv != "":
			v := os.Getenv(w.SecretEnv)
			if v == "" {
				return fmt.Errorf("webhook %q: env %s is empty", w.Name, w.SecretEnv)
			}
			w.secret = v
		default:
			w.secret = w.Secret
		}
	}
	return nil
}

func (c *Config) validate() error {
	if c.Claude.DefaultMaxBudgetUSD <= 0 {
		return fmt.Errorf("default max budget must be positive, got %v", c.Claude.DefaultMaxBudgetUSD)
	}
	switch c.Claude.CredentialMode {
	case ModeLogin, ModeLongLivedToken:
	default:
		return fmt.Errorf("unknown credential_mode %q", c.Claude.CredentialMode)
	}
	if len(c.Tokens) == 0 {
		return fmt.Errorf("at least one API token is required")
	}
	seen := map[string]bool{}
	for _, t := range c.Tokens {
		if t.Name == "" {
			return fmt.Errorf("every token needs a name")
		}
		if seen[t.Name] {
			return fmt.Errorf("duplicate token name %q", t.Name)
		}
		seen[t.Name] = true
		if len(t.secret) < 16 {
			return fmt.Errorf("token %q: secret must be at least 16 characters", t.Name)
		}
		if len(t.Queues) == 0 {
			return fmt.Errorf("token %q: needs queues (use [\"*\"] for all)", t.Name)
		}
	}
	seenHook := map[string]bool{}
	for _, w := range c.Webhooks {
		if w.Name == "" {
			return fmt.Errorf("every webhook needs a name")
		}
		if seenHook[w.Name] {
			return fmt.Errorf("duplicate webhook name %q", w.Name)
		}
		seenHook[w.Name] = true
		u, err := url.Parse(w.URL)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("webhook %q: url must be an absolute http(s) URL", w.Name)
		}
		// Every event is signed, so a receiver without a secret cannot verify
		// anything and would accept a forged notification.
		if len(w.secret) < 16 {
			return fmt.Errorf("webhook %q: secret must be at least 16 characters", w.Name)
		}
	}
	if c.WebhookRetryWindow <= 0 {
		return fmt.Errorf("webhook_retry_window must be positive")
	}
	// A keep-alive slower than the ~8h access-token window defeats its own
	// purpose; the design calls for 4h.
	if iv := c.Claude.KeepaliveInterval.Duration(); iv != 0 && iv > 8*time.Hour {
		return fmt.Errorf("keepalive_interval %s is longer than the access token window; use 4h or less", iv)
	}
	if c.Claude.LoginAttemptTTL <= 0 {
		return fmt.Errorf("login_attempt_ttl must be positive")
	}
	return nil
}

// AllowsQueue reports whether this token may submit to a queue.
func (t *TokenConfig) AllowsQueue(queue string) bool {
	for _, q := range t.Queues {
		if q == "*" || q == queue {
			return true
		}
	}
	return false
}

// Lookup finds the token for a presented bearer secret. The comparison is
// constant-time and covers every configured token so that a miss costs the
// same as a hit.
func (c *Config) Lookup(presented string) (*TokenConfig, bool) {
	want := sha256.Sum256([]byte(presented))
	var found *TokenConfig
	for i := range c.Tokens {
		got := sha256.Sum256([]byte(c.Tokens[i].secret))
		if subtle.ConstantTimeCompare(want[:], got[:]) == 1 {
			found = &c.Tokens[i]
		}
	}
	return found, found != nil
}
