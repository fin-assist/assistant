package fakellm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func pause(ctx context.Context, ms int) bool {
	if ms == 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func makeFrames(id string, step Step, req request, data templateData) ([][]byte, error) {
	var events []map[string]any
	add := func(kind string, fields map[string]any) {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["type"] = kind
		fields["sequence_number"] = len(events)
		events = append(events, fields)
	}
	add("response.created", map[string]any{"response": map[string]any{"id": id, "status": "in_progress", "output": []any{}}})
	if step.Fault == "refusal" {
		add("response.refusal.delta", map[string]any{"delta": "Synthetic refusal"})
		return encodeEvents(events)
	}
	tools := map[string]bool{}
	for _, t := range req.Tools {
		tools[t.Type] = true
		if t.Type == "function" {
			if len(t.Parameters) == 0 {
				return nil, errors.New("function schema missing")
			}
			tools["function:"+t.Name] = true
		}
	}
	output := []any{}
	searches := 0
	for _, o := range step.Output {
		if o.Type == "web_search_call" {
			if !tools["web_search"] || searches >= *req.MaxToolCalls {
				continue
			}
			searches++
		}
		index := len(output)
		itemID := fmt.Sprintf("%s_%d", id, index)
		item := map[string]any{"id": itemID, "type": o.Type, "status": "in_progress"}
		switch o.Type {
		case "function_call":
			if !tools["function:"+o.Name] {
				return nil, errors.New("function unavailable")
			}
			item["name"], item["call_id"], item["arguments"] = o.Name, o.CallID, ""
		case "message":
			item["role"], item["content"] = "assistant", []any{}
		}
		add("response.output_item.added", map[string]any{"output_index": index, "item": clone(item)})
		fields := func() map[string]any { return map[string]any{"output_index": index, "item_id": itemID} }
		switch o.Type {
		case "function_call":
			args := string(o.Arguments)
			// Split on rune boundaries: JSON arguments can contain Cyrillic text.
			runes := []rune(args)
			middle := len(runes) / 2
			for _, chunk := range []string{string(runes[:middle]), string(runes[middle:])} {
				f := fields()
				f["delta"] = chunk
				add("response.function_call_arguments.delta", f)
			}
			f := fields()
			f["arguments"], f["name"] = args, o.Name
			add("response.function_call_arguments.done", f)
			item["arguments"] = args
		case "web_search_call":
			add("response.web_search_call.searching", fields())
			item["action"] = map[string]any{"type": "search", "query": "synthetic public finance query"}
		case "message":
			f := fields()
			f["content_index"], f["part"] = 0, map[string]any{"type": "output_text", "text": ""}
			add("response.content_part.added", f)
			var text strings.Builder
			for _, chunk := range o.Text {
				s, e := render(chunk, data)
				if e != nil {
					return nil, e
				}
				text.WriteString(s)
				f = fields()
				f["content_index"], f["delta"] = 0, s
				add("response.output_text.delta", f)
			}
			part := map[string]any{"type": "output_text", "text": text.String(), "annotations": o.Annotations}
			f = fields()
			f["content_index"], f["text"] = 0, text.String()
			add("response.output_text.done", f)
			f = fields()
			f["content_index"], f["part"] = 0, part
			add("response.content_part.done", f)
			item["content"] = []any{part}
		}
		item["status"] = "completed"
		add("response.output_item.done", map[string]any{"output_index": index, "item": clone(item)})
		output = append(output, item)
	}
	usage := step.Usage
	if searches == 0 {
		if usage.TotalTokens >= usage.ToolTokens {
			usage.TotalTokens -= usage.ToolTokens
		}
		usage.ToolTokens = 0
	}
	add("response.completed", map[string]any{"response": map[string]any{"id": id, "status": "completed", "output": output, "usage": map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens, "input_tokens_details": map[string]any{"tool_tokens": usage.ToolTokens}}}})
	return encodeEvents(events)
}

func clone(m map[string]any) map[string]any {
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func encodeEvents(events []map[string]any) ([][]byte, error) {
	frames := make([][]byte, 0, len(events))
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		frames = append(frames, append(append([]byte("data: "), raw...), []byte("\n\n")...))
	}
	return frames, nil
}
