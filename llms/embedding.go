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

// EmbeddingTask is a provider-neutral hint about what the embedding is for.
// It is advisory: providers that support task-typed embeddings (e.g. Gemini)
// use it to produce better-targeted vectors; providers that do not (e.g.
// OpenAI) ignore it. The zero value (EmbeddingTaskUnspecified) requests no
// task specialization.
type EmbeddingTask string

const (
	// EmbeddingTaskUnspecified is the zero value: no task hint.
	EmbeddingTaskUnspecified EmbeddingTask = ""
	// EmbeddingTaskRetrievalDocument embeds a corpus document for retrieval.
	// EmbeddingInput.Title may be set with this task.
	EmbeddingTaskRetrievalDocument EmbeddingTask = "retrieval_document"
	// EmbeddingTaskRetrievalQuery embeds a search query for retrieval.
	EmbeddingTaskRetrievalQuery EmbeddingTask = "retrieval_query"
	// EmbeddingTaskSemanticSimilarity embeds text for similarity comparison.
	EmbeddingTaskSemanticSimilarity EmbeddingTask = "semantic_similarity"
	// EmbeddingTaskClassification embeds text to be classified.
	EmbeddingTaskClassification EmbeddingTask = "classification"
	// EmbeddingTaskClustering embeds text for clustering.
	EmbeddingTaskClustering EmbeddingTask = "clustering"
	// EmbeddingTaskQuestionAnswering embeds a question for QA retrieval.
	EmbeddingTaskQuestionAnswering EmbeddingTask = "question_answering"
	// EmbeddingTaskFactVerification embeds text for fact verification.
	EmbeddingTaskFactVerification EmbeddingTask = "fact_verification"
	// EmbeddingTaskCodeRetrievalQuery embeds a code-search query.
	EmbeddingTaskCodeRetrievalQuery EmbeddingTask = "code_retrieval_query"
)

// EmbeddingInput is one item to embed. ID lets callers defensively match
// responses to inputs.
type EmbeddingInput struct {
	ID   string
	Text string
	// Title is an optional document title, advisory and only meaningful with
	// EmbeddingTaskRetrievalDocument; providers that do not support it ignore
	// it.
	Title string
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
	// Task is an advisory, provider-neutral hint; honored where supported
	// (e.g. Gemini), ignored elsewhere (e.g. OpenAI). Zero value = none.
	Task EmbeddingTask
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
