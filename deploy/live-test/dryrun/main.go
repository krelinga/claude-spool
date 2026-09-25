// Command dryrun derives a read-only twin of every queue in a queues.yaml, so
// any queue can be tested against a live login without writing anything.
//
//	go run . <in queues.yaml> <out queues.yaml>
//
// The output holds every original queue unchanged, plus <name>-dryrun for each.
// A twin keeps the prompt, the skill and the tool list, so it exercises the same
// plumbing, and differs in three ways:
//
//   - its system prompt starts with a dry-run preamble: read only, and report
//     what would have been written;
//   - every known mutating tool, built-in or connector, is disallowed, and a
//     deny beats an allow, so a mistake in the prompt cannot write;
//   - its outcome gains a would_write field, where that report lands.
//
// The prompt is advice; the deny list is the guarantee. So a queue that allows
// a tool from a connector this program does not know is an error, not a guess:
// add the connector's mutating tools to mutatingTools first.
package main

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// mutatingTools lists, per connector server prefix, every tool that creates,
// changes or deletes something. Names are as the CLI offers them on this
// account; the Notion list extends the one the spike observed.
var mutatingTools = map[string][]string{
	"mcp__claude_ai_Notion__": {
		"notion-convert-page-to-skill",
		"notion-create-attachment",
		"notion-create-comment",
		"notion-create-database",
		"notion-create-file-upload",
		"notion-create-folder",
		"notion-create-pages",
		"notion-create-view",
		"notion-duplicate-page",
		"notion-move-pages",
		"notion-send-message-to-session",
		"notion-spawn-session",
		"notion-stop-session",
		"notion-update-data-source",
		"notion-update-folder",
		"notion-update-page",
		"notion-update-view",
		"notion-upload-skill",
	},
	"mcp__claude_ai_Todoist__": {
		"add-comments",
		"add-filters",
		"add-labels",
		"add-projects",
		"add-reminders",
		"add-sections",
		"add-tasks",
		"complete-tasks",
		"delete-object",
		"import-project-template",
		"manage-assignments",
		"project-management",
		"project-move",
		"reorder-objects",
		"reschedule-tasks",
		"uncomplete-tasks",
		"update-comments",
		"update-filters",
		"update-labels",
		"update-projects",
		"update-reminders",
		"update-sections",
		"update-tasks",
	},
}

// mutatingBuiltins are denied too. A queue should not offer them anyway, but a
// twin must not depend on that.
var mutatingBuiltins = []string{"Bash", "Edit", "NotebookEdit", "Write"}

const suffix = "-dryrun"

const preamble = `This is a DRY RUN. Do not create, update, move or delete anything, in any
connected service or anywhere else. Search and read only. Tools that write are
denied; do not try to work around a denial.

Do everything else exactly as you would for real: load the skill, reach the
data sources, check what already exists, and resolve every value. Then put
what you would have written in would_write: where (database or service),
whether you would create or update and which page, and every property with
its value. Put the pages you read in links.

Set status to succeeded if you got that far, needs_input if the real run would
have had to ask a question (and ask it), and failed if you could not reach
what you needed or could not tell what to write.

The queue's own instructions follow. Apply them, except that nothing is written.
`

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: dryrun <in queues.yaml> <out queues.yaml>")
		os.Exit(2)
	}
	in, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "dryrun:", err)
		os.Exit(1)
	}
	out, names, err := generate(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dryrun: %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "dryrun:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "dryrun: wrote %s with %s\n", os.Args[2], strings.Join(names, ", "))
}

