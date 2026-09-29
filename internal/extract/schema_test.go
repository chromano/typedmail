package extract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWireSchema(t *testing.T) {
	in := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"required": ["n"],
		"properties": {
			"n": {"type": "integer", "minimum": 1},
			"amount": {"type": ["string", "null"], "pattern": "^\\d+$", "description": "Total."},
			"method": {"type": ["string", "null"], "enum": ["card", "check", null]},
			"items": {"type": "array", "minItems": 1, "maxItems": 5, "items": {"type": "object", "properties": {}}},
			"many": {"type": "array", "minItems": 2, "items": {"type": "string"}},
			"ship_to": {"type": ["object", "null"], "properties": {"city": {"type": "string"}}}
		}
	}`
	want := `{
		"type": "object",
		"required": ["amount", "items", "many", "method", "n", "ship_to"],
		"additionalProperties": false,
		"properties": {
			"n": {"type": "integer", "description": "Must satisfy: minimum 1."},
			"amount": {"anyOf": [{"type": "string"}, {"type": "null"}], "description": "Total. Must satisfy: pattern ^\\d+$."},
			"method": {"anyOf": [{"type": "string", "enum": ["card", "check"]}, {"type": "null", "enum": null}]},
			"items": {"description": "Must satisfy: maxItems 5.", "anyOf": [
				{"type": "array", "minItems": 1,
					"items": {"type": "object", "properties": {}, "required": [], "additionalProperties": false}},
				{"type": "null"}
			]},
			"many": {"description": "Must satisfy: minItems 2.", "anyOf": [
				{"type": "array", "items": {"type": "string"}},
				{"type": "null"}
			]},
			"ship_to": {"anyOf": [
				{"type": "object", "properties": {"city": {"anyOf": [{"type": "string"}, {"type": "null"}]}},
					"required": ["city"], "additionalProperties": false},
				{"type": "null"}
			]}
		}
	}`
	got, err := wireSchema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	// enumOf returns nil for the null branch, which has no enum keyword.
	w := mustJSON(t, want)
	delete(w["properties"].(map[string]any)["method"].(map[string]any)["anyOf"].([]any)[1].(map[string]any), "enum")
	if !reflect.DeepEqual(roundTrip(t, got), w) {
		g, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("wire schema:\n%s", g)
	}
}

func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return mustJSON(t, string(b))
}

func TestCompleteSchema(t *testing.T) {
	in := `{
		"type": "object",
		"required": ["id"],
		"properties": {
			"id": {"type": "string"},
			"note": {"type": "string"},
			"tags": {"type": ["array"], "items": {"type": "string"}},
			"status": {"type": "string", "enum": ["open", "closed"]},
			"kind": {"const": "order"},
			"total": {"$ref": "#/$defs/amount_or_null"},
			"subtotal": {"$ref": "#/$defs/amount"},
			"either": {"anyOf": [{"type": "string"}, {"type": "null"}]},
			"lines": {"type": "array", "items": {
				"type": "object", "required": ["sku"],
				"properties": {"sku": {"type": "string"}, "qty": {"type": ["integer", "null"]}}
			}}
		},
		"$defs": {
			"amount": {"type": "string"},
			"amount_or_null": {"type": ["string", "null"]}
		}
	}`
	want := `{
		"type": "object",
		"required": ["either", "id", "kind", "lines", "note", "status", "subtotal", "tags", "total"],
		"properties": {
			"id": {"type": "string"},
			"note": {"type": ["string", "null"]},
			"tags": {"type": ["array", "null"], "items": {"type": "string"}},
			"status": {"type": ["string", "null"], "enum": ["open", "closed", null]},
			"kind": {"anyOf": [{"const": "order"}, {"type": "null"}]},
			"total": {"$ref": "#/$defs/amount_or_null"},
			"subtotal": {"anyOf": [{"$ref": "#/$defs/amount"}, {"type": "null"}]},
			"either": {"anyOf": [{"type": "string"}, {"type": "null"}]},
			"lines": {"type": ["array", "null"], "items": {
				"type": "object", "required": ["qty", "sku"],
				"properties": {"sku": {"type": "string"}, "qty": {"type": ["integer", "null"]}}
			}}
		},
		"$defs": {
			"amount": {"type": "string"},
			"amount_or_null": {"type": ["string", "null"]}
		}
	}`
	got, err := completeSchema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip(t, got), mustJSON(t, want)) {
		g, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("complete schema:\n%s", g)
	}
}

func TestCompleteSchemaSurvivesRefCycles(t *testing.T) {
	in := `{"type": "object", "properties": {"a": {"$ref": "#/$defs/loop"}},
		"$defs": {"loop": {"$ref": "#/$defs/loop"}}}`
	if _, err := completeSchema(json.RawMessage(in)); err != nil {
		t.Fatal(err)
	}
}

func TestWireSchemaRejectsNonObjectSchemas(t *testing.T) {
	for _, s := range []string{`{}`, `{"type": "string"}`, `[]`, `not json`} {
		if _, err := wireSchema(json.RawMessage(s)); err == nil {
			t.Errorf("wireSchema(%s) succeeded, want an error", s)
		}
	}
}

// TestInboxSchemasUseOnlySupportedKeywords checks every schema in schemas/
// against the subset structured outputs accept.
func TestInboxSchemasUseOnlySupportedKeywords(t *testing.T) {
	files, err := filepath.Glob("../../schemas/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no schemas found: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := wireSchema(raw)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		walk(wire, func(path string, n map[string]any) {
			for _, k := range append(slices.Clone(described), "$schema", "oneOf") {
				if _, ok := n[k]; ok {
					t.Errorf("%s: %s still has %q", f, path, k)
				}
			}
			if _, ok := n["type"].([]any); ok {
				t.Errorf("%s: %s still has a type list", f, path)
			}
			if v, ok := n["minItems"].(float64); ok && v > 1 {
				t.Errorf("%s: %s has minItems %v", f, path, v)
			}
			if n["type"] == "object" && n["additionalProperties"] != false {
				t.Errorf("%s: %s object without additionalProperties: false", f, path)
			}
			if props, ok := n["properties"].(map[string]any); ok {
				required, _ := n["required"].([]any)
				if len(required) != len(props) {
					t.Errorf("%s: %s requires %v of its %d properties, want all", f, path, required, len(props))
				}
			}
		})
	}
}

func walk(node any, visit func(path string, n map[string]any)) {
	var rec func(path string, node any)
	rec = func(path string, node any) {
		switch n := node.(type) {
		case map[string]any:
			visit(path, n)
			for k, v := range n {
				rec(path+"/"+k, v)
			}
		case []any:
			for i, v := range n {
				rec(path+"/"+string(rune('0'+i)), v)
			}
		}
	}
	rec("", node)
}
