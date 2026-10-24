package generation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/tooldata"
)

type extraReadArgs struct {
	Currency string `json:"currency"`
}
type extraReader struct{ calls int }

func (r *extraReader) ReadBalance(_ context.Context, subject tooldata.Subject, args extraReadArgs) (tooldata.Result, error) {
	r.calls++
	if subject.UserID != "server-user" || subject.TransferMode != "without" || args.Currency != "RUB" {
		return tooldata.Result{}, errors.New("wrong subject or arguments")
	}
	return tooldata.Result{JSON: json.RawMessage(`{"amount_minor":"9007199254740993"}`)}, nil
}
func extraReadTool(reader *extraReader) Tool {
	return NewReadTool(ToolDefinition{Name: "read_balance", Description: "Read one trusted balance", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"currency":{"type":"string"}},"required":["currency"]}`)}, func(raw json.RawMessage) (extraReadArgs, error) {
		var args extraReadArgs
		err := json.Unmarshal(raw, &args)
		if err == nil && args.Currency != "RUB" {
			err = errors.New("unsupported currency")
		}
		return args, err
	}, reader.ReadBalance, true)
}

func TestUnitRegistryExtensionRunsWithoutCommonDataPort(t *testing.T) {
	// extraReader has one method and deliberately does not implement tooldata.Data.
	reader := &extraReader{}
	registry, err := NewToolRegistry(extraReadTool(reader))
	if err != nil {
		t.Fatal(err)
	}
	store := &engineStore{}
	observer := &engineObserver{}
	limits := engineLimits()
	limits.Observer = observer
	model := &engineModel{script: func(_ context.Context, request ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
		if len(request.Tools) != 2 || !strings.Contains(string(request.Tools[0]), "read_balance") {
			t.Fatal("registered schema missing")
		}
		if step == 1 {
			return ModelResult{Output: []ModelItem{engineTool("balance", "read_balance", `{"currency":"RUB"}`)}}, nil
		}
		if !strings.Contains(string(request.Input[len(request.Input)-1]), "9007199254740993") {
			t.Fatal("typed result lost precision")
		}
		return engineFinal(emit, "Ответ")
	}}
	engine, err := NewWithTools(store, store, store, model, registry, limits, func() time.Time { return time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 || model.network != 2 || store.outcome.ErrorCode != "" || strings.Join(observer.functions, ",") != "read_balance" || registry.MetricName("read_balance") != "read_balance" || registry.MetricName("PRIVATE arbitrary model name") != "unknown" {
		t.Fatalf("extension failed: %+v calls=%d observer=%+v", store.outcome, reader.calls, observer)
	}
}

func TestUnitRegistryExtensionStillValidatesWholeBatchBeforeEffects(t *testing.T) {
	for _, invalid := range []string{`{"currency":"USD"}`, `{"currency":"RUB","user_id":"other"}`, `{"currency":"RUB","currency":"USD"}`, `{"Currency":"RUB"}`, `{"currency":null}`, `{}`} {
		t.Run(invalid, func(t *testing.T) {
			reader := &extraReader{}
			registry, err := NewToolRegistry(extraReadTool(reader))
			if err != nil {
				t.Fatal(err)
			}
			store := &engineStore{}
			model := &engineModel{script: func(context.Context, ModelRequest, func(ModelDelta) error, int) (ModelResult, error) {
				return ModelResult{Output: []ModelItem{engineTool("good", "read_balance", `{"currency":"RUB"}`), engineTool("bad", "read_balance", invalid)}}, nil
			}}
			engine, err := NewWithTools(store, store, store, model, registry, engineLimits(), time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if err = engine.Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			if reader.calls != 0 || store.outcome.ErrorCode != "MODEL_ERROR" {
				t.Fatalf("invalid batch had effects: %+v calls=%d", store.outcome, reader.calls)
			}
		})
	}
}

func TestUnitRegistryRejectsInvalidStartupDefinitions(t *testing.T) {
	good := extraReadTool(&extraReader{})
	cases := map[string][]Tool{"empty": {}, "duplicate": {good, good}, "zero handler": {{}}, "nil decoder": {NewReadTool[extraReadArgs](good.definition, nil, (&extraReader{}).ReadBalance, true)}, "nil reader": {NewReadTool[extraReadArgs](good.definition, func(json.RawMessage) (extraReadArgs, error) { return extraReadArgs{}, nil }, nil, true)}}
	for _, name := range []string{"", strings.Repeat("a", 65), "Upper", "user-id", "user.id", "1tool", "user id", "web_search", "unknown", "приватное"} {
		tool := good
		tool.definition.Name = name
		cases["name/"+name] = []Tool{tool}
	}
	for _, schema := range []string{`null`, `[]`, `{}`, `{"type":"object"}`, `{"type":"string"}`, `{"type":"object","properties":{},"additionalProperties":true}`, `{"type":"object","properties":{},"additionalProperties":false,"required":["missing"]}`, `{"type":"object","properties":{"x":{"type":"future"}},"additionalProperties":false}`, `{"type":"object","properties":{},"properties":{},"additionalProperties":false}`, `{"type":"object","properties":{"x":{"type":"array"}},"additionalProperties":false}`, strings.Repeat(" ", 16384) + `{}`} {
		tool := good
		tool.definition.Parameters = json.RawMessage(schema)
		cases["schema/"+schema[:min(len(schema), 80)]] = []Tool{tool}
	}
	for i := 0; i < 65; i++ {
		tool := good
		tool.definition.Name = fmt.Sprint("tool", i)
		cases["too many"] = append(cases["too many"], tool)
	}
	for name, tools := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewToolRegistry(tools...); !errors.Is(err, ErrInvalidTools) {
				t.Fatalf("invalid startup accepted: %v", err)
			}
		})
	}
}

func TestUnitRegistryOwnsDefinitionAndRequestSchemaCopies(t *testing.T) {
	tool := extraReadTool(&extraReader{})
	registry, err := NewToolRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	original := string(registry.definitions(0)[0])
	for i := range tool.definition.Parameters {
		tool.definition.Parameters[i] = ' '
	}
	request := registry.definitions(1)
	for i := range request[0] {
		request[0][i] = 'x'
	}
	request[1][0] = 'x'
	if string(registry.definitions(0)[0]) != original || string(registry.definitions(1)[1]) != `{"type":"web_search"}` {
		t.Fatal("caller changed immutable registry")
	}
	if _, err := registry.prepare(engineTool("read", "read_balance", `{"currency":"RUB"}`)); err != nil {
		t.Fatal("caller changed parser schema", err)
	}
	if _, err := NewWithTools(&engineStore{}, &engineStore{}, &engineStore{}, &engineModel{}, nil, engineLimits(), time.Now); !errors.Is(err, ErrInvalidBudget) {
		t.Fatal("nil registry accepted")
	}
}

func TestUnitRegistryEnforcesPrimitiveShapesBeforeCustomDecoder(t *testing.T) {
	for _, kind := range []string{"integer", "number", "string", "boolean"} {
		definition := ToolDefinition{Name: "typed_value", Description: "Typed extension", Parameters: json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,"properties":{"value":{"type":%q}}}`, kind))}
		decoded := 0
		// This deliberately permissive decoder proves that registry structure checks
		// do not depend on the extension remembering the schema's primitive type.
		tool := NewReadTool(definition, func(json.RawMessage) (struct{}, error) { decoded++; return struct{}{}, nil }, func(context.Context, tooldata.Subject, struct{}) (tooldata.Result, error) {
			return tooldata.Result{}, nil
		}, false)
		registry, err := NewToolRegistry(tool)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{`0`, `-9223372036854775808`, `9007199254740993`, `0.5`, `1e0`, `"1"`, `true`, `[]`, `{}`, `null`} {
			t.Run(kind+"/"+value, func(t *testing.T) {
				numeric := value == `0` || value == `-9223372036854775808` || value == `9007199254740993` || value == `0.5` || value == `1e0`
				valid := kind == "number" && numeric || kind == "integer" && numeric && !strings.ContainsAny(value, ".eE") || kind == "string" && value == `"1"` || kind == "boolean" && value == `true`
				before := decoded
				_, err := registry.prepare(engineTool("call", "typed_value", `{"value":`+value+`}`))
				if valid != (err == nil) || (!valid && decoded != before) {
					t.Fatalf("shape %s value %s accepted=%v decoded=%d", kind, value, err == nil, decoded-before)
				}
			})
		}
	}
}

