package claudecli

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/krelinga/claude-spool-be/internal/config"
	"github.com/krelinga/claude-spool-be/internal/store"
)

// SPIKE (§6 item 6): these patterns are built from the documented and reported
// error strings, not yet from captured real failures. Widen them once the
// spike has recorded genuine `Login expired` and usage-limit results. They are
// the single place run-level failure text is interpreted.
var (
	authPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)login\s+expired`),
		regexp.MustCompile(`(?i)authentication_failed`),
		regexp.MustCompile(`(?i)\bnot\s+logged\s+in\b`),
		regexp.MustCompile(`(?i)please\s+run\s+/login`),
		regexp.MustCompile(`(?i)run\s+` + "`?" + `claude\s+auth\s+login`),
		regexp.MustCompile(`(?i)oauth\s+token\s+(has\s+)?expired`),
		regexp.MustCompile(`(?i)invalid\s+(api\s+key|bearer\s+token)`),
		regexp.MustCompile(`(?i)\b401\b.*unauthorized|unauthorized.*\b401\b`),
	}
	usagePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)usage\s+limit\s+reached`),
		regexp.MustCompile(`(?i)rate\s*limit`),
		regexp.MustCompile(`(?i)\b429\b`),
		regexp.MustCompile(`(?i)quota\s+exceeded`),
		regexp.MustCompile(`(?i)out\s+of\s+(credits|usage)`),
	}
	// The CLI reports a reset as a trailing epoch, e.g.
	// "Claude AI usage limit reached|1753500000".
	usageResetRe = regexp.MustCompile(`\|(\d{9,13})\b`)
	// A tool call refused by the allowlist, seen as an errored tool_result.
	denialPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)permission`),
		regexp.MustCompile(`(?i)not\s+allowed`),
		regexp.MustCompile(`(?i)\bdenied\b`),
	}
)

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// Collector accumulates the signals a run produces as its stream arrives.
type Collector struct {
	SessionID    string
	Init         *Event
	Result       *Event
	ToolCalls    int
	RateLimited  bool
	Denials      []string
	toolNameByID map[string]string
	seenDenial   map[string]bool
}

func NewCollector() *Collector {
	return &Collector{toolNameByID: map[string]string{}, seenDenial: map[string]bool{}}
}

// Observe folds one event into the collector.
func (c *Collector) Observe(e *Event) {
	if e.SessionID != "" {
		c.SessionID = e.SessionID
	}
	switch {
	case e.IsInit():
		c.Init = e
	case e.IsResult():
		c.Result = e
	}
	// An api_retry carrying a rate-limit note is the early warning that the
	// subscription budget, not this job, is the problem (§3.4).
	if strings.Contains(e.Type, "retry") {
		if s, ok := e.rawString("error", "message", "reason"); ok && matchAny(usagePatterns, s) {
			c.RateLimited = true
		}
	}
	for _, b := range e.Blocks() {
		switch b.Type {
		case "tool_use":
			c.ToolCalls++
			if b.ID != "" {
				c.toolNameByID[b.ID] = b.Name
			}
		case "tool_result":
			if !b.IsError {
				continue
			}
			text := b.TextContent()
			if matchAny(usagePatterns, text) {
				c.RateLimited = true
			}
			if matchAny(denialPatterns, text) {
				name := c.toolNameByID[b.ToolUseID]
				if name == "" {
					name = "unknown tool"
				}
				if !c.seenDenial[name] {
					c.seenDenial[name] = true
					c.Denials = append(c.Denials, name)
				}
			}
		}
	}
}

// MissingCapabilities reports which declared requirements are absent from the
// system/init event. A queue whose skill or connector is gone must fail loudly
// rather than let Claude improvise without its tools (§3.2).
//
// It returns nil when there is no init event to check against: that is a
// different failure, classified elsewhere.
func (c *Collector) MissingCapabilities(req config.Requires) []string {
	if c.Init == nil {
		return nil
	}
	var missing []string

	have := map[string]bool{}
	for _, cmd := range c.Init.SlashCommands {
		have[strings.ToLower(strings.TrimPrefix(cmd, "/"))] = true
	}
	// Some CLI versions report synced skills separately from slash commands.
	if raw, ok := c.Init.rawJSON("skills"); ok {
		var skills []string
		if err := json.Unmarshal(raw, &skills); err == nil {
			for _, s := range skills {
				have[strings.ToLower(strings.TrimPrefix(s, "/"))] = true
			}
		}
	}
	for _, want := range req.Skills {
		if !have[strings.ToLower(strings.TrimPrefix(want, "/"))] {
			missing = append(missing, "skill "+want)
		}
	}

	connected := map[string]bool{}
	for _, srv := range c.Init.MCPServers {
		if srv.Connected() {
			connected[strings.ToLower(srv.Name)] = true
		}
	}
	for _, want := range req.Connectors {
		if !connected[strings.ToLower(want)] {
			missing = append(missing, "connector "+want)
		}
	}
	return missing
}

// Run is everything the executor knows about a finished subprocess.
type Run struct {
	Collector *Collector
	// TimedOut is set when the executor killed the run on the queue's timeout.
	TimedOut bool
	// Missing is set when required capabilities were absent at init.
	Missing []string
	// ExitErr is the subprocess error, if any.
	ExitErr error
	// Stderr is the tail of the CLI's stderr, used only for classification and
	// error messages.
	Stderr string
}

// Classification is the run-level verdict plus the retry hint a blocking
// failure carries.
type Classification struct {
	Result store.Result
	// ResetAt is when a usage block may lift, when the CLI told us.
	ResetAt *time.Time
}

