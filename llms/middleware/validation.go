package middleware

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/media"
)

// ValidationError reports a structurally invalid ChatRequest or
// EmbeddingRequest rejected by the validation middleware before any provider
// call. Like *CircuitOpenError it
// is deliberately neither a *llms.ProviderError nor a *llms.LimitError, so
// llms.Retryable is false: a malformed request fails identically on every
// attempt, so retrying is pointless. See docs/adr/0006-validation-error.md.
type ValidationError struct {
	Reason string
	// embedding selects the "invalid embedding request" prefix; it is set
	// only by ValidationEmbeddings, so a caller-built ValidationError keeps
	// the chat prefix (ADR-0045).
	embedding bool
}

func (e *ValidationError) Error() string {
	if e.embedding {
		return "invalid embedding request: " + e.Reason
	}
	return "invalid chat request: " + e.Reason
}

// ValidationChat returns middleware that rejects structurally invalid chat
// requests up front. Per the spec's recommended order it is the OUTERMOST
// middleware, so a malformed request never consumes retry, timeout, circuit,
// or rate-limit work.
//
// Scope is structural and app-neutral only (divergence M3): text-only System,
// non-empty messages, well-formed image/document parts on user messages
// (ADR-0040), and tool-call <-> tool-result pairing. Semantic/agent
// policy (empty-response handling, guidance injection) is the application's,
// not the toolkit's. ValidationEmbeddings is the embedding-side counterpart.
func ValidationChat() ChatMiddleware {
	return func(next llms.ChatClient) llms.ChatClient {
		return &validationChat{next: next}
	}
}

// ValidationEmbeddings returns middleware that rejects structurally invalid
// embedding requests up front with a *ValidationError (ADR-0045): an input
// that sets both Text and Parts, or whose Parts hold an empty text part, a
// malformed image or document part, or any other part type. Like
// ValidationChat it belongs outermost. The batch limit stays the provider
// client's check (the application owns chunking), and which media a
// provider accepts stays the adapter's.
func ValidationEmbeddings() EmbeddingMiddleware {
	return func(next llms.EmbeddingClient) llms.EmbeddingClient {
		return &validationEmbedding{next: next}
	}
}

type validationEmbedding struct {
	next llms.EmbeddingClient
}

func (c *validationEmbedding) Model() llms.ModelRef   { return c.next.Model() }
func (c *validationEmbedding) DefaultDimensions() int { return c.next.DefaultDimensions() }

func (c *validationEmbedding) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	for i := range req.Inputs {
		if err := media.CheckEmbeddingInput(&req.Inputs[i]); err != nil {
			return llms.EmbeddingResponse{}, &ValidationError{Reason: fmt.Sprintf("input %d: %v", i, err), embedding: true}
		}
	}
	return c.next.Embed(ctx, req) //nolint:wrapcheck // pass provider/limiter errors through unwrapped
}

type validationChat struct {
	next llms.ChatClient
}

func (c *validationChat) Model() llms.ModelRef { return c.next.Model() }

func (c *validationChat) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	if err := validateChatRequest(req); err != nil {
		return llms.ChatResponse{}, err
	}
	return c.next.Complete(ctx, req) //nolint:wrapcheck // pass provider/limiter errors through unwrapped
}

func validateChatRequest(req llms.ChatRequest) error {
	for i := range req.System {
		if t := req.System[i].Type; t != llms.ContentText {
			return &ValidationError{Reason: fmt.Sprintf("System part %d: only text is allowed, got %q", i, t)}
		}
	}
	return ValidateMessages(req.Messages)
}

