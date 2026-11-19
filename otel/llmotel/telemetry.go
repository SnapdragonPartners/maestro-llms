package llmotel

import (
	"errors"
	"fmt"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ScopeName is the instrumentation scope of every tracer and meter the
// package creates.
const ScopeName = "github.com/SnapdragonPartners/maestro-llms/otel/llmotel"

// Attributes the package defines where the GenAI conventions have nothing
// (ADR-0051 §2). They go on spans only, never on metrics.
const (
	// AttrAttempt is the retry attempt number (middleware.AttemptFromContext)
	// on an attempt-layer span, when a retry layer sits outside it.
	AttrAttempt = attribute.Key("maestro_llms.attempt")
	// AttrPurpose is the request's llms.Purpose, when set.
	AttrPurpose = attribute.Key("maestro_llms.purpose")
	// AttrToolloopOutcome is the toolloop.OutcomeKind on a Run span.
	AttrToolloopOutcome = attribute.Key("maestro_llms.toolloop.outcome")
	// AttrToolloopIterations is a Run's Outcome.Iterations: the number of
	// provider responses that contained tool calls.
	AttrToolloopIterations = attribute.Key("maestro_llms.toolloop.iterations")
)

// Error types the package sets besides llms.ErrorKind values (ADR-0051 §5).
const (
	// ErrorTypeOther is error.type for an error that carries no
	// llms.ErrorKind (the conventions' fallback value).
	ErrorTypeOther = "_OTHER"
	// ErrorTypeRateLimited is error.type for a *llms.LimitError.
	ErrorTypeRateLimited = string(llms.ErrorKindRateLimited)
	// ErrorTypeToolError is error.type on a tool span whose result has
	// IsError set.
	ErrorTypeToolError = "tool_error"
)

// durationBuckets and tokenBuckets are the advisory bucket boundaries from
// the semconv v1.41.0 GenAI metric definitions.
func durationBuckets() []float64 {
	return []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
}

func tokenBuckets() []float64 {
	return []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
}

// Options configures New. Every field is optional.
type Options struct {
	// TracerProvider creates the package's tracer. nil: the global provider
	// (otel.GetTracerProvider), read at New and never set.
	TracerProvider trace.TracerProvider
	// MeterProvider creates the package's meter. nil: the global provider
	// (otel.GetMeterProvider), read at New and never set.
	MeterProvider metric.MeterProvider
	// ProviderName returns gen_ai.provider.name for a model. nil: the default
	// mapping, which cannot see the deployment: openai → "openai", anthropic
	// → "anthropic", google → "gcp.gen_ai" (backend unknown), and any other
	// ProviderID as-is. An application that knows more overrides it, for
	// example returning "gcp.vertex_ai" for Gemini on Vertex.
	ProviderName func(llms.ModelRef) string
}

// Telemetry holds the tracer, meter instruments and options. Create one with
// New and share it: it is safe for concurrent use.
type Telemetry struct {
	tracer       trace.Tracer
	providerName func(llms.ModelRef) string
	duration     metric.Float64Histogram
	tokens       metric.Int64Histogram
}

// New creates the tracer and the metric instruments once. It returns an
// error only if the meter refuses an instrument.
func New(opts Options) (*Telemetry, error) {
	tp := opts.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	mp := opts.MeterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName, metric.WithSchemaURL(semconv.SchemaURL))
	duration, err := genaiconv.NewClientOperationDuration(meter,
		metric.WithExplicitBucketBoundaries(durationBuckets()...))
	if err != nil {
		return nil, fmt.Errorf("llmotel: creating %s: %w", duration.Name(), err)
	}
	tokens, err := genaiconv.NewClientTokenUsage(meter,
		metric.WithExplicitBucketBoundaries(tokenBuckets()...))
	if err != nil {
		return nil, fmt.Errorf("llmotel: creating %s: %w", tokens.Name(), err)
	}
	pn := opts.ProviderName
	if pn == nil {
		pn = DefaultProviderName
	}
	return &Telemetry{
		tracer:       tp.Tracer(ScopeName, trace.WithSchemaURL(semconv.SchemaURL)),
		providerName: pn,
		duration:     duration.Inst(),
		tokens:       tokens.Inst(),
	}, nil
}

// DefaultProviderName is the gen_ai.provider.name mapping used when
// Options.ProviderName is nil (ADR-0051 §4).
func DefaultProviderName(ref llms.ModelRef) string {
	switch ref.Provider {
	case llms.ProviderOpenAI:
		return "openai"
	case llms.ProviderAnthropic:
		return "anthropic"
	case llms.ProviderGoogle:
		return "gcp.gen_ai"
	default:
		return string(ref.Provider)
	}
}

// errorType maps an error to error.type (ADR-0051 §5). It never looks at the
// error's text.
func errorType(err error) string {
	var pe *llms.ProviderError
	if errors.As(err, &pe) && pe.Kind != "" {
		return string(pe.Kind)
	}
	var le *llms.LimitError
	if errors.As(err, &le) {
		return ErrorTypeRateLimited
	}
	return ErrorTypeOther
}

// commonAttrs are the low-cardinality attributes shared by spans and metrics:
// operation, provider and request model.
func (t *Telemetry) commonAttrs(op attribute.KeyValue, ref llms.ModelRef) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 3)
	attrs = append(attrs, op)
	if name := t.providerName(ref); name != "" {
		attrs = append(attrs, semconv.GenAIProviderNameKey.String(name))
	}
	if ref.Name != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(ref.Name))
	}
	return attrs
}

// spanName is "<operation> <model>", or the operation alone when the model is
// unknown.
func spanName(op, model string) string {
	if model == "" {
		return op
	}
	return op + " " + model
}

// positive appends key=v when v > 0. A zero llms.Usage field may mean "not
// reported", so it is never emitted (ADR-0051 §4).
func positive(attrs []attribute.KeyValue, key attribute.Key, v int) []attribute.KeyValue {
	if v > 0 {
		return append(attrs, key.Int(v))
	}
	return attrs
}

// float32Attr converts through the shortest decimal form, so 0.7 is exported
// as 0.7 rather than 0.699999988079071.
func float32Attr(key attribute.Key, v float32) attribute.KeyValue {
	f, err := strconv.ParseFloat(strconv.FormatFloat(float64(v), 'g', -1, 32), 64)
	if err != nil {
		f = float64(v)
	}
	return key.Float64(f)
}
