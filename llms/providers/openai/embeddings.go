package openai

import (
	"context"

	"github.com/openai/openai-go/v3"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// maxBatchInputs caps inputs per request. OpenAI's embeddings endpoint accepts
// at most 2048 inputs; exceeding it is a caller error. Chunking is the
// application's responsibility (it owns retry policy, progress, and source
// IDs), so this is a hard validation error rather than auto-splitting.
const maxBatchInputs = 2048

// EmbeddingClient is an llms.EmbeddingClient backed by the official OpenAI
// SDK's embeddings endpoint.
//
//nolint:govet // fieldalignment: size dominated by embedded third-party SDK client
type EmbeddingClient struct {
	api        openai.Client
	model      string
	dimensions int
}

// compile-time assertion lives in the test file (a package-level var would
// trip gochecknoglobals).

// NewEmbeddings builds an EmbeddingClient. It returns a *llms.ProviderError
// of kind config when required values are missing or invalid.
func NewEmbeddings(opts ...Option) (*EmbeddingClient, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if err := s.validate(true); err != nil {
		return nil, err
	}
	return &EmbeddingClient{
		api:        newAPI(s.requestOptions()),
		model:      s.model,
		dimensions: s.dimensions,
	}, nil
}

// Model returns the model this client targets.
func (c *EmbeddingClient) Model() llms.ModelRef {
	return llms.ModelRef{Provider: providerName, Name: c.model}
}

// DefaultDimensions returns the advisory configured dimension (0 if unset).
// The authoritative dimension is the length of each returned vector.
func (c *EmbeddingClient) DefaultDimensions() int { return c.dimensions }

// Embed returns one vector per input, in input order, tagged with input IDs.
func (c *EmbeddingClient) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	if len(req.Inputs) == 0 {
		return llms.EmbeddingResponse{}, &llms.ProviderError{
			Provider: providerName, Model: c.model,
			Kind: llms.ErrorKindBadRequest, Message: "no embedding inputs",
		}
	}
	if len(req.Inputs) > maxBatchInputs {
		return llms.EmbeddingResponse{}, &llms.ProviderError{
			Provider: providerName, Model: c.model, Kind: llms.ErrorKindBadRequest,
			Message: "too many inputs for one request; the application must chunk",
		}
	}

	texts := make([]string, len(req.Inputs))
	for i := range req.Inputs {
		texts[i] = req.Inputs[i].Text
	}

	params := openai.EmbeddingNewParams{
		Model: c.model,
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
	}
	switch {
	case req.Dimensions > 0:
		params.Dimensions = openai.Int(int64(req.Dimensions))
	case c.dimensions > 0:
		params.Dimensions = openai.Int(int64(c.dimensions))
	}

	resp, err := c.api.Embeddings.New(ctx, params)
	if err != nil {
		return llms.EmbeddingResponse{}, classifyError(c.model, err)
	}
	return c.toResponse(req, resp)
}