// ValidateMessages applies ValidationChat's structural message rules to a
// transcript: at least one message, non-empty content, content parts legal
// for their role, and tool-call <-> tool-result pairing with no calls left
// unanswered at the end. It returns a *ValidationError, or nil. toolloop
// uses it to check a compacted transcript before sending it (ADR-0030).
func ValidateMessages(msgs []llms.Message) error {
	if len(msgs) == 0 {
		return &ValidationError{Reason: "at least one message is required"}
	}
	ps := &pairingState{pending: map[string]struct{}{}, resolved: map[string]struct{}{}}
	for mi := range msgs {
		m := &msgs[mi]
		if !hasNonReasoning(m) {
			if m.Role == llms.RoleAssistant {
				// The usual source is a caller appending a ChatResponse
				// with no text and no tool calls (a reasoning model that
				// spent its output budget on reasoning); say so. Reasoning
				// parts alone don't count: other providers drop them, and
				// a reasoning-only turn is not a reply (ADR-0021).
				return &ValidationError{Reason: fmt.Sprintf("message %d (%s): empty content; drop assistant responses with no text or tool calls (e.g. output budget spent on reasoning) before resending", mi, m.Role)}
			}
			return &ValidationError{Reason: fmt.Sprintf("message %d (%s): empty content", mi, m.Role)}
		}
		if err := validateParts(mi, m); err != nil {
			return err
		}
		if err := ps.step(mi, m); err != nil {
			return err
		}
	}
	if len(ps.pending) > 0 {
		return &ValidationError{Reason: "request ends with tool calls lacking results: " + idList(ps.pending)}
	}
	return nil
}

// hasNonReasoning reports whether m has any part other than reasoning.
func hasNonReasoning(m *llms.Message) bool {
	for i := range m.Content {
		if m.Content[i].Type != llms.ContentReasoning {
			return true
		}
	}
	return false
}

// validateParts checks each content part matches its discriminant AND is
// legal for the message role: tool_call and reasoning only on assistant,
// tool_result only on tool, image and document only on user (ADR-0040) or
// inside a tool result's Parts (ADR-0041). Text is allowed on
// user/assistant here; tool messages are further
// restricted to tool_result-only by pairingState.tool (so the effective
// contract is: text on user/assistant, tool_call on assistant, tool_result
// on tool). Enforcing role/part legality here (not just the payload
// discriminant) stops malformed transcripts from reaching a provider adapter
// that would serialize them.
func validateParts(mi int, m *llms.Message) error {
	for pi := range m.Content {
		p := &m.Content[pi]
		switch p.Type {
		case llms.ContentText:
		case llms.ContentToolCall:
			if err := validateToolCall(mi, pi, m.Role, p); err != nil {
				return err
			}
		case llms.ContentReasoning:
			if err := validateReasoning(mi, pi, m.Role, p); err != nil {
				return err
			}
		case llms.ContentImage, llms.ContentDocument:
			if err := validateMedia(mi, pi, m.Role, p); err != nil {
				return err
			}
		case llms.ContentToolResult:
			if p.ToolResult == nil || p.ToolResult.ToolCallID == "" {
				return &ValidationError{Reason: fmt.Sprintf("message %d part %d: tool_result missing ToolResult/ToolCallID", mi, pi)}
			}
			if m.Role != llms.RoleTool {
				return &ValidationError{Reason: fmt.Sprintf("message %d part %d: tool_result only allowed on tool messages, got role %q", mi, pi, m.Role)}
			}
			if err := media.CheckToolResult(p.ToolResult); err != nil {
				return &ValidationError{Reason: fmt.Sprintf("message %d part %d: %v", mi, pi, err)}
			}
		default:
			return &ValidationError{Reason: fmt.Sprintf("message %d part %d: unknown content type %q", mi, pi, p.Type)}
		}
	}
	return nil
}

// validateReasoning checks a reasoning part: payload and Provider set, and
// only on assistant messages (ADR-0021).
func validateReasoning(mi, pi int, role llms.Role, p *llms.ContentPart) error {
	if p.Reasoning == nil || p.Reasoning.Provider == "" {
		return &ValidationError{Reason: fmt.Sprintf("message %d part %d: reasoning missing Reasoning/Provider", mi, pi)}
	}
	if role != llms.RoleAssistant {
		return &ValidationError{Reason: fmt.Sprintf("message %d part %d: reasoning only allowed on assistant messages, got role %q", mi, pi, role)}
	}
	return nil
}