// generate returns the input with a twin appended for every queue, and the
// names of the twins.
func generate(in []byte) ([]byte, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil, fmt.Errorf("empty document")
	}
	queues := lookup(doc.Content[0], "queues")
	if queues == nil || queues.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("no queues mapping")
	}

	existing := map[string]bool{}
	for i := 0; i < len(queues.Content); i += 2 {
		existing[queues.Content[i].Value] = true
	}

	var names []string
	orig := len(queues.Content)
	for i := 0; i < orig; i += 2 {
		name := queues.Content[i].Value
		if strings.HasSuffix(name, suffix) {
			continue
		}
		twin := name + suffix
		if existing[twin] {
			return nil, nil, fmt.Errorf("queue %q already exists; the generator owns that name", twin)
		}
		body, err := makeTwin(name, queues.Content[i+1])
		if err != nil {
			return nil, nil, fmt.Errorf("queue %q: %w", name, err)
		}
		key := &yaml.Node{
			Kind:        yaml.ScalarNode,
			Tag:         "!!str",
			Value:       twin,
			HeadComment: fmt.Sprintf("Generated from %s by deploy/live-test/dryrun. Do not edit; edit %s.", name, name),
		}
		queues.Content = append(queues.Content, key, body)
		names = append(names, twin)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), names, nil
}

func makeTwin(name string, q *yaml.Node) (*yaml.Node, error) {
	if q.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a mapping")
	}
	t := clone(q)

	// Refuse before changing anything: every connector the queue can reach must
	// have its mutating tools listed, or the deny list has a hole.
	deny := map[string]bool{}
	for _, b := range mutatingBuiltins {
		deny[b] = true
	}
	for prefix, tools := range mutatingTools {
		for _, tool := range tools {
			deny[prefix+tool] = true
		}
	}
	for _, tool := range seqValues(lookup(t, "allowed_tools")) {
		if strings.HasPrefix(tool, "mcp__") && !knownServer(tool) {
			return nil, fmt.Errorf("allows %s, from a connector with no entry in mutatingTools", tool)
		}
	}

	if d := lookup(t, "description"); d != nil {
		d.Value = "Dry run of " + name + ": " + d.Value
	}

	sp := preamble
	if old := lookup(t, "system_prompt"); old != nil {
		sp += "\n" + old.Value
	}
	setScalar(t, "system_prompt", sp, yaml.LiteralStyle)

	if a := lookup(t, "allowed_tools"); a != nil {
		kept := a.Content[:0]
		for _, n := range a.Content {
			if !deny[n.Value] {
				kept = append(kept, n)
			}
		}
		a.Content = kept
	}

	d := lookup(t, "disallowed_tools")
	if d == nil {
		d = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setNode(t, "disallowed_tools", d)
	}
	have := map[string]bool{}
	for _, v := range seqValues(d) {
		have[v] = true
	}
	var add []string
	for tool := range deny {
		if !have[tool] && !coveredByWildcard(tool, have) {
			add = append(add, tool)
		}
	}
	sort.Strings(add)
	for i, tool := range add {
		n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: tool}
		if i == 0 {
			n.HeadComment = "Added for the dry run: everything that writes."
		}
		d.Content = append(d.Content, n)
	}

	ext := lookup(t, "outcome_extension")
	if ext == nil {
		ext = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setNode(t, "outcome_extension", ext)
	}
	if lookup(ext, "would_write") != nil {
		return nil, fmt.Errorf("outcome_extension already has would_write, which the twin needs")
	}
	var field yaml.Node
	if err := yaml.Unmarshal([]byte("type: string\ndescription: Where, create or update, and every property and value that would have been written.\n"), &field); err != nil {
		return nil, err
	}
	setNode(ext, "would_write", field.Content[0])

	// A test run's jobs are not worth keeping.
	setScalar(t, "retention", "30d", 0)
	return t, nil
}

func knownServer(tool string) bool {
	for prefix := range mutatingTools {
		if strings.HasPrefix(tool, prefix) {
			return true
		}
	}
	return false
}

// coveredByWildcard reports whether a server-prefix wildcard already in the
// list, like mcp__claude_ai_Todoist__*, denies tool.
func coveredByWildcard(tool string, have map[string]bool) bool {
	for h := range have {
		if strings.HasSuffix(h, "*") && strings.HasPrefix(tool, strings.TrimSuffix(h, "*")) {
			return true
		}
	}
	return false
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setNode(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

func setScalar(m *yaml.Node, key, value string, style yaml.Style) {
	setNode(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: style})
}

func seqValues(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, c.Value)
	}
	return out
}

func clone(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, ch := range n.Content {
		c.Content[i] = clone(ch)
	}
	return &c
}
