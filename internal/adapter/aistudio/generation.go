package aistudio

import (
	"context"
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

// Stream reserves the exact HTTP body and preserves complete raw output items.
func (m *GenerationModel) Stream(ctx context.Context, request generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
	if emit == nil {
		return generation.ModelResult{}, errors.New("model callback is required")
	}
	result, err := m.client.StreamGuarded(ctx, Request{Instructions: request.Instructions, Input: request.Input, Tools: request.Tools, MaxOutputTokens: request.MaxOutputTokens, MaxToolCalls: request.MaxToolCalls}, func(delta Delta) error {
		return emit(generation.ModelDelta{OutputIndex: delta.OutputIndex, ContentIndex: delta.ContentIndex, Text: delta.Text})
	}, reserve)
	if errors.Is(err, ErrRefused) {
		return generation.ModelResult{}, generation.ErrRefused
	}
	if err != nil {
		return generation.ModelResult{}, err
	}
	output := make([]generation.ModelItem, 0, len(result.Output))
	for _, item := range result.Output {
		output = append(output, generation.ModelItem{ID: item.ID, Type: item.Type, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments, Raw: item.Raw})
	}
	return generation.ModelResult{Output: output}, nil
}