// validateToolCall checks a tool_call part: payload and ID set, and only on
// assistant messages.
func validateToolCall(mi, pi int, role llms.Role, p *llms.ContentPart) error {
	if p.ToolCall == nil || p.ToolCall.ID == "" {
		return &ValidationError{Reason: fmt.Sprintf("message %d part %d: tool_call missing ToolCall/ID", mi, pi)}
	}
	if role != llms.RoleAssistant {
		return &ValidationError{Reason: fmt.Sprintf("message %d part %d: tool_call only allowed on assistant messages, got role %q", mi, pi, role)}
	}
	return nil
}

// validateMedia checks an image or document part: a well-formed payload
// (media.Check) and only on user messages (ADR-0040).
func validateMedia(mi, pi int, role llms.Role, p *llms.ContentPart) error {
	if _, err := media.Check(p); err != nil {
		return &ValidationError{Reason: fmt.Sprintf("message %d part %d: %v", mi, pi, err)}
	}
	if role != llms.RoleUser {
		return &ValidationError{Reason: fmt.Sprintf("message %d part %d: %s only allowed on user messages, got role %q", mi, pi, p.Type, role)}
	}
	return nil
}

// pairingState tracks tool-call IDs awaiting results across the conversation
// so missing, duplicate, and orphaned tool results can be rejected.
type pairingState struct {
	pending  map[string]struct{}
	resolved map[string]struct{}
}

func (s *pairingState) step(mi int, m *llms.Message) error {
	switch m.Role {
	case llms.RoleAssistant:
		return s.assistant(mi, m)
	case llms.RoleTool:
		return s.tool(mi, m)
	case llms.RoleUser:
		if len(s.pending) > 0 {
			return &ValidationError{Reason: fmt.Sprintf("message %d: user turn before tool results delivered for: %s", mi, idList(s.pending))}
		}
	default:
		// Valid roles are exactly user/assistant/tool (there is no system
		// role — system is ChatRequest.System). Reject anything else at the
		// outermost validator instead of letting it reach a provider adapter.
		return &ValidationError{Reason: fmt.Sprintf("message %d: unknown role %q", mi, m.Role)}
	}
	return nil
}

func (s *pairingState) assistant(mi int, m *llms.Message) error {
	if len(s.pending) > 0 {
		return &ValidationError{Reason: fmt.Sprintf("message %d: previous tool calls have no results: %s", mi, idList(s.pending))}
	}
	s.resolved = map[string]struct{}{}
	for pi := range m.Content {
		p := &m.Content[pi]
		if p.Type != llms.ContentToolCall {
			continue
		}
		id := p.ToolCall.ID
		if _, dup := s.pending[id]; dup {
			return &ValidationError{Reason: fmt.Sprintf("message %d: duplicate tool_call ID %q", mi, id)}
		}
		s.pending[id] = struct{}{}
	}
	return nil
}

func (s *pairingState) tool(mi int, m *llms.Message) error {
	for pi := range m.Content {
		p := &m.Content[pi]
		if p.Type != llms.ContentToolResult {
			return &ValidationError{Reason: fmt.Sprintf("message %d: a tool message may only contain tool_result parts", mi)}
		}
		id := p.ToolResult.ToolCallID
		if _, ok := s.pending[id]; !ok {
			if _, done := s.resolved[id]; done {
				return &ValidationError{Reason: fmt.Sprintf("message %d: duplicate tool_result for ID %q", mi, id)}
			}
			return &ValidationError{Reason: fmt.Sprintf("message %d: orphaned tool_result for unknown tool_call ID %q", mi, id)}
		}
		delete(s.pending, id)
		s.resolved[id] = struct{}{}
	}
	return nil
}

// idList renders a set of IDs sorted, for deterministic error messages.
func idList(set map[string]struct{}) string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}
