package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type queue struct {
	Description      string                    `yaml:"description"`
	Prompt           string                    `yaml:"prompt"`
	SystemPrompt     string                    `yaml:"system_prompt"`
	AllowedTools     []string                  `yaml:"allowed_tools"`
	DisallowedTools  []string                  `yaml:"disallowed_tools"`
	OutcomeExtension map[string]map[string]any `yaml:"outcome_extension"`
}

func parse(t *testing.T, b []byte) map[string]queue {
	t.Helper()
	var f struct {
		Queues map[string]queue `yaml:"queues"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Queues
}

// The real queues file is the case that matters: every queue in it must get a
// twin that can reach nothing that writes.
func TestRealQueuesFile(t *testing.T) {
	in, err := os.ReadFile("../../spool/queues.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, names, err := generate(in)
	if err != nil {
		t.Fatal(err)
	}
	before, after := parse(t, in), parse(t, out)
	if len(names) != len(before) || len(after) != 2*len(before) {
		t.Fatalf("want a twin per queue: %d queues in, %d out, twins %v", len(before), len(after), names)
	}

	for name, orig := range before {
		if got := after[name]; got.SystemPrompt != orig.SystemPrompt || len(got.AllowedTools) != len(orig.AllowedTools) {
			t.Errorf("%s: the original queue was changed", name)
		}
		twin, ok := after[name+suffix]
		if !ok {
			t.Errorf("%s: no twin", name)
			continue
		}
		if twin.Prompt != orig.Prompt {
			t.Errorf("%s: twin prompt differs, so it would not test the same skill", name)
		}
		if !strings.HasPrefix(twin.SystemPrompt, "This is a DRY RUN.") || !strings.HasSuffix(twin.SystemPrompt, orig.SystemPrompt) {
			t.Errorf("%s: twin system prompt should be the preamble followed by the original", name)
		}
		if _, ok := twin.OutcomeExtension["would_write"]; !ok {
			t.Errorf("%s: twin has no would_write", name)
		}
		denied := map[string]bool{}
		for _, d := range twin.DisallowedTools {
			denied[d] = true
		}
		for _, a := range twin.AllowedTools {
			if denied[a] {
				t.Errorf("%s: twin still allows %s", name, a)
			}
		}
		for prefix, tools := range mutatingTools {
			for _, tool := range tools {
				if !denied[prefix+tool] && !coveredByWildcard(prefix+tool, denied) {
					t.Errorf("%s: twin does not deny %s", name, prefix+tool)
				}
			}
		}
	}
}

func TestUnknownConnectorIsAnError(t *testing.T) {
	in := []byte(`queues:
  q:
    prompt: "{{input}}"
    allowed_tools: [mcp__claude_ai_Linear__create-issue]
`)
	if _, _, err := generate(in); err == nil || !strings.Contains(err.Error(), "mutatingTools") {
		t.Fatalf("want an error naming mutatingTools, got %v", err)
	}
}

func TestTwinNameCollisionIsAnError(t *testing.T) {
	in := []byte(`queues:
  q:
    prompt: "{{input}}"
  q-dryrun:
    prompt: "{{input}}"
`)
	if _, _, err := generate(in); err == nil {
		t.Fatal("want an error when the twin's name is taken")
	}
}
