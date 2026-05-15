package testllm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// defaultFakeDimensions is used when a FakeEmbeddingClient has no Dims set.
const defaultFakeDimensions = 8

// FakeEmbeddingClient is a deterministic, concurrency-safe llms.EmbeddingClient
// for tests. A vector is chosen for each input by, in order: Func (if set),
// an exact match in Vectors keyed by input text, otherwise a deterministic
// hash of the text. Hash-derived vectors are stable across processes and
// platforms (no map iteration, no unseeded randomness).
// Fields are grouped pointer-bearing first (fieldalignment); construct with
// keyed literals.
type FakeEmbeddingClient struct {
	// Func, if set, fully overrides the behavior below.
	Func func(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error)
	// Err, if set (and Func is nil), is returned from every Embed call.
	Err error
	// Vectors maps input text to a fixed vector; takes precedence over the
	// hash fallback for matching inputs.
	Vectors map[string][]float32
	// ModelRef is reported by Model.
	ModelRef llms.ModelRef
	// Usage is returned on every successful Embed call.
	Usage llms.Usage

	calls []llms.EmbeddingRequest
	// Dims is the hash-fallback vector length; defaults to 8. A non-zero
	// EmbeddingRequest.Dimensions overrides it per call.
	Dims int
	mu   sync.Mutex
}

// Model returns the configured model reference.
func (f *FakeEmbeddingClient) Model() llms.ModelRef { return f.ModelRef }

// DefaultDimensions returns the configured hash-fallback dimension.
func (f *FakeEmbeddingClient) DefaultDimensions() int {
	if f.Dims > 0 {
		return f.Dims
	}
	return defaultFakeDimensions
}

// Embed records the request and returns one vector per input, in input order.
func (f *FakeEmbeddingClient) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	// Record before the context check so a canceled call is still observable.
	f.mu.Lock()
	f.calls = append(f.calls, copyEmbeddingRequest(req))
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return llms.EmbeddingResponse{}, fmt.Errorf("testllm: context done: %w", err)
	}

	if f.Func != nil {
		return f.Func(ctx, req)
	}
	if f.Err != nil {
		return llms.EmbeddingResponse{}, f.Err
	}

	dims := f.DefaultDimensions()
	if req.Dimensions > 0 {
		dims = req.Dimensions
	}

	vectors := make([]llms.EmbeddingVector, len(req.Inputs))
	for i := range req.Inputs {
		in := req.Inputs[i]
		values, ok := f.Vectors[in.Text]
		if !ok {
			values = hashVector(in.Text, dims)
		}
		vectors[i] = llms.EmbeddingVector{ID: in.ID, Values: values}
	}
	return llms.EmbeddingResponse{Vectors: vectors, Usage: f.Usage}, nil
}

// Calls returns a copy of the recorded requests, in order.
func (f *FakeEmbeddingClient) Calls() []llms.EmbeddingRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]llms.EmbeddingRequest, len(f.calls))
	for i := range f.calls {
		out[i] = copyEmbeddingRequest(f.calls[i])
	}
	return out
}

// Reset clears recorded calls.
func (f *FakeEmbeddingClient) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// hashVector deterministically derives a length-dims float32 vector from text.
// Each component is a little-endian uint32 of a per-index SHA-256 digest
// divided by 2^32, giving a value in [0, 1) (strictly < 1), so the same text
// always yields the same vector everywhere.
func hashVector(text string, dims int) []float32 {
	const twoPow32 = float64(1 << 32)
	out := make([]float32, dims)
	for i := range out {
		sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s", i, text))
		u := binary.LittleEndian.Uint32(sum[:4])
		out[i] = float32(float64(u) / twoPow32)
	}
	return out
}
