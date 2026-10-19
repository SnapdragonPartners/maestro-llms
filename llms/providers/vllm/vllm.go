// Package vllm is the former name of the openaicompat package, kept as a
// permanent forwarding alias so existing imports keep compiling (ADR-0027).
// Every identifier here is the openaicompat one: Client and Option are type
// aliases, and the functions forward their arguments unchanged.
//
// Deprecated: import
// github.com/SnapdragonPartners/maestro-llms/llms/providers/openaicompat
// instead. The client reports llms.ProviderOpenAICompat ("openaicompat").
package vllm

import (
	"net/http"

	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openaicompat"
)

// Client is openaicompat.Client.
//
// Deprecated: use openaicompat.Client.
type Client = openaicompat.Client

// Option is openaicompat.Option.
//
// Deprecated: use openaicompat.Option.
type Option = openaicompat.Option

// New forwards to openaicompat.New.
//
// Deprecated: use openaicompat.New.
func New(opts ...Option) (*Client, error) {
	return openaicompat.New(opts...) //nolint:wrapcheck // a pure alias: openaicompat's typed *llms.ProviderError, unchanged
}

// WithBaseURL forwards to openaicompat.WithBaseURL.
//
// Deprecated: use openaicompat.WithBaseURL.
func WithBaseURL(u string) Option { return openaicompat.WithBaseURL(u) }

// WithModel forwards to openaicompat.WithModel.
//
// Deprecated: use openaicompat.WithModel.
func WithModel(model string) Option { return openaicompat.WithModel(model) }

// WithAPIKey forwards to openaicompat.WithAPIKey.
//
// Deprecated: use openaicompat.WithAPIKey.
func WithAPIKey(key string) Option { return openaicompat.WithAPIKey(key) }

// WithHTTPClient forwards to openaicompat.WithHTTPClient.
//
// Deprecated: use openaicompat.WithHTTPClient.
func WithHTTPClient(c *http.Client) Option { return openaicompat.WithHTTPClient(c) }

// WithMaxRetries forwards to openaicompat.WithMaxRetries.
//
// Deprecated: use openaicompat.WithMaxRetries.
func WithMaxRetries(n int) Option { return openaicompat.WithMaxRetries(n) }
