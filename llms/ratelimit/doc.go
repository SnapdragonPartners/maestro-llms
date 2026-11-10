// Package ratelimit defines the Limiter and Reservation interfaces and the
// reservation request and usage-unit types. It ships two implementations:
// InMemoryLimiter for a single process, and FromBucket, which adapts any
// bucket.Limiter (bucket/memory in process, or bucket/postgres shared across
// instances) to Limiter (ADR-0025). All composes limiters into one that admits
// a request only when every limiter does, rolling back on a refusal
// (ADR-0042). Applications may still implement Limiter themselves.
package ratelimit
