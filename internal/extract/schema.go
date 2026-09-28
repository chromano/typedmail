package extract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Structured outputs accept a subset of JSON Schema. wireSchema rewrites an
// inbox schema into that subset:
//
//   - type lists such as ["string", "null"] become anyOf
//   - constraints the API can't enforce (pattern, minimum, maxItems, ...) are
//     removed and described in the field's description instead, so the model
//     still sees them; validation against the full schema enforces them
//   - objects get additionalProperties: false, which the API requires
//
// The stored schema is left untouched.
func wireSchema(schema json.RawMessage) (map[string]any, error) {
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, fmt.Errorf("parse inbox schema: %w", err)
	}
	m, ok := root.(map[string]any)
	if !ok || m["type"] != "object" {
		return nil, fmt.Errorf("inbox schema must be a JSON Schema of type object")
	}
	return rewrite(m).(map[string]any), nil
}

// described are the keywords removed from the wire schema and restated in
// the description.
var described = []string{
	"pattern", "minLength", "maxLength",
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	"maxItems", "uniqueItems", "minProperties", "maxProperties",
}

// keywordsFor lists what each branch of a split type list keeps.
var keywordsFor = map[string][]string{
	"object":  {"properties", "required", "additionalProperties"},
	"array":   {"items", "minItems"},
	"string":  {"format", "enum", "const"},
	"number":  {"enum", "const"},
	"integer": {"enum", "const"},
	"boolean": {"enum", "const"},
}

func rewrite(node any) any {
	switch n := node.(type) {
	case []any:
		for i := range n {
			n[i] = rewrite(n[i])
		}
		return n
	case map[string]any:
		return rewriteSchema(n)
	default:
		return node
	}
}

func rewriteSchema(n map[string]any) map[string]any {
	for _, k := range []string{"properties", "$defs", "definitions"} {
		if sub, ok := n[k].(map[string]any); ok {
			for name, s := range sub {
				sub[name] = rewrite(s)
			}
		}
	}
	for _, k := range []string{"items", "anyOf", "allOf", "not"} {
		if sub, ok := n[k]; ok {
			n[k] = rewrite(sub)
		}
	}
	if sub, ok := n["oneOf"]; ok {
		delete(n, "oneOf")
		n["anyOf"] = rewrite(sub)
	}
	delete(n, "$schema")

	var notes []string
	for _, k := range described {
		if v, ok := n[k]; ok {
			notes = append(notes, fmt.Sprintf("%s %v", k, v))
			delete(n, k)
		}
	}
	if v, ok := n["minItems"].(float64); ok && v > 1 {
		notes = append(notes, fmt.Sprintf("minItems %v", v))
		delete(n, "minItems")
	}
	if len(notes) > 0 {
		sort.Strings(notes)
		desc, _ := n["description"].(string)
		n["description"] = strings.TrimSpace(desc + " Must satisfy: " + strings.Join(notes, ", ") + ".")
	}

	if types, ok := n["type"].([]any); ok {
		return splitTypes(n, types)
	}
	if n["type"] == "object" {
		n["additionalProperties"] = false
	}
	return n
}

// splitTypes turns {"type": ["string", "null"], ...} into an anyOf with one
// branch per type, each keeping only the keywords that apply to it.
func splitTypes(n map[string]any, types []any) map[string]any {
	var branches []any
	for _, t := range types {
		name, _ := t.(string)
		branch := map[string]any{"type": name}
		for _, k := range keywordsFor[name] {
			if v, ok := n[k]; ok {
				branch[k] = v
			}
		}
		if enum, ok := branch["enum"].([]any); ok {
			branch["enum"] = enumOf(enum, name)
		}
		if name == "object" {
			branch["additionalProperties"] = false
		}
		branches = append(branches, branch)
	}
	out := map[string]any{"anyOf": branches}
	for _, k := range []string{"title", "description", "default"} {
		if v, ok := n[k]; ok {
			out[k] = v
		}
	}
	return out
}

// enumOf keeps the enum values that belong to one branch of a type list:
// ["card", null] becomes ["card"] for string and is dropped for null.
func enumOf(enum []any, typ string) []any {
	var out []any
	for _, v := range enum {
		if v == nil {
			continue
		}
		if _, isString := v.(string); isString == (typ == "string") {
			out = append(out, v)
		}
	}
	return out
}
