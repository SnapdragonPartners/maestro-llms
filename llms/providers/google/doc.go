// Package google implements the maestro-llms clients for Google Gemini: New
// builds the chat Client and NewEmbeddings the EmbeddingClient, on either the
// Gemini API (WithAPIKey) or Vertex AI (WithVertex + WithCredentials) through
// one shared Option set (ADR-0027, ADR-0028).
//
// Tool-call arguments lose integer precision beyond 2^53 in both
// directions. The genai SDK decodes a response's functionCall args into a
// map[string]any, and re-encodes a request's args through one, so JSON
// numbers become float64: 9007199254740993 arrives in ToolCall.Parameters
// as 9007199254740992, and is sent as that when replayed. This is SDK
// behavior the adapter can't avoid (divergence G5); a tool whose arguments
// carry large integer IDs should declare them as strings in its schema.
package google
