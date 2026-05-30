package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// tagsResponse is the wire shape of GET /api/tags. We hold onto the SDK-
// free struct definition here so the Ollama "no SDK dependency" stance
// (see package doc + ADR-0002) extends to listing.
type tagsResponse struct {
	Models []tagsModel `json:"models"`
}

type tagsModel struct {
	Name       string `json:"name"`
	ModifiedAt string `json:"modified_at"`
	Digest     string `json:"digest"`
	Details    struct {
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
	Size int64 `json:"size"`
}

// ListModels returns the models locally pulled into the Ollama runtime,
// via GET /api/tags. Implements llms.ModelLister.
//
// IMPORTANT — semantics differ from hosted providers (see ADR-0012):
//   - This is the LOCAL pull list, not the provider catalog. A model
//     appears here only after `ollama pull`.
//   - ModelInfo.Created carries the LOCAL ModifiedAt timestamp (when the
//     file was pulled or re-pulled), NOT the upstream release date.
//   - ModelInfo.Family is intentionally left empty: Ollama models are
//     community-uploaded under arbitrary tags (`llama3.2:1b`,
//     `mistral:instruct`, etc.) with no canonical family convention, so
//     "latest in family" has no consistent meaning. *Client does not
//     implement a LatestInFamily helper for that reason.
func (c *Client) ListModels(ctx context.Context) ([]llms.ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", http.NoBody)
	if err != nil {
		return nil, c.classify(err)
	}
	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, c.classify(err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, c.classify(err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, c.classify(&httpStatusErr{status: httpResp.StatusCode, header: httpResp.Header, msg: msg})
	}

	var tr tagsResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, c.classify(&httpStatusErr{
			status: httpResp.StatusCode, header: httpResp.Header,
			msg: "invalid /api/tags JSON: " + err.Error(),
		})
	}
	out := make([]llms.ModelInfo, 0, len(tr.Models))
	for i := range tr.Models {
		m := tr.Models[i]
		var created time.Time
		if m.ModifiedAt != "" {
			// modified_at is RFC 3339 with sub-second precision.
			if t, err := time.Parse(time.RFC3339Nano, m.ModifiedAt); err == nil {
				created = t
			}
		}
		out = append(out, llms.ModelInfo{
			ID: m.Name,
			// Family intentionally empty — see godoc above.
			Created: created,
			Raw:     m,
		})
	}
	return out, nil
}