func TestUnitBuiltinRegistryWithoutReadersSupportsDisabledModel(t *testing.T) {
	registry, err := NewToolRegistry(BuiltinTools(nil)...)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.definitions(0)) != 8 || registry.MetricName("off_topic") != "off_topic" {
		t.Fatal("disabled model lost function definitions or metrics")
	}
	for _, name := range []string{"get_data_overview", "aggregate_operations", "find_operations", "get_analytics", "list_periods"} {
		plan, err := registry.prepare(engineTool("read", name, `{}`))
		if err != nil {
			t.Fatal(name, err)
		}
		result, err := plan.run(t.Context(), tooldata.Subject{UserID: "server-user", TransferMode: "without"})
		if !errors.Is(err, tooldata.ErrUnavailable) || len(result.JSON) != 0 {
			t.Fatalf("missing reader %s returned result=%+v error=%v", name, result, err)
		}
	}
}

func TestUnitRegistryRejectsNestedStructuralAndBuiltinSemanticErrors(t *testing.T) {
	registry := builtinRegistryForTest(t)
	for _, tc := range []struct{ name, args string }{
		{"aggregate_operations", `{"filter":[]}`},
		{"aggregate_operations", `{"filter":{"q":{}}}`},
		{"aggregate_operations", `{"group_by":{}}`},
		{"aggregate_operations", `{"group_by":[1]}`},
		{"aggregate_operations", `{"group_by":[null]}`},
		{"aggregate_operations", `{"limit":-1}`},
		{"aggregate_operations", `{"limit":201}`},
		{"aggregate_operations", `{"group_by":["month","category","merchant"]}`},
		{"aggregate_operations", `{"all_time":true,"filter":{"from":"2026-10-01T00:00:00Z"}}`},
		{"find_operations", `{"limit":101}`},
		{"find_operations", `{"order":"invalid"}`},
		{"get_analytics", `{"period":"week"}`},
		{"list_periods", `{"period":"week"}`},
	} {
		if _, err := registry.prepare(engineTool("read", tc.name, tc.args)); !errors.Is(err, errModelProtocol) {
			t.Fatalf("invalid arguments accepted: %s %s: %v", tc.name, tc.args, err)
		}
	}
}
