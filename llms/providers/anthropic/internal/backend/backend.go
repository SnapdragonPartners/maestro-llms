// Package backend carries construction signals from the backend leaf
// packages (anthropicvertex) to the base anthropic package without adding
// public API. A signal travels as a marker option through
// anthropic.WithRequestOptions, the channel backends already use, and
// anthropic.New removes it before the options reach the SDK.
package backend

import "github.com/anthropics/anthropic-sdk-go/option"

// noModelListing marks a client whose backend does not serve the Anthropic
// models endpoint. The embedded option is WithoutEnvironmentDefaults, whose
// Apply is a no-op, so the marker is harmless even if it reached the SDK;
// anthropic.New already sets that option on every client.
type noModelListing struct {
	option.RequestOption
	name string
}

// NoModelListing returns a marker that makes the client's ListModels fail
// fast with a config error naming backendName (e.g. "Vertex AI"), without
// sending a request (ADR-0037).
func NoModelListing(backendName string) option.RequestOption {
	return noModelListing{RequestOption: option.WithoutEnvironmentDefaults(), name: backendName}
}

// NoModelListingBackend reports whether o is a NoModelListing marker and,
// if so, the backend name it carries.
func NoModelListingBackend(o option.RequestOption) (string, bool) {
	m, ok := o.(noModelListing)
	return m.name, ok
}
