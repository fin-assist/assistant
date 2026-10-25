package generation

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

type wireToolForTest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireSchemaForTest struct {
	Type        string                       `json:"type"`
	Description string                       `json:"description"`
	Properties  map[string]wireSchemaForTest `json:"properties"`
	Items       *wireSchemaForTest           `json:"items"`
}

func builtinWireToolsForTest(t *testing.T) (map[string]wireToolForTest, int) {
	t.Helper()
	raw, err := json.Marshal(builtinRegistryForTest(t).definitions(0))
	if err != nil {
		t.Fatal(err)
	}
	var list []wireToolForTest
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	tools := make(map[string]wireToolForTest, len(list))
	for _, tool := range list {
		tools[tool.Name] = tool
	}
	return tools, len(raw)
}

func TestUnitBuiltinToolDescriptionsAreDistinctAndBounded(t *testing.T) {
	tools, _ := builtinWireToolsForTest(t)
	want := []string{"get_data_overview", "aggregate_operations", "find_operations", "get_analytics", "list_periods", "off_topic", "present_result", "cannot_answer"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	seen := map[string]string{}
	for _, name := range want {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("missing tool %s", name)
		}
		d := tool.Description
		if strings.TrimSpace(d) == "" || len(d) > 4096 || !utf8.ValidString(d) {
			t.Fatalf("%s: invalid description %q", name, d)
		}
		// Each description is one or two sentences to keep the per-step input cost modest.
		if sentences := strings.Count(d, ". ") + 1; sentences > 2 || len(d) > 300 {
			t.Fatalf("%s: description too long (%d bytes, %d sentences)", name, len(d), sentences)
		}
		if other, duplicate := seen[d]; duplicate {
			t.Fatalf("%s repeats the description of %s", name, other)
		}
		seen[d] = name
	}
}

func TestUnitBuiltinToolDescriptionsStateRoutingRules(t *testing.T) {
	tools, _ := builtinWireToolsForTest(t)
	for name, fragments := range map[string][]string{
		"aggregate_operations": {"find_operations"},
		"find_operations":      {"not to compute totals"},
		"get_analytics":        {"list_periods"},
		"list_periods":         {"period_key", "get_analytics"},
		"present_result":       {"previous step", "never in the same batch"},
		"cannot_answer":        {"{}", "already read", "off_topic"},
		"off_topic":            {"{}", "cannot_answer"},
	} {
		for _, fragment := range fragments {
			if !strings.Contains(tools[name].Description, fragment) {
				t.Fatalf("%s description lacks %q: %s", name, fragment, tools[name].Description)
			}
		}
	}
}

func TestUnitBuiltinToolPropertiesAreDescribed(t *testing.T) {
	tools, _ := builtinWireToolsForTest(t)
	var walk func(tool, path string, schema wireSchemaForTest)
	walk = func(tool, path string, schema wireSchemaForTest) {
		for name, property := range schema.Properties {
			child := path + "." + name
			// Nested objects are described by their own properties.
			if property.Type != "object" && strings.TrimSpace(property.Description) == "" {
				t.Fatalf("%s%s has no description", tool, child)
			}
			walk(tool, child, property)
		}
	}
	for name, tool := range tools {
		var schema wireSchemaForTest
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		walk(name, "", schema)
	}
}

func TestUnitBuiltinToolFieldsStateUnitsAndFormats(t *testing.T) {
	tools, _ := builtinWireToolsForTest(t)
	property := func(tool string, path ...string) string {
		var schema wireSchemaForTest
		if err := json.Unmarshal(tools[tool].Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		for _, name := range path {
			schema = schema.Properties[name]
		}
		return schema.Description
	}
	for _, tool := range []string{"aggregate_operations", "find_operations"} {
		for field, fragments := range map[string][]string{
			"from":              {"Inclusive", "RFC 3339", "user's time zone offset"},
			"to":                {"Exclusive", "RFC 3339", "user's time zone offset", "[from, to)"},
			"amount_from_minor": {"minor currency units"},
			"amount_to_minor":   {"minor currency units"},
		} {
			for _, fragment := range fragments {
				if d := property(tool, "filter", field); !strings.Contains(d, fragment) {
					t.Fatalf("%s filter.%s lacks %q: %s", tool, field, fragment, d)
				}
			}
		}
		// A literal offset example would be copied for users outside that zone and shift month bounds.
		for _, field := range []string{"from", "to"} {
			if d := property(tool, "filter", field); strings.ContainsAny(d, "+") || strings.Contains(d, "T00:") {
				t.Fatalf("%s filter.%s contains a literal timestamp example: %s", tool, field, d)
			}
		}
		if d := property(tool, "all_time"); !strings.Contains(d, "not allowed together with from or to") {
			t.Fatalf("%s all_time: %s", tool, d)
		}
	}
	if d := property("present_result", "result_id"); !strings.Contains(d, "call_id") {
		t.Fatalf("present_result.result_id: %s", d)
	}
}

func TestUnitBuiltinToolSchemasStayWithinModestBudget(t *testing.T) {
	// The schemas are sent with every step and count toward the 24 000-token /
	// 65 536-byte per-step budget (docs/generation.md); keep them a small share.
	_, size := builtinWireToolsForTest(t)
	if size > 8192 { // about 2048 estimated tokens
		t.Fatalf("tool schemas use %d bytes", size)
	}
}
