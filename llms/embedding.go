package llms

import "context"

// EmbeddingClient is the synchronous embedding interface.
type EmbeddingClient interface {
	// Embed returns one vector per input, preserving input order.
	Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
	// Model returns the model this client targets.
	Model() ModelRef
	// DefaultDimensions is advisory. The authoritative dimension is the
	// length of each returned vector; some models support a caller-selected
	// dimension via EmbeddingRequest.Dimensions.
	DefaultDimensions() int
}

// EmbeddingInput is one item to embed. ID lets callers defensively match
// responses to inputs.
type EmbeddingInput struct {
	ID   string
	Text string
}

// EmbeddingRequest is a provider-neutral embedding request. Provider clients
// return a typed validation/config error when Inputs exceeds the provider or
// model batch limit; automatic chunking is the application's responsibility.
type EmbeddingRequest struct {
	Inputs []EmbeddingInput
	// Purpose is an app-supplied label; not interpreted by the core package.
	Purpose Purpose
	// Dimensions overrides the model default when the provider supports it;
	// zero means use the model default.
	Dimensions int
	// Metadata is app-supplied, provider-neutral context.
	Metadata map[string]string
}

// EmbeddingVector is one result vector, tagged with its input ID.
type EmbeddingVector struct {
	ID     string
	Values []float32
}

// EmbeddingResponse holds vectors in input order.
type EmbeddingResponse struct {
	Vectors []EmbeddingVector
	Usage   Usage
	// Raw is optional provider-specific data, outside the stability contract.
	Raw any
}
