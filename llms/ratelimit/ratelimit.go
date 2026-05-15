package ratelimit

import (
	"context"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Operation is the transport shape of a call, used by limiters and middleware.
// It answers "what provider API class is this?" — distinct from llms.Purpose,
// which is application intent.
type Operation string

const (
	// OperationChat is a chat/completion call.
	OperationChat Operation = "chat"
	// OperationEmbedding is an embedding call.
	OperationEmbedding Operation = "embedding"
)

// UsageUnits is the rate-limit-relevant size of a request. Estimators
// overestimate rather than underestimate, since overestimation is the safer
// error for a limiter.
type UsageUnits struct {
	InputTokens     int
	OutputTokensMax int
	TotalTokensMax  int
	Requests        int
}

// tokens returns the token count a reservation should hold for these units:
// TotalTokensMax when set, otherwise InputTokens + OutputTokensMax.
func (u UsageUnits) tokens() int {
	if u.TotalTokensMax > 0 {
		return u.TotalTokensMax
	}
	return u.InputTokens + u.OutputTokensMax
}

// Subject identifies who a reservation is on behalf of. All fields are
// optional; distributed limiters use them for per-tenant/user accounting.
type Subject struct {
	TenantID string
	UserID   string
	JobID    string
}

// ReservationRequest is presented to a Limiter before a provider call.
//
// Fields are grouped pointer-bearing first (fieldalignment); construct with
// keyed literals.
type ReservationRequest struct {
	Provider       string
	Model          string
	Operation      Operation
	Purpose        llms.Purpose
	Metadata       map[string]string
	Subject        Subject
	EstimatedUnits UsageUnits
}

// Limiter gates provider calls. Implementations must not assume process-local
// state is authoritative: the same middleware runs against an in-memory
// limiter (desktop) and a shared/distributed one (multi-instance cloud).
type Limiter interface {
	// Reserve is called before the provider request. It returns a Reservation
	// or, when the request cannot be admitted, a *llms.LimitError (distinct
	// from a provider 429 so retry/circuit middleware can tell them apart).
	Reserve(ctx context.Context, req ReservationRequest) (Reservation, error)
}

// Reservation is the handle returned by Reserve.
//
// Lifecycle: middleware should always call Release, and call Commit when
// actual usage is known. Implementations must make repeated Release safe and
// Release safe after Commit. Release must be callable with a context that has
// already been canceled (the request context), so callers pass a
// cancellation-surviving context such as context.WithoutCancel(ctx).
type Reservation interface {
	// Commit reconciles actual usage against the estimate. If actual is below
	// the estimate, implementations may refund the difference; if above, they
	// charge the delta. Commit does not release the concurrency lease.
	Commit(ctx context.Context, usage llms.Usage) error
	// Release frees the concurrency/request lease. It is idempotent and safe
	// after Commit.
	Release(ctx context.Context) error
}

// LimiterStats is an optional capability a Limiter may also implement.
// Consumers discover it with a type assertion; the core Limiter interface
// stays minimal so distributed implementations need not support stats.
type LimiterStats interface {
	Stats(ctx context.Context) (LimiterSnapshot, error)
}

// LimiterSnapshot is a point-in-time view of an in-memory limiter, for
// debugging and metrics. Distributed limiters may populate a subset.
type LimiterSnapshot struct {
	AvailableTokens int
	MaxCapacity     int
	ActiveRequests  int
	MaxConcurrency  int
	TokenWaitHits   int64
	SlotWaitHits    int64
}
