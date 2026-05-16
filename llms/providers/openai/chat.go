package openai

import (
	"context"

	"github.com/openai/openai-go"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ChatClient is an llms.ChatClient backed by the OpenAI **Responses API**
// (not Chat Completions, which OpenAI is deprecating). The conversation is
// sent as structured Responses input items, so tool calls round-trip
// faithfully rather than being flattened into a text blob.
//
//nolint:govet // fieldalignment: size dominated by embedded third-party SDK client
type ChatClient struct {
	api   openai.Client
	model string
}

// compile-time assertion lives in the test file.

// NewChat builds a Responses-API chat client. It returns a
// *llms.ProviderError of kind config when required values are missing.
func NewChat(opts ...Option) (*ChatClient, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if err := s.validate(false); err != nil {
		return nil, err
	}
	return &ChatClient{api: openai.NewClient(s.requestOptions()...), model: s.model}, nil
}

// Model returns the model this client targets.
func (c *ChatClient) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// Complete sends a synchronous chat completion via the Responses API.
func (c *ChatClient) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	params, err := c.toParams(req)
	if err != nil {
		return llms.ChatResponse{}, err
	}
	resp, apiErr := c.api.Responses.New(ctx, params)
	if apiErr != nil {
		return llms.ChatResponse{}, classifyError(c.model, apiErr)
	}
	return toChatResponse(resp), nil
}
