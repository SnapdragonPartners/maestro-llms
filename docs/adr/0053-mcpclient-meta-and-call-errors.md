# 0053. `mcpclient`: per-call `_meta` and classified call errors

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

ADR-0049 §4.6 made every transport or protocol failure of a tool call one
`Execute` error, and `Execute` sent only the tool's name and arguments.
maestro-agents (#194, its ADR 0025) needs two more things from
`mcpclient`, and neither is specific to it:

1. **Per-call context that isn't a tool argument.** It needs to send the
   run and principal a call acts for, the action identity, and, for a write,
   an idempotency key. MCP's place for that is the request's `_meta`.
   Putting it in the arguments shows it to the model and the schema, and
   HTTP headers work only for Streamable HTTP.
2. **Whether a failed call may be retried.** A write needs to know if the
   request never left (a retry is safe), if the server answered (a retry
   won't help), or if the outcome is unknown. A wrong "not sent" repeats a
   write.

The go-sdk (v1.8.0) can't make the second distinction from its errors:

- `mcp.ErrConnectionClosed` covers a request that was never written (the
  connection was already closing or broken), but the SDK picks it by
  matching JSON-RPC codes -32003 and -32004, so a server reply with either
  code lands there too, flattened to text.
- A failed `http.Client.Do` is wrapped as "rejected by transport", and the
  SDK's comment says the request "didn't reach the server". That's false
  when the connection breaks after the body was written, for example while
  waiting for response headers.
- A cancelled context returns `ctx.Err()` whether or not the request went
  out.
- When the Streamable HTTP client fails the connection (including when a
  stream reconnect gets a 404 for an expired session), it hands the same
  error value to every call still waiting, including calls the server
  already received and may have run. The text is the same as a 404 on the
  call's own request.
- A stdio read of a message that is neither request nor response fails the
  connection with a bare JSON-RPC -32600 error, which also reaches every
  waiting call.

Watching the wire instead isn't possible: wrapping the SDK's `Transport`
would break Streamable HTTP, because the SDK calls an unexported method on
its own connection type.

## Decision

### 1. `Options.Meta`

```go
type Options struct {
    // ...
    Meta func(ctx context.Context, call llms.ToolCall) (map[string]any, error)
}
```

`Execute` calls `Meta` with its own `ctx` and the call, and sends the map as
the `tools/call` request's `_meta`. The app puts its values in `ctx` (it
already wraps `toolloop.Tool`), and `Meta` reads them.

- Nil `Meta`, or a nil or empty map, sends no app keys. The SDK still adds
  its own `io.modelcontextprotocol/...` keys under protocol 2026-07-28.
- The map is copied before it is sent, because the SDK writes its keys into
  the map it gets.
- `Meta` returns an error so an app missing its run context can fail
  closed instead of sending the call without `_meta`.
- Keys must follow MCP's `_meta` key format (an optional prefix of
  dot-separated labels and a slash, then a name). A prefix with a label
  `modelcontextprotocol` or `mcp` (any case) is refused, since MCP reserves
  it and the SDK doesn't overwrite a key an app has set, so an app key could
  replace the protocol's own. The unprefixed name `progressToken`, which MCP
  defines, is refused too. Values must marshal to JSON.
- A `Meta` error or an invalid map is `ErrNotSent` (§2).

`Meta` applies to `tools/call` only. Reading `_meta` on the server side
(`mcpserver`, ADR-0050) is not part of this decision.

### 2. Three classes of call error

Every `Execute` error matches exactly one of three sentinels under
`errors.Is`, and keeps the original error wrapped (so toolloop's
`classifyStop` still finds a context error and reports `OutcomeCanceled` or
`OutcomeDeadline`):

| Class | Meaning | Errors |
|---|---|---|
| `ErrNotSent` | The request was never handed to the SDK; the server can't have acted on it. A retry is safe (after reconnecting, where the session or connection is gone). | the session is closed; the SDK connection has ended; `ctx` is already done; the arguments aren't valid JSON; `Meta` failed or returned an invalid map |
| `ErrServerError` | The server answered this call with a JSON-RPC error, or with a result the client finds malformed. Asking again won't help. A `*CallError` (`Tool`, `Code`, `Message`, `Data`, `Malformed`). | §2.2 |
| `ErrOutcomeUnknown` | Anything else: the request may have been sent and no answer was matched to it. | timeouts and cancellation after the call started, dropped connections, every failed HTTP request, HTTP error statuses, an expired session, a JSON-RPC error the SDK may have produced itself |

The rule behind every row: **when the classification can't be certain, the
error is `ErrOutcomeUnknown`, never `ErrNotSent`.** Unknown is safe for the
caller to act on; a wrong "not sent" repeats a write.

