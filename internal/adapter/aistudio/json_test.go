package aistudio

import (
	"encoding/json"
	"testing"
)

func TestUnitJSONBoundary(t *testing.T) {
	for _, raw := range []string{`{"type":"message","nested":[{"a":1}]}`, `{"s":"\\\""}`, `[]`, `null`, `42`, `true`} {
		var v json.RawMessage
		if err := decodeJSON([]byte(raw), &v); err != nil {
			t.Errorf("valid JSON rejected %q: %v", raw, err)
		}
	}
	for _, raw := range []string{``, `{"type":"message","type":"function_call"}`, `{"nested":{"a":1,"a":2}}`, `{"nested":[{"a":1,"a":2}]}`, `{} {}`, `[`, `}`, `{"a":}`, `{"a":1`, `[1,]`, `[true]false`, `{"a":1,1:2}`} {
		var v json.RawMessage
		if err := decodeJSON([]byte(raw), &v); err == nil {
			t.Errorf("invalid JSON accepted %q", raw)
		}
	}
	var target struct {
		N int `json:"n"`
	}
	if err := decodeJSON([]byte(`{"n":"wrong"}`), &target); err == nil {
		t.Fatal("typed decode failure ignored")
	}
}

func TestUnitJSONPreservesExactNumbers(t *testing.T) {
	var v map[string]any
	if err := decodeJSON([]byte(`{"value":9007199254740993}`), &v); err != nil || v["value"] != json.Number("9007199254740993") {
		t.Fatalf("%v %v", v, err)
	}
	if err := decodeJSON([]byte{'"', 0xff, '"'}, &v); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}
