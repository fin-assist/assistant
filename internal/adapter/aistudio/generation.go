package aistudio

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/fin-assist/assistant/internal/generation"
)

// GenerationModel adapts the Responses transport to the generation's consumer port.
type GenerationModel struct{ client *Client }

// NewGenerationModel requires an already configured reusable transport.
func NewGenerationModel(client *Client) (*GenerationModel, error) {
	if client == nil {
		return nil, errors.New("model transport is required")
	}
	return &GenerationModel{client: client}, nil
}

// Stream reserves the exact HTTP body. Function correlation is canonicalized to
// the unique provider item ID before the stateless context reaches the engine.
func (m *GenerationModel) Stream(ctx context.Context, request generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
	if emit == nil {
		return generation.ModelResult{}, errors.New("model callback is required")
	}
	result, err := m.client.StreamGuarded(ctx, Request{PrivacyScope: request.PrivacyScope, Instructions: request.Instructions, Input: request.Input, Tools: request.Tools, MaxOutputTokens: request.MaxOutputTokens, MaxToolCalls: request.MaxToolCalls}, func(delta Delta) error {
		return emit(generation.ModelDelta{OutputIndex: delta.OutputIndex, ContentIndex: delta.ContentIndex, Text: delta.Text})
	}, reserve)
	if errors.Is(err, ErrRefused) {
		return generation.ModelResult{}, generation.ErrRefused
	}
	if errors.Is(err, ErrRequestTooLarge) {
		return generation.ModelResult{}, generation.ErrInputTooLarge
	}
	if err != nil {
		return generation.ModelResult{}, err
	}
	output := make([]generation.ModelItem, 0, len(result.Output))
	for _, item := range result.Output {
		mapped, err := generationItem(item)
		if err != nil {
			return generation.ModelResult{}, err
		}
		if mapped.Type == "function_call" {
			mapped.Arguments, err = restoreSearchArguments(mapped.Name, mapped.Arguments, result.redactions)
			if err != nil {
				return generation.ModelResult{}, err
			}
			if mapped.Arguments != item.Arguments {
				var fields map[string]json.RawMessage
				if decodeJSON(mapped.Raw, &fields) != nil {
					return generation.ModelResult{}, errPrivacy
				}
				fields["arguments"], _ = json.Marshal(mapped.Arguments)
				mapped.Raw, _ = json.Marshal(fields)
			}
		}
		output = append(output, mapped)
	}
	return generation.ModelResult{Output: output, Usage: generation.Usage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens, TotalTokens: result.Usage.TotalTokens, ToolTokens: result.Usage.ToolTokens}}, nil
}

// AI Studio can use the function name as call_id even for repeated calls. Its
// item ID remains unique. Rewrite both the consumer key and the raw input item:
// the engine's function_call_output and the next manual-context POST then agree.
func generationItem(item Item) (generation.ModelItem, error) {
	mapped := generation.ModelItem{ID: item.ID, Type: item.Type, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments, Raw: item.Raw}
	if item.Type != "function_call" {
		return mapped, nil
	}
	var fields map[string]json.RawMessage
	if item.ID == "" || decodeProviderJSON(item.Raw, &fields) != nil || fields == nil {
		return generation.ModelItem{}, errors.New("invalid model function correlation")
	}
	key, err := json.Marshal(item.ID)
	if err != nil {
		return generation.ModelItem{}, errors.New("invalid model function correlation")
	}
	fields["call_id"] = key
	raw, err := json.Marshal(fields)
	if err != nil {
		return generation.ModelItem{}, errors.New("invalid model function correlation")
	}
	mapped.CallID, mapped.Raw = item.ID, raw
	return mapped, nil
}
