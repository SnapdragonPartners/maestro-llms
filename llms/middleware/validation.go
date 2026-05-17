package middleware

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ValidationError reports a structurally invalid ChatRequest rejected by the
// validation middleware before any provider call. Like *CircuitOpenError it
// is deliberately neither a *llms.ProviderError nor a *llms.LimitError, so
// llms.Retryable is false: a malformed request fails identically on every
// attempt, so retrying is pointless. See docs/adr/0006-validation-error.md.
type ValidationError struct {
	Reason string
}

func (e *ValidationError) Error() string { return "invalid chat request: " + e.Reason }

// ValidationChat returns middleware that rejects structurally invalid chat
// requests up front. Per the spec's recommended order it is the OUTERMOST
// middleware, so a malformed request never consumes retry, timeout, circuit,
// or rate-limit work.
//
// Scope is structural and app-neutral only (divergence M3): text-only System,
// non-empty messages, and tool-call <-> tool-result pairing. Semantic/agent
// policy (empty-response handling, guidance injection) is the application's,
// not the toolkit's. There is intentionally no ValidationEmbeddings: the
// embedding batch limit is the provider client's responsibility (spec: app
// owns chunking), so there is no app-neutral structural rule to enforce here.
func ValidationChat() ChatMiddleware {
	return func(next llms.ChatClient) llms.ChatClient {
		return &validationChat{next: next}
	}
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
			return &ValidationError{fmt.Sprintf("System part %d: only text is allowed in v0, got %q", i, t)}
		}
	}
	if len(req.Messages) == 0 {
		return &ValidationError{"at least one message is required"}
	}
	ps := &pairingState{pending: map[string]struct{}{}, resolved: map[string]struct{}{}}
	for mi := range req.Messages {
		m := &req.Messages[mi]
		if len(m.Content) == 0 {
			return &ValidationError{fmt.Sprintf("message %d (%s): empty content", mi, m.Role)}
		}
		if err := validateParts(mi, m); err != nil {
			return err
		}
		if err := ps.step(mi, m); err != nil {
			return err
		}
	}
	if len(ps.pending) > 0 {
		return &ValidationError{"request ends with tool calls lacking results: " + idList(ps.pending)}
	}
	return nil
}

// validateParts checks each content part matches its discriminant.
func validateParts(mi int, m *llms.Message) error {
	for pi := range m.Content {
		p := &m.Content[pi]
		switch p.Type {
		case llms.ContentText:
		case llms.ContentToolCall:
			if p.ToolCall == nil || p.ToolCall.ID == "" {
				return &ValidationError{fmt.Sprintf("message %d part %d: tool_call missing ToolCall/ID", mi, pi)}
			}
		case llms.ContentToolResult:
			if p.ToolResult == nil || p.ToolResult.ToolCallID == "" {
				return &ValidationError{fmt.Sprintf("message %d part %d: tool_result missing ToolResult/ToolCallID", mi, pi)}
			}
		default:
			return &ValidationError{fmt.Sprintf("message %d part %d: unknown content type %q", mi, pi, p.Type)}
		}
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
			return &ValidationError{fmt.Sprintf("message %d: user turn before tool results delivered for: %s", mi, idList(s.pending))}
		}
	default:
		// Valid roles are exactly user/assistant/tool (there is no system
		// role — system is ChatRequest.System). Reject anything else at the
		// outermost validator instead of letting it reach a provider adapter.
		return &ValidationError{fmt.Sprintf("message %d: unknown role %q", mi, m.Role)}
	}
	return nil
}

func (s *pairingState) assistant(mi int, m *llms.Message) error {
	if len(s.pending) > 0 {
		return &ValidationError{fmt.Sprintf("message %d: previous tool calls have no results: %s", mi, idList(s.pending))}
	}
	s.resolved = map[string]struct{}{}
	for pi := range m.Content {
		p := &m.Content[pi]
		if p.Type != llms.ContentToolCall {
			continue
		}
		id := p.ToolCall.ID
		if _, dup := s.pending[id]; dup {
			return &ValidationError{fmt.Sprintf("message %d: duplicate tool_call ID %q", mi, id)}
		}
		s.pending[id] = struct{}{}
	}
	return nil
}

func (s *pairingState) tool(mi int, m *llms.Message) error {
	for pi := range m.Content {
		p := &m.Content[pi]
		if p.Type != llms.ContentToolResult {
			return &ValidationError{fmt.Sprintf("message %d: a tool message may only contain tool_result parts", mi)}
		}
		id := p.ToolResult.ToolCallID
		if _, ok := s.pending[id]; !ok {
			if _, done := s.resolved[id]; done {
				return &ValidationError{fmt.Sprintf("message %d: duplicate tool_result for ID %q", mi, id)}
			}
			return &ValidationError{fmt.Sprintf("message %d: orphaned tool_result for unknown tool_call ID %q", mi, id)}
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
