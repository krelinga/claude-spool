package claudecli

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/krelinga/claude-spool/backend/internal/config"
	"github.com/krelinga/claude-spool/backend/internal/store"
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

// LooksLikeAuthFailure reports whether output indicates an expired or absent
// login. Exported so the auth manager classifies a keep-alive the same way the
// job classifier does — one set of patterns, one place to widen them.
func LooksLikeAuthFailure(s string) bool { return matchAny(authPatterns, s) }

// LooksLikeUsageLimit reports whether output indicates a usage or rate limit.
func LooksLikeUsageLimit(s string) bool { return matchAny(usagePatterns, s) }

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
	// The result line carries the CLI's own denial list, which beats inferring
	// from tool_result text. Verified present (as []) on CLI 2.1.282; the shape
	// when non-empty is still unconfirmed, so several spellings are tried and
	// the text-based inference below remains as a fallback.
	if e.IsResult() {
		for _, name := range parseDenials(e) {
			if !c.seenDenial[name] {
				c.seenDenial[name] = true
				c.Denials = append(c.Denials, name)
			}
		}
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

// Capabilities is the result of checking a queue's declared requirements
// against what the CLI actually reported at init.
type Capabilities struct {
	// Missing are requirements that are genuinely absent: a skill that is not
	// synced, a connector that is not configured or needs authorising. These
	// are config problems and auto-pause the queue.
	Missing []string
	// Pending are connectors that had not finished connecting yet. Observed in
	// the spike: a server reads "pending" at init and contributes no tools, and
	// reads "connected" on the next run. Transient, so worth retrying rather
	// than pausing the queue.
	Pending []string
}

func (c Capabilities) OK() bool { return len(c.Missing) == 0 && len(c.Pending) == 0 }

// capabilityMatches compares a requirement against a name the CLI reported,
// tolerating the prefixes the CLI adds.
//
// Verified on CLI 2.1.282: connectors arrive as "claude.ai Notion" and skills
// as "anthropic-skills:notion-media", so a queue declaring "notion" or
// "notion-media" must still match. Matching on the trailing segment keeps
// queues.yaml readable without pinning it to the CLI's namespacing.
func capabilityMatches(available, want string) bool {
	a := strings.ToLower(strings.TrimSpace(available))
	w := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(want, "/")))
	if a == w {
		return true
	}
	// Allow the requirement to be the tail of a prefixed name, after one of the
	// separators the CLI uses. Suffix matching rather than last-segment
	// matching, because connector names contain spaces: "google calendar" has
	// to match "claude.ai Google Calendar".
	for _, sep := range []string{":", " ", "/"} {
		if strings.HasSuffix(a, sep+w) {
			return true
		}
	}
	return false
}

// CheckCapabilities compares declared requirements against the init event.
//
// A queue whose skill or connector is absent must fail loudly rather than let
// Claude improvise without its tools (§3.2). It returns a zero value when there
// is no init event to check against: that is a different failure, classified
// elsewhere.
func (c *Collector) CheckCapabilities(req config.Requires) Capabilities {
	var out Capabilities
	if c.Init == nil {
		return out
	}

	// Skills appear in slash_commands, and separately in a skills array.
	var haveSkills []string
	haveSkills = append(haveSkills, c.Init.SlashCommands...)
	if raw, ok := c.Init.rawJSON("skills"); ok {
		var skills []string
		if err := json.Unmarshal(raw, &skills); err == nil {
			haveSkills = append(haveSkills, skills...)
		}
	}
	for _, want := range req.Skills {
		if !matchesAny(haveSkills, want) {
			out.Missing = append(out.Missing, "skill "+want)
		}
	}

	for _, want := range req.Connectors {
		srv, found := c.findServer(want)
		switch {
		case !found:
			out.Missing = append(out.Missing, "connector "+want+" (not configured)")
		case srv.Pending():
			out.Pending = append(out.Pending, "connector "+want+" (still connecting)")
		case !srv.Connected():
			out.Missing = append(out.Missing, "connector "+want+" ("+srv.Status+")")
		case !c.hasToolsFrom(srv):
			// Connected but contributing nothing is indistinguishable from
			// absent as far as the job is concerned.
			out.Missing = append(out.Missing, "connector "+want+" (no tools available)")
		}
	}
	return out
}

