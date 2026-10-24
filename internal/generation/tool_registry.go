package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/fin-assist/assistant/internal/tooldata"
)

// ErrInvalidTools identifies an invalid startup tool configuration.
var ErrInvalidTools = errors.New("invalid generation tool registry")

// ToolDefinition is the provider-facing contract of one server-owned function.
type ToolDefinition struct {
	Name, Description string
	Parameters        json.RawMessage
}

// Tool binds a definition to a typed argument parser and its execution behavior.
// Construct read extensions with NewReadTool; terminal/presentation tools are built in.
type Tool struct {
	definition ToolDefinition
	prepare    func(ModelItem) (toolPlan, error)
	priority   bool
}

// NewReadTool binds one typed reader without extending the engine's dependencies.
// Decode is pure: the complete batch is prepared before any reader is called.
func NewReadTool[A any](definition ToolDefinition, decode func(json.RawMessage) (A, error), read func(context.Context, tooldata.Subject, A) (tooldata.Result, error), required bool) Tool {
	tool := Tool{definition: definition}
	if decode == nil || read == nil {
		return tool
	}
	tool.prepare = func(item ModelItem) (toolPlan, error) {
		args, err := decode(json.RawMessage(item.Arguments))
		if err != nil {
			return toolPlan{}, errModelProtocol
		}
		return toolPlan{item: item, args: args, required: required, run: func(ctx context.Context, subject tooldata.Subject) (tooldata.Result, error) {
			return read(ctx, subject, args)
		}}, nil
	}
	return tool
}

type registeredTool struct {
	tool   Tool
	schema argumentSchema
}

// ToolRegistry is a validated, immutable snapshot shared by execution and metrics.
// Returned schemas are copies, so request construction cannot mutate later steps.
type ToolRegistry struct {
	tools   map[string]registeredTool
	schemas []json.RawMessage
}

// NewToolRegistry validates all functions before a model request can be sent.
func NewToolRegistry(tools ...Tool) (*ToolRegistry, error) {
	if len(tools) == 0 || len(tools) > 64 {
		return nil, ErrInvalidTools
	}
	r := &ToolRegistry{tools: make(map[string]registeredTool, len(tools))}
	for _, tool := range tools {
		d := tool.definition
		if !validToolName(d.Name) || d.Name == "unknown" || d.Name == "web_search" || tool.prepare == nil || d.Description == "" || len(d.Description) > 4096 || !utf8.ValidString(d.Description) || len(d.Parameters) > 16384 || !validObject(d.Parameters) {
			return nil, ErrInvalidTools
		}
		if _, exists := r.tools[d.Name]; exists {
			return nil, ErrInvalidTools
		}
		var schema argumentSchema
		if json.Unmarshal(d.Parameters, &schema) != nil || schema.Type != "object" || !schema.valid() {
			return nil, ErrInvalidTools
		}
		d.Parameters = bytes.Clone(d.Parameters)
		tool.definition = d
		wire, err := json.Marshal(map[string]any{"type": "function", "name": d.Name, "description": d.Description, "parameters": d.Parameters})
		if err != nil {
			return nil, ErrInvalidTools
		}
		r.tools[d.Name] = registeredTool{tool: tool, schema: schema}
		r.schemas = append(r.schemas, wire)
	}
	return r, nil
}

func validToolName(name string) bool {
	if len(name) < 1 || len(name) > 64 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, c := range []byte(name) {
		if c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// MetricName maps arbitrary input to the finite startup registry or "unknown".
func (r *ToolRegistry) MetricName(name string) string {
	if r != nil {
		if _, ok := r.tools[name]; ok {
			return name
		}
	}
	return "unknown"
}

func (r *ToolRegistry) definitions(searchRemaining int) []json.RawMessage {
	result := make([]json.RawMessage, 0, len(r.schemas)+1)
	for _, schema := range r.schemas {
		result = append(result, bytes.Clone(schema))
	}
	if searchRemaining > 0 {
		result = append(result, json.RawMessage(`{"type":"web_search"}`))
	}
	return result
}

func (r *ToolRegistry) priority(name string) bool {
	return r.tools[name].tool.priority
}

func (r *ToolRegistry) prepare(item ModelItem) (toolPlan, error) {
	entry, ok := r.tools[item.Name]
	raw := json.RawMessage(item.Arguments)
	if !ok || !validObject(raw) || !entry.schema.accepts(raw) {
		return toolPlan{}, errModelProtocol
	}
	return entry.tool.prepare(item)
}

// argumentSchema validates the closed structural subset used by tool contracts.
// Value constraints remain in each typed parser, preserving established defaults.
type argumentSchema struct {
	Type                 string                    `json:"type"`
	Properties           map[string]argumentSchema `json:"properties"`
	AdditionalProperties *bool                     `json:"additionalProperties"`
	Items                *argumentSchema           `json:"items"`
	Required             []string                  `json:"required"`
}

func (s argumentSchema) valid() bool {
	switch s.Type {
	case "object":
		if s.Properties == nil || s.AdditionalProperties == nil || *s.AdditionalProperties {
			return false
		}
		for name, property := range s.Properties {
			if name == "" || !property.valid() {
				return false
			}
		}
		seen := map[string]bool{}
		for _, name := range s.Required {
			if _, ok := s.Properties[name]; !ok || seen[name] {
				return false
			}
			seen[name] = true
		}
		return true
	case "array":
		return s.Items != nil && s.Items.valid()
	case "string", "integer", "number", "boolean":
		return true
	default:
		return false
	}
}

func (s argumentSchema) accepts(raw json.RawMessage) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	switch s.Type {
	case "object":
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil || values == nil {
			return false
		}
		for _, name := range s.Required {
			if _, ok := values[name]; !ok {
				return false
			}
		}
		for name, value := range values {
			property, ok := s.Properties[name]
			if !ok || !property.accepts(value) {
				return false
			}
		}
	case "array":
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		for _, value := range values {
			if !s.Items.accepts(value) {
				return false
			}
		}
	case "string":
		var value string
		return json.Unmarshal(raw, &value) == nil
	case "boolean":
		var value bool
		return json.Unmarshal(raw, &value) == nil
	case "integer":
		value := bytes.TrimSpace(raw)
		return numericPrefix(value) && !bytes.ContainsAny(value, ".eE")
	case "number":
		value := bytes.TrimSpace(raw)
		return numericPrefix(value)
	}
	return true
}

func numericPrefix(value []byte) bool {
	return len(value) > 0 && (value[0] == '-' || value[0] >= '0' && value[0] <= '9')
}
