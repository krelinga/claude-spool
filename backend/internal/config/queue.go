package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Defaults applied to any queue field left unset in queues.yaml.
const (
	DefaultMaxTurns  = 30
	DefaultTimeout   = Duration(10 * 60 * 1e9)     // 10m
	DefaultRetention = Duration(180 * 24 * 3600e9) // 180d
	DefaultWeight    = 1.0

	// MinMaxTurns is the floor below which a queue cannot reliably produce a
	// structured outcome.
	MinMaxTurns = 5
)

// ArgSpec describes one structured argument a queue accepts alongside the free
// text input, so a share-sheet client can send data rather than prose.
//
// This is a deliberate subset of JSON Schema: enough to keep clients honest,
// small enough not to pull in a schema library. Unsupported keywords in
// queues.yaml are a config error rather than a silently ignored constraint.
type ArgSpec struct {
	Type        string   `yaml:"type" json:"type"`
	Required    bool     `yaml:"required" json:"required,omitempty"`
	Enum        []string `yaml:"enum" json:"enum,omitempty"`
	Format      string   `yaml:"format" json:"format,omitempty"`
	Description string   `yaml:"description" json:"description,omitempty"`
}

// OutcomeField is one extra property merged into the base outcome JSON schema
// handed to `claude --json-schema` (design §3.4 layer 2).
type OutcomeField struct {
	Type        string   `yaml:"type" json:"type,omitempty"`
	Enum        []string `yaml:"enum" json:"enum,omitempty"`
	Description string   `yaml:"description" json:"description,omitempty"`
}

// Requires declares the claude.ai capabilities a queue's prompt depends on.
// The executor checks these against the CLI's system/init event before letting
// Claude run, so a credential-mode switch that strips skills or connectors
// fails loudly instead of letting Claude improvise without its tools (§3.2).
type Requires struct {
	Skills     []string `yaml:"skills" json:"skills,omitempty"`
	Connectors []string `yaml:"connectors" json:"connectors,omitempty"`
}

// Queue is a named, config-defined job type. Clients send input; the queue
// supplies the prompt, the policy, and the tracking (§3.3).
type Queue struct {
	Name         string   `yaml:"-" json:"name"`
	Description  string   `yaml:"description" json:"description,omitempty"`
	Prompt       string   `yaml:"prompt" json:"prompt"`
	SystemPrompt string   `yaml:"system_prompt" json:"system_prompt,omitempty"`
	Requires     Requires `yaml:"requires" json:"requires,omitempty"`

	// Tools is the built-in tool set the run may use at all, passed to
	// --tools. This is the real restriction.
	//
	// Verified against CLI 2.1.282: --allowedTools only *pre-approves* tools,
	// it does not restrict them — a built-in the CLI considers safe (a
	// read-only `echo` through Bash, say) still runs when it is absent from
	// allowed_tools. --tools removes the tool outright. Leaving this empty
	// means every built-in is available, which for an unattended queue is
	// almost never what you want.
	Tools []string `yaml:"tools" json:"tools,omitempty"`
	// AllowedTools pre-approves tools so they are not denied for want of a
	// human, including MCP connector tools, which --tools does not cover.
	AllowedTools []string `yaml:"allowed_tools" json:"allowed_tools,omitempty"`
	// DisallowedTools denies specific tools outright and wins over
	// AllowedTools. Use it for MCP tools, which --tools cannot restrict.
	DisallowedTools  []string                `yaml:"disallowed_tools" json:"disallowed_tools,omitempty"`
	OutcomeExtension map[string]OutcomeField `yaml:"outcome_extension" json:"outcome_extension,omitempty"`
	Args             map[string]ArgSpec      `yaml:"args" json:"args,omitempty"`
	Model            string                  `yaml:"model" json:"model,omitempty"`
	MaxTurns         int                     `yaml:"max_turns" json:"max_turns"`
	// MaxBudgetUSD caps what one job may spend (--max-budget-usd). Zero means
	// claude.default_max_budget_usd. Context, not work, dominates cost, so a
	// queue that accidentally loads every connector's tools is expensive rather
	// than slow.
	MaxBudgetUSD float64  `yaml:"max_budget_usd" json:"max_budget_usd,omitempty"`
	Timeout      Duration `yaml:"timeout" json:"timeout"`
	Weight       float64  `yaml:"weight" json:"weight"`
	Notify       []string `yaml:"notify" json:"notify,omitempty"`
	Retention    Duration `yaml:"retention" json:"retention"`

	// ConfigHash identifies the exact definition a job ran under, so history
	// stays accurate after a template edit (§3.3). Derived, not configured.
	ConfigHash string `yaml:"-" json:"config_hash"`
}