func matchesAny(available []string, want string) bool {
	for _, a := range available {
		if capabilityMatches(a, want) {
			return true
		}
	}
	return false
}

func (c *Collector) findServer(want string) (MCPServer, bool) {
	for _, srv := range c.Init.MCPServers {
		if capabilityMatches(srv.Name, want) {
			return srv, true
		}
	}
	return MCPServer{}, false
}

// hasToolsFrom reports whether the init event lists any tool contributed by a
// server. This is the signal that matters: the tool list is what Claude can
// actually call.
func (c *Collector) hasToolsFrom(srv MCPServer) bool {
	prefix := srv.ToolPrefix()
	for _, t := range c.Init.Tools {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// Run is everything the executor knows about a finished subprocess.
type Run struct {
	Collector *Collector
	// Cancelled is set when an operator stopped the run.
	Cancelled bool
	// TimedOut is set when the executor killed the run on the queue's timeout.
	TimedOut bool
	// Caps is the capability check performed against the init event.
	Caps Capabilities
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

	// An explicit cancellation outranks everything: the operator's intent is
	// not a failure, and the run was stopped before it could finish.
	if r.Cancelled {
		res.Status = store.StatusCancelled
		res.ErrorMessage = "cancelled while running"
		return Classification{Result: res}
	}

	// Ordered most-specific first: a capability problem explains everything
	// downstream of it, and a timeout explains a missing result.
	if len(r.Caps.Missing) > 0 {
		return fail(store.ErrKindCapabilityMissing,
			"required capabilities unavailable: "+strings.Join(r.Caps.Missing, ", "))
	}
	if len(r.Caps.Pending) > 0 {
		return fail(store.ErrKindCapabilityPending,
			"connectors were still connecting: "+strings.Join(r.Caps.Pending, ", "))
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
	// Verified on 2.1.282: subtype "error_max_turns", terminal_reason
	// "max_turns", and errors ["Reached maximum number of turns (N)"].
	if c.Result != nil && (c.Result.Subtype == "error_max_turns" || c.Result.TerminalReason == "max_turns") {
		return fail(store.ErrKindMaxTurns,
			nonEmpty(firstLine(strings.Join(c.Result.Errors, "; ")), "hit the queue's max_turns limit"))
	}
	if r.ExitErr != nil {
		return fail(store.ErrKindCLI, cliErrorMessage(r))
	}
	if c.Result == nil {
		return fail(store.ErrKindCLI, "claude exited without a result event")
	}
	if c.Result.IsError {
		return fail(store.ErrKindCLI, firstLine(nonEmpty(
			strings.Join(c.Result.Errors, "; "), c.Result.Result, r.Stderr, "claude reported an error")))
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
	if res := r.Collector.Result; res != nil {
		sb.WriteString(res.Result)
		sb.WriteString("\n")
		// errors[] is often the only place a failure is spelled out: a failed
		// run may carry no `result` field at all.
		for _, e := range res.Errors {
			sb.WriteString(e)
			sb.WriteString("\n")
		}
		if s, ok := res.rawString("error", "message"); ok {
			sb.WriteString(s)
			sb.WriteString("\n")
		}
	}
	sb.WriteString(r.Stderr)
	return sb.String()
}

func cliErrorMessage(r Run) string {
	if res := r.Collector.Result; res != nil {
		if s := firstLine(strings.Join(res.Errors, "; ")); s != "" {
			return s
		}
	}
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

// parseDenials reads the result line's permission_denials field, accepting
// either a list of tool names or a list of objects naming the tool.
func parseDenials(e *Event) []string {
	raw, ok := e.rawJSON("permission_denials", "permissionDenials")
	if !ok {
		return nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		return names
	}
	// A failed array decode leaves zero-value elements behind, so start clean
	// rather than appending to the partial result.
	names = nil
	var objs []struct {
		ToolName string `json:"tool_name"`
		Tool     string `json:"tool"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(raw, &objs); err != nil {
		return nil
	}
	for _, o := range objs {
		if n := nonEmpty(o.ToolName, o.Tool, o.Name); n != "" {
			names = append(names, n)
		}
	}
	return names
}