#### 2.1 Not sent is decided before the SDK is called

Nothing the SDK returns is classified as `ErrNotSent`, because its errors
can't tell "never written" from "written and not answered" (Context).
Instead `Execute` checks everything that can fail before it calls
`CallTool`, and refuses the call itself.

The "connection has ended" check comes from a goroutine started by
`Connect` that waits on the SDK session's `Wait` and records when it
returns. `Wait` returns only once the JSON-RPC connection is completely
done, after which nothing can be written on it. A call that races the end
of the connection gets the SDK's error and is `ErrOutcomeUnknown`.

So after a server restart or an expired session, the call that discovers
it is `ErrOutcomeUnknown`, and once the dead connection has finished, the
following calls are `ErrNotSent` until the app reconnects.

An expired-session 404 is `ErrOutcomeUnknown` even though MCP says a
server MUST answer a terminated session's requests with 404, which implies
it didn't run them: the SDK reports the same error when a call's own
response stream reconnects and gets the 404, and hands it to every waiting
call (Context), so it can't be tied to a request the server never saw.

#### 2.2 Server error is only this call's own answer

An SDK error is `ErrServerError` only when it directly wraps a JSON-RPC
error object, which is how the go-sdk returns a JSON-RPC error response
that it matched to this call by request ID (`calling %q: %w`), and the code
is not one the SDK also creates itself: -32600 and -32700 (its read
failures, handed to every waiting call) and -32003 to -32005 (its
"closing" and "rejected by transport" sentinels). A server that really
sends one of those codes is reported as unknown, which is safe.

A JSON-RPC error inside an HTTP error response (4xx or 5xx with a
JSON-RPC body) is `ErrOutcomeUnknown`: the SDK hands that same error to
every waiting call when it fails the connection, so it can't be tied to
this call.

A result that decodes but that `internal/convert` rejects (ADR-0049 §4.7)
is a `*CallError` with `Malformed` set: it is this call's own result. A
result the SDK itself fails to decode is `ErrOutcomeUnknown`, since that
error's shape isn't distinctive.

Note that the go-sdk server sends a tool handler's error with code 0, so
`Code` 0 is a real code; `Malformed` is what marks a malformed result.

#### 2.3 Pinned to the go-sdk's error shapes

The classification reads the go-sdk v1.8.0's error shapes: the direct
wrap in its call layer, its sentinel codes, and its handling of a failed
connection. Tests pin each one end to end (the in-memory transport, a raw
JSON-RPC server, and Streamable HTTP against a real go-sdk handler), and a
table test pins the shapes `classify` relies on. A go-sdk bump must pass
them; Dependabot keeps go-sdk bumps ungrouped (ADR-0049 §6). If a future
SDK changes a shape, the safe failure is a server error reported as
unknown; the tests fail before that ships.

### 3. API

```go
var (
    ErrNotSent        = errors.New("not sent")
    ErrServerError    = errors.New("server error")
    ErrOutcomeUnknown = errors.New("outcome unknown")
)

type CallError struct {
    Err       error           // the underlying error
    Tool      string          // the server's name
    Message   string
    Data      json.RawMessage // the JSON-RPC error's data, as sent
    Code      int64           // zero when Malformed
    Malformed bool
}
```

`*CallError` matches `ErrServerError` through its `Is` method. No go-sdk
type appears in the API (ADR-0049 §2); `Unwrap` reaches the SDK's error, as
`Execute` errors already did.

## Consequences

- maestro-agents' MCP adapter can pass its run context and idempotency
  keys as `_meta`, and decide per class whether to retry a write.
- `Execute`'s behavior on success is unchanged. Its errors gain the three
  classes; their messages change (they now name the class). A caller that
  matched an `Execute` error's text needs updating; `errors.Is` on a
  context error still works.
- A call with invalid JSON arguments, or on a session whose connection has
  ended, now fails before reaching the SDK.
- `mcpserver` doesn't yet pass a request's `_meta` to `Execute`; a server
  built with it can't read what this client sends until that is added.
- No `MAESTRO_DIVERGENCES.md` row: Maestro has no MCP client (ADR-0049).

## References

- Issue #194; maestro-agents#23 and its ADR 0025.
- ADR-0049 §2, §4.6, §4.7 and §6; ADR-0050; ADR-0011 and ADR-0019
  (loop outcomes).
- MCP specification: `_meta` key format and reservations; Streamable HTTP
  session management (404 for a terminated session).
- go-sdk v1.8.0: `mcp/transport.go` (`call`), `internal/jsonrpc2/conn.go`,
  `mcp/streamable.go` (`Write`, `handleSSE`, `checkResponse`).
