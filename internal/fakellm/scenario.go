// Package fakellm serves deterministic, stateless Responses scenarios over HTTP.
// It replaces only the model, never financial services or assistant policy.
package fakellm

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"text/template"
)

//go:embed scenarios/*.json
var scenarios embed.FS

// Scenario selects the latest user question; exactly one scenario is the fallback.
type Scenario struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern,omitempty"`
	Default bool   `json:"default,omitempty"`
	Steps   []Step `json:"steps"`
}

// Step describes one HTTP response and optional assertions on the real request.
// Delays are milliseconds and are cancelled with the request context.
type Step struct {
	Output              []Output `json:"output,omitempty"`
	Usage               Usage    `json:"usage"`
	DelayMS             int      `json:"delay_ms,omitempty"`
	ChunkDelayMS        int      `json:"chunk_delay_ms,omitempty"`
	HTTPStatus          int      `json:"http_status,omitempty"`
	Fault               string   `json:"fault,omitempty"` // timeout, disconnect or refusal
	MaxToolCalls        *int     `json:"max_tool_calls,omitempty"`
	WithoutTools        []string `json:"without_tools,omitempty"`
	InstructionsContain []string `json:"instructions_contain,omitempty"`
}

// Output is a completed function, search or message. Text chunks are Go templates
// over .Question and .Results (decoded function outputs keyed by call_id).
type Output struct {
	Type string `json:"type"`
	// CallID is a suite-local alias; the wire uses Name and a unique item ID.
	CallID      string            `json:"call_id,omitempty"`
	Name        string            `json:"name,omitempty"`
	Arguments   json.RawMessage   `json:"arguments,omitempty"`
	Text        []string          `json:"text,omitempty"`
	Annotations []json.RawMessage `json:"annotations,omitempty"`
}

// Usage is scenario data, not a price estimate or a tokenizer.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	ToolTokens   int64 `json:"tool_tokens"`
}

type compiledScenario struct {
	Scenario
	pattern *regexp.Regexp
}

// Handler is immutable after construction and keeps no dialogue or credentials.
type Handler struct{ scenarios []compiledScenario }

// Default loads the version-controlled JSON scenarios embedded in both binaries.
func Default() (*Handler, error) {
	raw, err := scenarios.ReadFile("scenarios/default.json")
	if err != nil {
		return nil, err
	}
	return Load(bytes.NewReader(raw))
}

// Load validates a bounded JSON scenario suite before accepting any traffic.
func Load(r io.Reader) (*Handler, error) {
	var suite []Scenario
	raw, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("fake scenario size limit exceeded")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&suite) != nil || d.Decode(new(any)) != io.EOF || len(suite) == 0 || len(suite) > 100 {
		return nil, errors.New("invalid fake model scenarios")
	}
	h := &Handler{}
	ids := map[string]bool{}
	fallbacks := 0
	for _, s := range suite {
		if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`).MatchString(s.ID) || ids[s.ID] || len(s.Steps) == 0 || len(s.Steps) > 32 || (s.Default && s.Pattern != "") || (!s.Default && s.Pattern == "") {
			return nil, errors.New("invalid fake model scenario")
		}
		ids[s.ID] = true
		p, err := regexp.Compile(s.Pattern)
		if err != nil {
			return nil, errors.New("invalid fake model pattern")
		}
		if s.Default {
			fallbacks++
		}
		calls := map[string]bool{}
		for _, step := range s.Steps {
			if err := validateStep(step, calls); err != nil {
				return nil, err
			}
		}
		h.scenarios = append(h.scenarios, compiledScenario{s, p})
	}
	if fallbacks != 1 {
		return nil, errors.New("fake model requires one default scenario")
	}
	return h, nil
}

func textTemplate(s string) (*template.Template, error) {
	return template.New("text").Option("missingkey=error").Funcs(template.FuncMap{"json": func(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }}).Parse(s)
}

func validateStep(s Step, calls map[string]bool) error {
	if s.DelayMS < 0 || s.DelayMS > 180000 || s.ChunkDelayMS < 0 || s.ChunkDelayMS > 10000 || len(s.Output) > 64 || (s.HTTPStatus != 0 && (s.HTTPStatus < 400 || s.HTTPStatus > 599)) || (s.Fault != "" && s.Fault != "timeout" && s.Fault != "disconnect" && s.Fault != "refusal") || (s.MaxToolCalls != nil && *s.MaxToolCalls < 0) || s.Usage.InputTokens < 0 || s.Usage.OutputTokens < 0 || s.Usage.TotalTokens < 0 || s.Usage.ToolTokens < 0 {
		return errors.New("invalid fake model step")
	}
	for _, o := range s.Output {
		switch o.Type {
		case "function_call":
			var args map[string]json.RawMessage
			if o.CallID == "" || o.Name == "" || calls[o.CallID] || json.Unmarshal(o.Arguments, &args) != nil || args == nil {
				return errors.New("invalid fake function")
			}
			calls[o.CallID] = true
		case "web_search_call":
		case "message":
			if len(o.Text) == 0 {
				return errors.New("empty fake message")
			}
			for _, s := range o.Text {
				if _, err := textTemplate(s); err != nil {
					return errors.New("invalid fake template")
				}
			}
		default:
			return errors.New("invalid fake output type")
		}
	}
	return nil
}
