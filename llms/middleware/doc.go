// Package middleware provides provider-neutral chat and embedding middleware
// (timeout, retry, circuit breaker, rate limiting, metrics, validation) and
// the ChainChat and ChainEmbeddings composition helpers. The first middleware
// argument to a chain is outermost.
package middleware
