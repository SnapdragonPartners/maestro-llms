package openai

import (
	"context"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/internal/extraparams"
)

// Client is an llms.ChatClient backed by the OpenAI **Responses API**
// (not Chat Completions, which OpenAI is deprecating). The conversation is
// sent as structured Responses input items, so tool calls round-trip
// faithfully rather than being flattened into a text blob.
//
//nolint:govet // fieldalignment: size dominated by embedded third-party SDK client
type Client struct {
	api   openai.Client
	model string
}

// compile-time assertion lives in the test file.

// New builds a Responses-API chat Client. It returns a *llms.ProviderError
// of kind config when required values are missing or invalid (e.g. a
// negative max-retry count) or an embeddings-only option such as
// WithDimensions is set (ADR-0035).
func New(opts ...Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if len(s.embedOnly) > 0 {
		return nil, configErr(strings.Join(s.embedOnly, ", ") + " applies only to NewEmbeddings")
	}
	if err := s.validate(false); err != nil {
		return nil, err
	}
	return &Client{api: newAPI(s.requestOptions()), model: s.model}, nil
}

// Model returns the model this client targets.
func (c *Client) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// Complete sends a synchronous chat completion via the Responses API.
func (c *Client) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	extra, _, err := extraparams.For(&req, providerName)
	if err != nil {
		return llms.ChatResponse{}, chatBadRequest(c.model, err.Error())
	}
	params, perr := c.toParams(req)
	if perr != nil {
		return llms.ChatResponse{}, perr
	}
	var opts []option.RequestOption
	if extra != nil {
		opts = append(opts, option.WithMiddleware(extraparams.Middleware(extra))) //nolint:bodyclose // the SDK owns and closes the response
	}
	resp, apiErr := c.api.Responses.New(ctx, params, opts...)
	if apiErr != nil {
		return llms.ChatResponse{}, classifyError(c.model, apiErr)
	}
	out := toChatResponse(resp)
	_, applied := mapEffort(req.Reasoning)
	if extraparams.Overrides(extra, "reasoning", "effort") {
		applied = nil // ProviderParams replaced the mapped effort (ADR-0024)
	}
	out.AppliedReasoning = applied
	return out, nil
}
