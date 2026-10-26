package fakellm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type request struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions"`
	Input        []json.RawMessage `json:"input"`
	Tools        []struct {
		Type       string          `json:"type"`
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	} `json:"tools"`
	Stream       bool  `json:"stream"`
	Store        *bool `json:"store"`
	MaxToolCalls *int  `json:"max_tool_calls"`
	Text         struct {
		Format struct {
			Type string `json:"type"`
		} `json:"format"`
	} `json:"text"`
	ResponseFormat json.RawMessage `json:"response_format"`
}

// ServeHTTP implements only POST /v1/responses. Invalid requests get fixed errors,
// never echoed dialogue, credentials or template errors.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer func() { _ = r.Body.Close() }()
	var req request
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 131072))
	if r.Header.Get("x-data-logging-enabled") != "false" || d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF || !req.Stream || req.Store == nil || *req.Store || req.Model == "" || len(req.Input) == 0 || req.MaxToolCalls == nil || *req.MaxToolCalls < 0 || (len(req.Tools) > 0 && ((req.Text.Format.Type != "" && req.Text.Format.Type != "text") || len(req.ResponseFormat) > 0)) {
		http.Error(w, "invalid Responses request or privacy policy", 400)
		return
	}
	scenario, step, data, err := h.selectStep(req)
	if err != nil {
		http.Error(w, "scenario request mismatch", 400)
		return
	}
	frames, err := makeFrames(scenario, step, req, data)
	if err != nil {
		http.Error(w, "scenario output mismatch", 400)
		return
	}
	if !pause(r.Context(), step.DelayMS) {
		return
	}
	if step.Fault == "timeout" {
		<-r.Context().Done()
		return
	}
	if step.HTTPStatus != 0 {
		http.Error(w, "synthetic model failure", step.HTTPStatus)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for i, f := range frames {
		if i > 0 && !pause(r.Context(), step.ChunkDelayMS) {
			return
		}
		if _, err := w.Write(f); err != nil {
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		if step.Fault == "disconnect" && i == len(frames)/2 {
			return
		}
	}
}

type templateData struct {
	Question string
	Results  map[string]any
	CallIDs  map[string]string
}

func (h *Handler) selectStep(req request) (string, Step, templateData, error) {
	data := templateData{Results: map[string]any{}}
	start := -1
	for i, raw := range req.Input {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return "", Step{}, data, errors.New("invalid input")
		}
		if m.Role == "user" {
			if json.Unmarshal(m.Content, &data.Question) != nil {
				return "", Step{}, data, errors.New("user content must be text")
			}
			start = i
		}
	}
	if start < 0 {
		return "", Step{}, data, errors.New("question missing")
	}
	var selected *compiledScenario
	for i := range h.scenarios {
		s := &h.scenarios[i]
		if !s.Default && s.pattern.MatchString(data.Question) {
			selected = s
			break
		}
	}
	if selected == nil {
		for i := range h.scenarios {
			if h.scenarios[i].Default {
				selected = &h.scenarios[i]
				break
			}
		}
	}
	if selected == nil {
		return "", Step{}, data, errors.New("default missing")
	}
	data.CallIDs = map[string]string{}
	for step, definition := range selected.Steps {
		for _, output := range definition.Output {
			if output.Type == "function_call" {
				data.CallIDs[output.CallID] = fmt.Sprintf("fake_%s_%d_%s", selected.ID, step, output.CallID)
			}
		}
	}
	index := 0
	for _, raw := range req.Input[start+1:] {
		var item struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return "", Step{}, data, errors.New("invalid continuation")
		}
		prefix := "fake_" + selected.ID + "_"
		if strings.HasPrefix(item.ID, prefix) {
			part, _, _ := strings.Cut(strings.TrimPrefix(item.ID, prefix), "_")
			n, e := strconv.Atoi(part)
			if e != nil || n < 0 || n >= len(selected.Steps) {
				return "", Step{}, data, errors.New("invalid step")
			}
			index = max(index, n+1)
		}
		if item.Type == "function_call_output" {
			decoder := json.NewDecoder(strings.NewReader(item.Output))
			decoder.UseNumber()
			var v any
			if item.CallID == "" || decoder.Decode(&v) != nil || decoder.Decode(new(any)) != io.EOF {
				return "", Step{}, data, errors.New("invalid tool output")
			}
			alias := ""
			for name, id := range data.CallIDs {
				if id == item.CallID {
					alias = name
					break
				}
			}
			if alias == "" {
				return "", Step{}, data, errors.New("unknown function output")
			}
			if _, exists := data.Results[alias]; exists {
				return "", Step{}, data, errors.New("duplicate tool output")
			}
			data.Results[alias] = v
		}
	}
	if index >= len(selected.Steps) {
		return "", Step{}, data, errors.New("scenario exhausted")
	}
	for _, previous := range selected.Steps[:index] {
		for _, o := range previous.Output {
			if o.Type == "function_call" {
				if _, ok := data.Results[o.CallID]; !ok {
					return "", Step{}, data, errors.New("function not executed")
				}
			}
		}
	}
	step := selected.Steps[index]
	if step.MaxToolCalls != nil && *step.MaxToolCalls != *req.MaxToolCalls {
		return "", Step{}, data, errors.New("search budget mismatch")
	}
	for _, absent := range step.WithoutTools {
		for _, tool := range req.Tools {
			if tool.Type == absent {
				return "", Step{}, data, errors.New("unexpected tool")
			}
		}
	}
	for _, s := range step.InstructionsContain {
		if !strings.Contains(req.Instructions, s) {
			return "", Step{}, data, errors.New("instruction missing")
		}
	}
	return fmt.Sprintf("fake_%s_%d", selected.ID, index), step, data, nil
}

func render(text string, data templateData) (string, error) {
	t, err := textTemplate(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err = t.Execute(&b, data); err != nil {
		return "", err
	}
	if b.Len() > 65536 {
		return "", errors.New("fake text too large")
	}
	return b.String(), nil
}
