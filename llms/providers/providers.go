// Package providers builds a chat client from a provider ID and common
// options, so configuration can choose the provider without every
// application writing the same switch over the provider packages
// (ADR-0048).
//
// Importing this package links every provider package and its SDK.
// Applications that use one provider can keep importing that package
// directly; provider-specific settings (anthropic.WithRequestOptions,
// google.WithMaxInputBytes, ...) are available only there.
//
// The provider is always explicit: it is never inferred from the model
// name, since the same model is reachable through several providers.
package providers

import (
	"errors"
	"fmt"
	"net/http"

	"cloud.google.com/go/auth"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/anthropic/anthropicvertex"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/google"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/ollama"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openai"
	"github.com/SnapdragonPartners/maestro-llms/llms/providers/openaicompat"
)

// Sentinels carried as the Cause of the *llms.ProviderError that New
// returns for its own refusals; match them with errors.Is.
var (
	// ErrUnknownProvider: the ProviderID names no adapter in this module.
	ErrUnknownProvider = errors.New("providers: unknown provider")
	// ErrUnsupportedOption: an option was set that the provider can't use.
	ErrUnsupportedOption = errors.New("providers: option not supported by this provider")
)

// Options are the settings common to the provider packages. Each field
// means the same thing for every provider, or New refuses it. A zero field
// is not passed on, so the provider package's default applies. Options
// grows only by new fields; use keyed literals.
type Options struct {
	// HTTPClient is the HTTP client for every provider (a custom transport,
	// or Private Service Connect on Vertex). With Vertex credentials, the
	// provider authenticates a copy and never modifies it (ADR-0029).
	HTTPClient *http.Client
	// Vertex selects Vertex AI for ProviderAnthropic (anthropicvertex) and
	// ProviderGoogle. Nil means the provider's direct API. Other providers
	// refuse it.
	Vertex *Vertex
	// Model is the model name, passed through unchecked. Every provider
	// requires it.
	Model string
	// APIKey authenticates the direct API. ProviderOllama refuses it, and
	// so does any provider when Vertex is set.
	APIKey string
	// BaseURL overrides the provider's endpoint, with the same meaning as
	// the provider package's WithBaseURL: ProviderOpenAICompat requires it
	// and it includes the /v1 path; with Vertex it is the Private Service
	// Connect endpoint's root. It is optional except for openaicompat.
	BaseURL string
}

// Vertex holds the Vertex AI settings. The credentials are supplied by the
// application; nothing discovers them (ADR-0009).
type Vertex struct {
	Credentials *auth.Credentials
	Project     string
	Location    string
}

// New builds the chat client for provider id from opts. It covers every
// ProviderID in this module (ProviderVLLM, having ProviderOpenAICompat's
// value, builds an openaicompat client); a Vertex client keeps its
// adapter's ID. The returned value is the provider package's concrete
// client, so optional interfaces such as llms.ModelLister stay reachable by
// type assertion.
//
// Every error is a *llms.ProviderError of kind config with Provider id.
// New's own refusals carry ErrUnknownProvider or ErrUnsupportedOption as
// the Cause; a provider constructor's errors are returned unchanged.
func New(id llms.ProviderID, opts Options) (llms.ChatClient, error) {
	switch id {
	case llms.ProviderAnthropic:
		return newAnthropic(opts)
	case llms.ProviderOpenAI:
		return newOpenAI(opts)
	case llms.ProviderGoogle:
		return newGoogle(opts)
	case llms.ProviderOllama:
		return newOllama(opts)
	case llms.ProviderOpenAICompat:
		return newOpenAICompat(opts)
	default:
		return nil, &llms.ProviderError{
			Provider: id, Kind: llms.ErrorKindConfig,
			Message: fmt.Sprintf("unknown provider %q", string(id)),
			Cause:   ErrUnknownProvider,
		}
	}
}

