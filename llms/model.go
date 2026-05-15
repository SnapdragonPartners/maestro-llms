package llms

// ModelRef identifies a provider-specific model. Name is passed through to the
// provider unchanged; unknown or new model names must not be rejected by the
// core package (an optional model registry is advisory only).
type ModelRef struct {
	Provider string
	Name     string
}

// Purpose is an application-supplied label describing why a call is happening.
// The core package does not interpret it; middleware and logging may. Callers
// may pass custom values beyond the constants below.
type Purpose string

const (
	// PurposeChat labels a general chat/completion call.
	PurposeChat Purpose = "chat"
	// PurposeEmbedding labels an embedding call.
	PurposeEmbedding Purpose = "embedding"
	// PurposeClassification labels a classification call.
	PurposeClassification Purpose = "classification"
	// PurposeSummarization labels a summarization call.
	PurposeSummarization Purpose = "summarization"
)
