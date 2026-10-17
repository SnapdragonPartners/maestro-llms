package llms

// ModelRef identifies a provider-specific model. Name is passed through to the
// provider unchanged; unknown or new model names must not be rejected by the
// core package (an optional model registry is advisory only).
type ModelRef struct {
	Provider ProviderID
	Name     string
}

// ProviderID names a provider adapter: the wire protocol and API family a
// client speaks, not a vendor or deployment (Anthropic on Vertex is still
// ProviderAnthropic, Gemini on Vertex is still ProviderGoogle). It is an open
// set: the constants cover the adapters in this module, and an application
// implementing ChatClient for another provider defines its own value. The
// core compares ProviderIDs only for equality (see ReasoningFrom).
type ProviderID string

// ProviderIDs of the adapters in this module.
const (
	ProviderAnthropic ProviderID = "anthropic"
	ProviderOpenAI    ProviderID = "openai"
	ProviderGoogle    ProviderID = "google"
	ProviderOllama    ProviderID = "ollama"
	ProviderVLLM      ProviderID = "vllm"
)

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