func newAnthropic(opts Options) (llms.ChatClient, error) {
	if err := refuse(llms.ProviderAnthropic, opts, true, true); err != nil {
		return nil, err
	}
	if v := opts.Vertex; v != nil {
		o := forward(opts, anthropicvertex.WithModel, nil, anthropicvertex.WithBaseURL, anthropicvertex.WithHTTPClient)
		o = append(o, anthropicvertex.WithVertex(v.Project, v.Location), anthropicvertex.WithCredentials(v.Credentials))
		return chat(anthropicvertex.New(o...))
	}
	return chat(anthropic.New(forward(opts, anthropic.WithModel, anthropic.WithAPIKey, anthropic.WithBaseURL, anthropic.WithHTTPClient)...))
}

func newOpenAI(opts Options) (llms.ChatClient, error) {
	if err := refuse(llms.ProviderOpenAI, opts, false, true); err != nil {
		return nil, err
	}
	return chat(openai.New(forward(opts, openai.WithModel, openai.WithAPIKey, openai.WithBaseURL, openai.WithHTTPClient)...))
}

func newGoogle(opts Options) (llms.ChatClient, error) {
	if err := refuse(llms.ProviderGoogle, opts, true, true); err != nil {
		return nil, err
	}
	o := forward(opts, google.WithModel, google.WithAPIKey, google.WithBaseURL, google.WithHTTPClient)
	if v := opts.Vertex; v != nil {
		o = append(o, google.WithVertex(v.Project, v.Location), google.WithCredentials(v.Credentials))
	}
	return chat(google.New(o...))
}

func newOllama(opts Options) (llms.ChatClient, error) {
	if err := refuse(llms.ProviderOllama, opts, false, false); err != nil {
		return nil, err
	}
	return chat(ollama.New(forward(opts, ollama.WithModel, nil, ollama.WithBaseURL, ollama.WithHTTPClient)...))
}

func newOpenAICompat(opts Options) (llms.ChatClient, error) {
	if err := refuse(llms.ProviderOpenAICompat, opts, false, true); err != nil {
		return nil, err
	}
	return chat(openaicompat.New(forward(opts, openaicompat.WithModel, openaicompat.WithAPIKey, openaicompat.WithBaseURL, openaicompat.WithHTTPClient)...))
}

// forward turns the non-zero common options into a provider's own options,
// so a zero field leaves the provider's default in place. apiKey is nil for
// a provider with no key option; refuse has already rejected a set APIKey
// there.
func forward[O any](opts Options, model, apiKey, baseURL func(string) O, httpClient func(*http.Client) O) []O {
	var o []O
	if opts.Model != "" {
		o = append(o, model(opts.Model))
	}
	if opts.APIKey != "" && apiKey != nil {
		o = append(o, apiKey(opts.APIKey))
	}
	if opts.BaseURL != "" {
		o = append(o, baseURL(opts.BaseURL))
	}
	if opts.HTTPClient != nil {
		o = append(o, httpClient(opts.HTTPClient))
	}
	return o
}

// refuse checks the options a provider can't use (ADR-0048 §3), before any
// provider constructor runs: Vertex where the provider has no Vertex route,
// APIKey where it takes no key, and APIKey together with Vertex.
func refuse(id llms.ProviderID, opts Options, vertexOK, apiKeyOK bool) error {
	var msg string
	switch {
	case opts.Vertex != nil && !vertexOK:
		msg = "Vertex is not supported by provider " + string(id)
	case opts.APIKey != "" && !apiKeyOK:
		msg = "APIKey is not supported by provider " + string(id)
	case opts.APIKey != "" && opts.Vertex != nil:
		msg = "APIKey and Vertex are exclusive: Vertex authenticates with Vertex.Credentials"
	default:
		return nil
	}
	return &llms.ProviderError{
		Provider: id, Kind: llms.ErrorKindConfig, Message: msg, Cause: ErrUnsupportedOption,
	}
}

// chat converts a provider constructor's typed result to the interface,
// keeping a nil client nil (not a non-nil interface holding a nil pointer).
func chat[C llms.ChatClient](c C, err error) (llms.ChatClient, error) {
	if err != nil {
		return nil, err
	}
	return c, nil
}
