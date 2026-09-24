package claudecli

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// bannedEnv are variables that must never reach the claude subprocess.
//
// Either one outranks the claude.ai login and would quietly turn off skills
// and connectors — the whole reason Spool keeps a real login (§3.1). They are
// stripped from the inherited environment rather than merely "not set", so an
// operator exporting one in the container cannot silently break every queue.
var bannedEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN",
}

// Invocation is one `claude -p` run, built from a job and its queue's config.
type Invocation struct {
	Binary string
	Prompt string

	// WorkDir is a fresh empty directory, so no stray .claude/ or .mcp.json
	// from elsewhere on the filesystem leaks into the run (§3.1).
	WorkDir string
	// ConfigDir becomes CLAUDE_CONFIG_DIR: the one login lives there.
	ConfigDir string

	SystemPromptFile string
	JSONSchemaFile   string
	AllowedTools     []string
	MaxTurns         int
	Model            string
	ResumeSession    string

	SyncSkills bool
	// ExtraEnv is appended last; it cannot reintroduce a banned variable.
	ExtraEnv []string
}

// Args builds the command line.
//
// SPIKE (§6): the exact spelling of these flags is unverified against a pinned
// CLI. --json-schema is passed a file path here; if the flag wants inline JSON
// instead, this function is the only thing that changes.
func (inv Invocation) Args() []string {
	args := []string{
		"-p", inv.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
	}
	if inv.JSONSchemaFile != "" {
		args = append(args, "--json-schema", inv.JSONSchemaFile)
	}
	if inv.SystemPromptFile != "" {
		args = append(args, "--append-system-prompt-file", inv.SystemPromptFile)
	}
	if len(inv.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(inv.AllowedTools, ","))
	}
	if inv.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(inv.MaxTurns))
	}
	if inv.Model != "" {
		args = append(args, "--model", inv.Model)
	}
	if inv.ResumeSession != "" {
		args = append(args, "--resume", inv.ResumeSession)
	}
	return args
}

// Env builds the subprocess environment from base (normally os.Environ()).
func (inv Invocation) Env(base []string) []string {
	out := make([]string, 0, len(base)+4)
	for _, kv := range base {
		if isBanned(kv) || hasKey(kv, "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_SYNC_SKILLS", "DISABLE_AUTOUPDATER") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "CLAUDE_CONFIG_DIR="+inv.ConfigDir)
	// Upgrades are deliberate: the classifier and the login parser both scrape
	// CLI output and are version-fragile (§3.2).
	out = append(out, "DISABLE_AUTOUPDATER=1")
	if inv.SyncSkills {
		out = append(out, "CLAUDE_CODE_SYNC_SKILLS=1")
	}
	for _, kv := range inv.ExtraEnv {
		if isBanned(kv) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isBanned(kv string) bool { return hasKey(kv, bannedEnv...) }

func hasKey(kv string, keys ...string) bool {
	name, _, ok := strings.Cut(kv, "=")
	if !ok {
		return false
	}
	for _, k := range keys {
		if name == k {
			return true
		}
	}
	return false
}

// Command builds the subprocess. The child gets its own process group so a
// timeout can signal the whole tree, not just the CLI's own pid.
func (inv Invocation) Command(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, inv.Binary, inv.Args()...)
	cmd.Dir = inv.WorkDir
	cmd.Env = inv.Env(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Nothing to type at: an unattended run must never block on stdin.
	cmd.Stdin = nil
	// Cancellation is handled explicitly by the executor so it can give the CLI
	// a grace period between SIGINT and SIGTERM.
	cmd.Cancel = func() error { return nil }
	return cmd
}
