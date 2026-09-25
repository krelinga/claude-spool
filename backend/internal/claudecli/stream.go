// Package claudecli owns everything that touches the Claude Code CLI's
// command line and output format.
//
// Every assumption about CLI surface lives in this package on purpose. The
// flags and the stream-json shape are version-specific and several are still
// unverified (design §6); when the spike answers them, this is the only place
// that changes. See docs/design/spike.md for the open questions.
package claudecli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Event is one line of `--output-format stream-json`.
//
// Raw keeps the undecoded line so we can probe for fields whose names are not
// yet confirmed without having to guess them all in the struct.
type Event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	SessionID string `json:"session_id"`

	// system/init
	Tools         []string    `json:"tools"`
	SlashCommands []string    `json:"slash_commands"`
	MCPServers    []MCPServer `json:"mcp_servers"`
	Model         string      `json:"model"`

	// result
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
	// Errors carries the CLI's own failure text, e.g.
	// ["Reached maximum number of turns (1)"]. Present on 2.1.282 and often
	// the only place the reason is spelled out, since `result` can be absent
	// on a failed run.
	Errors []string `json:"errors"`
	// TerminalReason is a machine-readable outcome: "completed", "max_turns",
	// "api_error". More trustworthy than Subtype, which reads "success" even on
	// an authentication failure.
	TerminalReason string `json:"terminal_reason"`

	// assistant/user
	Message json.RawMessage `json:"message"`

	Raw map[string]json.RawMessage `json:"-"`
}

type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Source distinguishes a claude.ai connector ("claudeai") from a locally
	// configured server. Observed on CLI 2.1.282.
	Source string `json:"source,omitempty"`
}

// Connected reports whether an MCP server is usable.
//
// Verified statuses on CLI 2.1.282: "connected", "pending", "needs-auth",
// "failed". Only a connected server contributes tools — a pending one reports
// none at all — so anything else is unusable.
func (m MCPServer) Connected() bool {
	switch strings.ToLower(m.Status) {
	case "connected", "ok", "ready":
		return true
	}
	return false
}

// Pending reports whether a server was still connecting. This is a race rather
// than a misconfiguration: the same server reads "connected" on the next run.
func (m MCPServer) Pending() bool {
	switch strings.ToLower(m.Status) {
	case "pending", "connecting":
		return true
	}
	return false
}

// nonToolNameChar matches everything the CLI replaces with "_" when it derives
// a tool-name prefix from a server name.
var nonToolNameChar = regexp.MustCompile(`[^A-Za-z0-9_]`)

// ToolPrefix is the prefix this server's tools carry. The server named
// "claude.ai Notion" contributes "mcp__claude_ai_Notion__notion-search" and so
// on, so the prefix is the sanitised name between double underscores.
func (m MCPServer) ToolPrefix() string {
	return "mcp__" + nonToolNameChar.ReplaceAllString(m.Name, "_") + "__"
}

func (e *Event) IsResult() bool { return e.Type == "result" }
func (e *Event) IsInit() bool   { return e.Type == "system" && e.Subtype == "init" }

// ContentBlock is one block of an assistant or user message.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// TextContent flattens a tool_result's content, which may be a bare string or
// an array of blocks depending on the tool.
func (b ContentBlock) TextContent() string {
	if b.Text != "" {
		return b.Text
	}
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(b.Content, &blocks); err == nil {
		var sb strings.Builder
		for _, sub := range blocks {
			sb.WriteString(sub.Text)
		}
		return sb.String()
	}
	return ""
}

type apiMessage struct {
	Role       string          `json:"role"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
}

// Blocks decodes the content blocks of an assistant or user message. A message
// whose content is a plain string yields a single text block.
func (e *Event) Blocks() []ContentBlock {
	if len(e.Message) == 0 {
		return nil
	}
	var m apiMessage
	if err := json.Unmarshal(e.Message, &m); err != nil {
		return nil
	}
	if len(m.Content) == 0 {
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		return blocks
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return []ContentBlock{{Type: "text", Text: s}}
	}
	return nil
}

// ParseEvent decodes one stream-json line.
func ParseEvent(line []byte) (*Event, error) {
	var e Event
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, fmt.Errorf("decode stream event: %w", err)
	}
	if err := json.Unmarshal(line, &e.Raw); err != nil {
		return nil, fmt.Errorf("decode stream event: %w", err)
	}
	return &e, nil
}

// rawString reads a string field from the undecoded line, for fields whose
// exact name varies across CLI versions.
func (e *Event) rawString(keys ...string) (string, bool) {
	for _, k := range keys {
		v, ok := e.Raw[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			return s, true
		}
	}
	return "", false
}

// rawJSON reads an object field from the undecoded line under any of the given
// names, ignoring nulls.
func (e *Event) rawJSON(keys ...string) (json.RawMessage, bool) {
	for _, k := range keys {
		v, ok := e.Raw[k]
		if !ok || len(v) == 0 || string(v) == "null" {
			continue
		}
		return v, true
	}
	return nil, false
}

// ScanEvents reads a stream-json stream, calling fn for each decoded event and
// writing every line verbatim to transcript.
//
// Undecodable lines are passed to the transcript and skipped: the CLI may emit
// diagnostics we do not model, and one odd line should not fail a job.
func ScanEvents(r io.Reader, transcript io.Writer, fn func(*Event) error) error {
	sc := bufio.NewScanner(r)
	// Assistant turns carrying large tool results can be far larger than the
	// 64KB default.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if transcript != nil {
			if _, err := transcript.Write(append(append([]byte{}, line...), '\n')); err != nil {
				return fmt.Errorf("write transcript: %w", err)
			}
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		e, err := ParseEvent(line)
		if err != nil {
			continue
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return sc.Err()
}
