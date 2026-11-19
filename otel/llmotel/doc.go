// Package llmotel exports maestro-llms calls to OpenTelemetry as GenAI
// spans and metrics (ADR-0051).
//
// It follows the GenAI semantic conventions as released in semconv v1.41.0.
// Those conventions are unstable, and the module is v0 so it can follow them:
// when the OpenTelemetry GenAI conventions repository cuts a release, a minor
// version of this module moves to it and its release notes list every renamed
// instrument and attribute.
//
// The package uses only the OpenTelemetry API. The application owns the SDK,
// exporters, resource, sampling and endpoint configuration, and passes its
// providers in Options (a nil provider falls back to the global one, which
// the package reads and never sets).
//
// # Wiring
//
//	t, err := llmotel.New(llmotel.Options{TracerProvider: tp, MeterProvider: mp})
//	chat := t.ChatCall()(middleware.RecommendedChat(t.Chat()(base), cfg))
//	out := t.Run(ctx, toolloop.Config{Client: chat, Tools: t.Tools(tools), ...}, llmotel.RunOptions{})
//
// Chat and Embeddings are the attempt layer: one CLIENT span per provider
// attempt, plus all metrics. Put them innermost, as above. ChatCall and
// EmbeddingsCall are the logical-call layer: one INTERNAL span per call
// through the whole chain, with no metrics and no usage attributes, so the
// attempt spans nest under it and the gaps between them are backoff and
// limiter waits.
//
// Metrics come from exactly one layer. Do not wrap a client with Chat both
// outside and inside a chain: every token sum and operation count would be
// recorded twice. An application that wants metrics per logical call rather
// than per attempt uses Chat outside the chain instead of inside it.
//
// # Content and errors
//
// Nothing the model reads or writes is exported: no prompts, completions,
// system prompts, tool definitions, arguments or results. Error text is never
// exported either (provider errors can echo request content). A failure is
// described only by error.type: the llms.ErrorKind of a *llms.ProviderError,
// "rate_limited" for a *llms.LimitError, and "_OTHER" for anything else.
package llmotel
