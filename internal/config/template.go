package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Templates are logic-free on purpose: Mustache-style substitution and nothing
// else. Complex prompt logic belongs in the skill, not in queues.yaml (§3.3).
var placeholderRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.]+)\s*\}\}`)

// templateRefs returns the placeholder names used by a template.
func templateRefs(tmpl string) ([]string, error) {
	var refs []string
	for _, m := range placeholderRe.FindAllStringSubmatch(tmpl, -1) {
		refs = append(refs, m[1])
	}
	if i := strings.Index(tmpl, "{{"); i >= 0 {
		// Catch a malformed placeholder rather than passing it to Claude verbatim.
		if loc := placeholderRe.FindStringIndex(tmpl[i:]); loc == nil || loc[0] != 0 {
			return nil, fmt.Errorf("malformed placeholder near %q", excerpt(tmpl[i:]))
		}
	}
	return refs, nil
}

func excerpt(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}

// Render substitutes {{input}} and {{args.<name>}} into a template.
func (q *Queue) Render(tmpl, input string, args map[string]any) string {
	return placeholderRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		sub := placeholderRe.FindStringSubmatch(m)
		ref := sub[1]
		if ref == "input" {
			return input
		}
		if name, ok := strings.CutPrefix(ref, "args."); ok {
			if v, ok := args[name]; ok {
				return formatArg(v)
			}
			return ""
		}
		return m
	})
}

func formatArg(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// JSON numbers decode as float64; render integers without a ".0" tail.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// ValidateArgs checks submitted args against the queue's declared specs. It
// returns a client-facing error: these come straight from an HTTP body.
func (q *Queue) ValidateArgs(args map[string]any) error {
	for name, spec := range q.Args {
		v, present := args[name]
		if !present {
			if spec.Required {
				return fmt.Errorf("missing required arg %q", name)
			}
			continue
		}
		if err := spec.validateValue(name, v); err != nil {
			return err
		}
	}
	for name := range args {
		if _, ok := q.Args[name]; !ok {
			return fmt.Errorf("unknown arg %q", name)
		}
	}
	return nil
}

func (s ArgSpec) validateValue(name string, v any) error {
	switch s.Type {
	case "string":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("arg %q must be a string", name)
		}
		if s.Format == "uri" {
			u, err := url.Parse(str)
			if err != nil || !u.IsAbs() {
				return fmt.Errorf("arg %q must be an absolute URI", name)
			}
		}
		if len(s.Enum) > 0 {
			for _, e := range s.Enum {
				if e == str {
					return nil
				}
			}
			return fmt.Errorf("arg %q must be one of %s", name, strings.Join(s.Enum, ", "))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("arg %q must be a boolean", name)
		}
	case "number":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("arg %q must be a number", name)
		}
	case "integer":
		f, ok := v.(float64)
		if !ok || f != float64(int64(f)) {
			return fmt.Errorf("arg %q must be an integer", name)
		}
	}
	return nil
}
