package claudecli

import (
	"encoding/json"
	"fmt"

	"github.com/krelinga/claude-spool/backend/internal/config"
)

// OutcomeStatus is the task-level verdict Claude reports (design §3.4 layer 2).
// It is distinct from the run-level result: a run can exit cleanly having
// failed at the task.
type OutcomeStatus string

const (
	OutcomeSucceeded  OutcomeStatus = "succeeded"
	OutcomeFailed     OutcomeStatus = "failed"
	OutcomeNeedsInput OutcomeStatus = "needs_input"
)

// Outcome is the base structured output every queue produces.
type Outcome struct {
	Status OutcomeStatus `json:"status"`
	// Summary is one line, written to be readable in a notification.
	Summary  string   `json:"summary"`
	Question string   `json:"question,omitempty"`
	Links    []string `json:"links,omitempty"`
}

func (o Outcome) Valid() bool {
	switch o.Status {
	case OutcomeSucceeded, OutcomeFailed, OutcomeNeedsInput:
		return true
	}
	return false
}

// BuildOutcomeSchema merges a queue's outcome_extension into the base schema
// handed to --json-schema.
func BuildOutcomeSchema(q *config.Queue) ([]byte, error) {
	props := map[string]any{
		"status": map[string]any{
			"type":        "string",
			"enum":        []string{"succeeded", "failed", "needs_input"},
			"description": "succeeded if the task is done; needs_input if you must ask before creating or modifying anything; failed otherwise.",
		},
		"summary": map[string]any{
			"type":        "string",
			"description": "One line describing what happened, readable on its own in a phone notification.",
		},
		"question": map[string]any{
			"type":        "string",
			"description": "When status is needs_input, the specific question to put to the user.",
		},
		"links": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "URLs of anything created or updated.",
		},
	}
	for name, f := range q.OutcomeExtension {
		if _, taken := props[name]; taken {
			return nil, fmt.Errorf("outcome_extension may not redefine base field %q", name)
		}
		spec := map[string]any{}
		if f.Type != "" {
			spec["type"] = f.Type
		}
		if len(f.Enum) > 0 {
			spec["enum"] = f.Enum
		}
		if f.Description != "" {
			spec["description"] = f.Description
		}
		props[name] = spec
	}
	schema := map[string]any{
		"type":       "object",
		"properties": props,
		"required":   []string{"status", "summary"},
	}
	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("build outcome schema: %w", err)
	}
	return b, nil
}

// UnattendedPreamble is prepended to every queue's system prompt. It is the
// one instruction every queue shares: nobody is watching (§3.1).
const UnattendedPreamble = `You're running unattended from a queue. Nobody can answer questions.
Don't guess on ambiguous requests that create or modify data; report needs_input with a specific question.
Check whether something already exists before you create it.`

// BuildSystemPrompt composes the shared preamble with the queue's own addition.
func BuildSystemPrompt(q *config.Queue) string {
	if q.SystemPrompt == "" {
		return UnattendedPreamble
	}
	return UnattendedPreamble + "\n\n" + q.SystemPrompt
}
