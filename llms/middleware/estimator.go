package middleware

import (
	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
)

// TokenEstimator estimates the rate-limit-relevant size of a request before it
// is sent. Estimates should overestimate: for a limiter, overestimation is the
// safe error.
type TokenEstimator interface {
	EstimateChat(req llms.ChatRequest) ratelimit.UsageUnits
	EstimateEmbeddings(req llms.EmbeddingRequest) ratelimit.UsageUnits
}

// estimator tuning. These are deliberately conservative (bias high).
const (
	// charsPerToken is intentionally low (~3) so character-count-based
	// estimates run higher than the typical ~4 chars/token, overestimating.
	charsPerToken = 3
	// perPartOverhead pads each content part for role/formatting tokens.
	perPartOverhead = 8
	// defaultOutputMax is assumed output budget when a request sets no
	// MaxTokens, so the reservation still accounts for generated tokens.
	defaultOutputMax = 1024
)

// DefaultEstimator is an approximate, provider-neutral TokenEstimator based on
// character counts. It does not tokenize; it trades accuracy for zero
// dependencies and a safe upward bias. Provider-specific estimators may be
// added later.
type DefaultEstimator struct{}

func tokensForText(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + charsPerToken - 1) / charsPerToken
}

func tokensForParts(parts []llms.ContentPart) int {
	total := 0
	for i := range parts {
		p := parts[i]
		total += perPartOverhead + tokensForText(p.Text)
		if p.ToolCall != nil {
			total += tokensForText(p.ToolCall.Name) + tokensForText(string(p.ToolCall.Parameters))
		}
		if p.ToolResult != nil {
			total += tokensForText(p.ToolResult.Content)
		}
	}
	return total
}

// EstimateChat estimates input tokens from system + messages + tool schemas,
// and output tokens from MaxTokens (or a default), biased high.
func (DefaultEstimator) EstimateChat(req llms.ChatRequest) ratelimit.UsageUnits {
	input := tokensForParts(req.System)
	for i := range req.Messages {
		input += tokensForParts(req.Messages[i].Content)
	}
	for i := range req.Tools {
		input += tokensForText(req.Tools[i].Name) +
			tokensForText(req.Tools[i].Description) +
			tokensForText(string(req.Tools[i].InputSchema))
	}
	outMax := req.MaxTokens
	if outMax <= 0 {
		outMax = defaultOutputMax
	}
	return ratelimit.UsageUnits{
		InputTokens:     input,
		OutputTokensMax: outMax,
		TotalTokensMax:  input + outMax,
		Requests:        1,
	}
}

// EstimateEmbeddings estimates input tokens from the concatenated inputs.
// Embeddings produce no output tokens.
func (DefaultEstimator) EstimateEmbeddings(req llms.EmbeddingRequest) ratelimit.UsageUnits {
	input := 0
	for i := range req.Inputs {
		input += tokensForText(req.Inputs[i].Text)
	}
	return ratelimit.UsageUnits{
		InputTokens:    input,
		TotalTokensMax: input,
		Requests:       1,
	}
}