// Classify turns a finished run into a stored result.
//
// Layer 1 is run-level: did the CLI get far enough to have an opinion?
// Layer 2 is task-level: did Claude say the task itself succeeded? Both are
// needed — `is_error: false` alone does not mean the task worked (§3.4).
func Classify(r Run) Classification {
	c := r.Collector
	res := store.Result{
		SessionID:         c.SessionID,
		ToolCalls:         c.ToolCalls,
		PermissionDenials: c.Denials,
	}
	if c.Result != nil {
		res.NumTurns = c.Result.NumTurns
		res.CostUSD = c.Result.TotalCostUSD
		res.DurationMS = c.Result.DurationMS
	}

	fail := func(kind store.ErrorKind, msg string) Classification {
		res.Status = store.StatusFailed
		res.ErrorKind = kind
		res.ErrorMessage = msg
		return Classification{Result: res}
	}

	// Ordered most-specific first: a capability problem explains everything
	// downstream of it, and a timeout explains a missing result.
	if len(r.Missing) > 0 {
		return fail(store.ErrKindCapabilityMissing,
			"required capabilities unavailable: "+strings.Join(r.Missing, ", "))
	}
	if r.TimedOut {
		return fail(store.ErrKindTimeout, "job exceeded its queue timeout")
	}

	text := classificationText(r)
	if matchAny(authPatterns, text) {
		return fail(store.ErrKindAuth, firstLine(text))
	}
	if c.RateLimited || matchAny(usagePatterns, text) {
		out := fail(store.ErrKindUsageLimit, firstLine(text))
		out.ResetAt = parseUsageReset(text)
		return out
	}
	if c.Result != nil && c.Result.Subtype == "error_max_turns" {
		return fail(store.ErrKindMaxTurns, "hit the queue's max_turns limit")
	}
	if r.ExitErr != nil {
		return fail(store.ErrKindCLI, cliErrorMessage(r))
	}
	if c.Result == nil {
		return fail(store.ErrKindCLI, "claude exited without a result event")
	}
	if c.Result.IsError {
		return fail(store.ErrKindCLI, firstLine(nonEmpty(c.Result.Result, r.Stderr, "claude reported an error")))
	}

	// Layer 2: the run was clean, so ask what Claude said about the task.
	raw, outcome, err := ExtractOutcome(c.Result)
	if err != nil {
		return fail(store.ErrKindInvalidOutcome, err.Error())
	}
	res.Outcome = raw
	res.Summary = outcome.Summary
	switch outcome.Status {
	case OutcomeSucceeded:
		res.Status = store.StatusSucceeded
	case OutcomeNeedsInput:
		res.Status = store.StatusNeedsInput
		if outcome.Question != "" && res.Summary == "" {
			res.Summary = outcome.Question
		}
	default:
		// A task-level failure is not a run-level error: the machinery worked,
		// the task did not. It carries no error_kind.
		res.Status = store.StatusFailed
	}
	return Classification{Result: res}
}

// classificationText is everything worth pattern-matching against.
func classificationText(r Run) string {
	var sb strings.Builder
	if r.Collector.Result != nil {
		sb.WriteString(r.Collector.Result.Result)
		sb.WriteString("\n")
		if s, ok := r.Collector.Result.rawString("error", "message"); ok {
			sb.WriteString(s)
			sb.WriteString("\n")
		}
	}
	sb.WriteString(r.Stderr)
	return sb.String()
}

func cliErrorMessage(r Run) string {
	if s := firstLine(r.Stderr); s != "" {
		return s
	}
	if r.Collector.Result != nil && r.Collector.Result.Result != "" {
		return firstLine(r.Collector.Result.Result)
	}
	return r.ExitErr.Error()
}

func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

func parseUsageReset(text string) *time.Time {
	m := usageResetRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return nil
	}
	var t time.Time
	if n > 1e12 {
		t = time.UnixMilli(n)
	} else {
		t = time.Unix(n, 0)
	}
	t = t.UTC()
	// Guard against a nonsense epoch pinning the executor open for years.
	if t.Before(time.Now()) || t.After(time.Now().Add(48*time.Hour)) {
		return nil
	}
	return &t
}

// fencedJSONRe finds a ```json fenced block, the documented fallback if
// structured output does not appear on the result line (§3.4).
var fencedJSONRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// ExtractOutcome pulls the task-level outcome off a result event.
//
// SPIKE (§6 item 3): the field name is unconfirmed, so several candidates are
// tried before falling back to a fenced JSON block in the result text.
func ExtractOutcome(result *Event) (json.RawMessage, Outcome, error) {
	var o Outcome
	raw, ok := result.rawJSON("structured_output", "structuredOutput", "structured_result", "structuredResult")
	if !ok {
		if m := fencedJSONRe.FindStringSubmatch(result.Result); m != nil {
			raw = json.RawMessage(m[1])
		} else if trimmed := strings.TrimSpace(result.Result); strings.HasPrefix(trimmed, "{") &&
			json.Valid([]byte(trimmed)) {
			raw = json.RawMessage(trimmed)
		} else {
			return nil, o, errNoOutcome
		}
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, o, errBadOutcome
	}
	if !o.Valid() {
		return nil, o, errBadOutcomeStatus
	}
	return raw, o, nil
}

type outcomeError string

func (e outcomeError) Error() string { return string(e) }

const (
	errNoOutcome        = outcomeError("claude returned no structured outcome")
	errBadOutcome       = outcomeError("claude's structured outcome was not valid JSON")
	errBadOutcomeStatus = outcomeError("claude's structured outcome had no valid status")
)
