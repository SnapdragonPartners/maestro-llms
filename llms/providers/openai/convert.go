package openai

import (
	"github.com/openai/openai-go"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// toResponse maps the SDK response to the app-neutral EmbeddingResponse.
// Results are placed by the provider-reported Index so output order matches
// input order regardless of how the API returns them, and each vector carries
// its input ID for defensive matching.
func (c *Client) toResponse(req llms.EmbeddingRequest, resp *openai.CreateEmbeddingResponse) (llms.EmbeddingResponse, error) {
	if len(resp.Data) != len(req.Inputs) {
		return llms.EmbeddingResponse{}, &llms.ProviderError{
			Provider: providerName, Model: c.model, Kind: llms.ErrorKindUnknown,
			Message: "provider returned a different number of vectors than inputs",
		}
	}

	vectors := make([]llms.EmbeddingVector, len(req.Inputs))
	filled := make([]bool, len(req.Inputs))
	for i := range resp.Data {
		e := resp.Data[i]
		idx := int(e.Index)
		if idx < 0 || idx >= len(req.Inputs) {
			return llms.EmbeddingResponse{}, &llms.ProviderError{
				Provider: providerName, Model: c.model, Kind: llms.ErrorKindUnknown,
				Message: "provider returned an out-of-range embedding index",
			}
		}
		if filled[idx] {
			return llms.EmbeddingResponse{}, &llms.ProviderError{
				Provider: providerName, Model: c.model, Kind: llms.ErrorKindUnknown,
				Message: "provider returned a duplicate embedding index",
			}
		}
		values := make([]float32, len(e.Embedding))
		for j, v := range e.Embedding {
			values[j] = float32(v)
		}
		vectors[idx] = llms.EmbeddingVector{ID: req.Inputs[idx].ID, Values: values}
		filled[idx] = true
	}
	for i := range filled {
		if !filled[i] {
			return llms.EmbeddingResponse{}, &llms.ProviderError{
				Provider: providerName, Model: c.model, Kind: llms.ErrorKindUnknown,
				Message: "provider did not return a vector for every input",
			}
		}
	}

	return llms.EmbeddingResponse{
		Vectors: vectors,
		Usage: llms.Usage{
			EmbeddingTokens: int(resp.Usage.PromptTokens),
			TotalTokens:     int(resp.Usage.TotalTokens),
		},
		Raw: resp,
	}, nil
}
