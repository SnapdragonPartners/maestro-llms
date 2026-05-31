package vllm

import (
	"context"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ListModels returns the models the vLLM server is currently serving, via
// /v1/models. Implements llms.ModelLister.
//
// IMPORTANT — semantics differ from hosted providers (see ADR-0015):
//   - This is the list of models LOADED on this vLLM instance, not a
//     catalog of available checkpoints. An operator typically launches
//     vLLM with one model.
//   - ModelInfo.Created carries the model LOAD time on this vLLM
//     instance (the `created` field vLLM returns is the server's
//     unix-seconds at model registration), NOT the upstream HuggingFace
//     release date. Same caveat as Ollama's modified_at.
//   - ModelInfo.Family is intentionally left empty: vLLM serves arbitrary
//     HuggingFace-format models (e.g. `mistralai/Ministral-3-14B-
//     Instruct-2512`, `Qwen/Qwen2.5-72B-Instruct`) with no canonical
//     family convention, so "latest in family" has no consistent meaning.
//     *Client therefore does not implement LatestInFamily.
func (c *Client) ListModels(ctx context.Context) ([]llms.ModelInfo, error) {
	page, err := c.api.Models.List(ctx)
	if err != nil {
		return nil, classifyError(c.model, err)
	}
	out := make([]llms.ModelInfo, 0, len(page.Data))
	for i := range page.Data {
		m := page.Data[i]
		var created time.Time
		if m.Created > 0 {
			created = time.Unix(m.Created, 0).UTC()
		}
		out = append(out, llms.ModelInfo{
			ID: m.ID,
			// Family intentionally empty — see godoc above.
			Created: created,
			Raw:     m,
		})
	}
	return out, nil
}
