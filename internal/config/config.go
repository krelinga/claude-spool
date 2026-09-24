package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
}

type ClaudeConfig struct {
	// Binary is the claude executable; pinned by the image, not by config.
	Binary string `yaml:"binary"`
	// ConfigDir becomes CLAUDE_CONFIG_DIR: the one login lives here.
	ConfigDir      string         `yaml:"config_dir"`
	CredentialMode CredentialMode `yaml:"credential_mode"`
	DefaultModel   string         `yaml:"default_model"`
	// SyncSkills sets CLAUDE_CODE_SYNC_SKILLS=1. Meaningless under
	// long_lived_token, where skills are vendored instead.
	SyncSkills bool `yaml:"sync_skills"`
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
	}
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
	return nil
}

func (c *Config) validate() error {
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