// QueueSet is the parsed queues.yaml.
type QueueSet struct {
	Queues map[string]*Queue `yaml:"queues"`
}

// Names returns queue names in a stable order.
func (qs *QueueSet) Names() []string {
	names := make([]string, 0, len(qs.Queues))
	for n := range qs.Queues {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (qs *QueueSet) Get(name string) (*Queue, bool) {
	q, ok := qs.Queues[name]
	return q, ok
}

var queueNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// LoadQueues reads and validates queues.yaml.
func LoadQueues(path string) (*QueueSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read queues: %w", err)
	}
	return ParseQueues(b)
}

func ParseQueues(b []byte) (*QueueSet, error) {
	var qs QueueSet
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&qs); err != nil {
		return nil, fmt.Errorf("parse queues: %w", err)
	}
	if len(qs.Queues) == 0 {
		return nil, fmt.Errorf("no queues defined")
	}
	for name, q := range qs.Queues {
		if q == nil {
			return nil, fmt.Errorf("queue %q: empty definition", name)
		}
		q.Name = name
		q.applyDefaults()
		if err := q.validate(); err != nil {
			return nil, fmt.Errorf("queue %q: %w", name, err)
		}
		h, err := q.hash()
		if err != nil {
			return nil, fmt.Errorf("queue %q: %w", name, err)
		}
		q.ConfigHash = h
	}
	return &qs, nil
}

func (q *Queue) applyDefaults() {
	if q.MaxTurns == 0 {
		q.MaxTurns = DefaultMaxTurns
	}
	if q.Timeout == 0 {
		q.Timeout = DefaultTimeout
	}
	if q.Retention == 0 {
		q.Retention = DefaultRetention
	}
	if q.Weight == 0 {
		q.Weight = DefaultWeight
	}
}

var validArgTypes = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true}

func (q *Queue) validate() error {
	if !queueNameRe.MatchString(q.Name) {
		return fmt.Errorf("name must match %s", queueNameRe)
	}
	if strings.TrimSpace(q.Prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	// Structured output costs turns of its own: a run observed in the spike
	// needed four before emitting it, and hitting the limit first produces
	// error_max_turns with no outcome at all. A queue with a tiny max_turns
	// would fail every job in a way that looks like a model problem.
	if q.MaxTurns < MinMaxTurns {
		return fmt.Errorf("max_turns must be >= %d (structured output needs several turns)", MinMaxTurns)
	}
	if q.Timeout <= 0 {
		return fmt.Errorf("timeout must be > 0")
	}
	if q.Weight <= 0 {
		return fmt.Errorf("weight must be > 0")
	}
	if q.MaxBudgetUSD < 0 {
		return fmt.Errorf("max_budget_usd must not be negative")
	}
	for name, spec := range q.Args {
		if !validArgTypes[spec.Type] {
			return fmt.Errorf("arg %q: unsupported type %q", name, spec.Type)
		}
		if spec.Format != "" && spec.Format != "uri" {
			return fmt.Errorf("arg %q: unsupported format %q (only \"uri\")", name, spec.Format)
		}
	}
	// A template referring to an undeclared arg would silently render empty at
	// submit time; catch it at load instead.
	for _, tmpl := range []string{q.Prompt, q.SystemPrompt} {
		refs, err := templateRefs(tmpl)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if ref == "input" {
				continue
			}
			argName, ok := strings.CutPrefix(ref, "args.")
			if !ok {
				return fmt.Errorf("template references unknown placeholder {{%s}}", ref)
			}
			if _, ok := q.Args[argName]; !ok {
				return fmt.Errorf("template references {{args.%s}} but no such arg is declared", argName)
			}
		}
	}
	for _, ev := range q.Notify {
		if !validNotifyEvents[ev] {
			return fmt.Errorf("unknown notify event %q", ev)
		}
	}
	return nil
}

var validNotifyEvents = map[string]bool{
	"job.succeeded":   true,
	"job.failed":      true,
	"job.needs_input": true,
	"job.interrupted": true,
	"job.cancelled":   true,
}

// hash is computed over the JSON encoding of the queue, which sorts map keys
// and so is stable across reloads.
func (q *Queue) hash() (string, error) {
	c := *q
	c.ConfigHash = ""
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("hash queue: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}
