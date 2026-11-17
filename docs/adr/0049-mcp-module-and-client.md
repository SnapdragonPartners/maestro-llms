# 0049. MCP module: a nested v0 module, and server tools as `toolloop.Tool`

- **Status:** Proposed
- **Date:** 2026-11-17

## Context

maestro-agents wants tools served by external MCP servers to run in the tool
loop beside in-process Go tools (#150). What it needs is generic: connect to
a server, list its tools, and get each one as a `toolloop.Tool` whose
`Execute` calls the server. Maestro and Morris could use the same thing, and
otherwise every consumer would write it against `toolloop.Tool` themselves.

ADR-0011 keeps MCP adapters out of `toolloop` ("no built-in
filesystem/shell/browser/retrieval/MCP tool adapters"). That stays: this ADR
adds a separate leaf, and `toolloop` and the core don't change.

A second request arrived before this one was built: an MCP **server** (#181)
for a new consumer, a ChatGPT "Dot" plugin written in Go, whose tools
(examples: a basic password manager, a ProtonMail Bridge wrapper) will be
`toolloop.Tool` values like Morris's and maestro-agents'. The server is not
part of this ADR's change; ADR-0050 will decide it. But the two are mirror
images: the client turns MCP tools into `[]toolloop.Tool`, the server serves
`[]toolloop.Tool` over MCP. Both translate the same things (input schemas,
`ToolResult` to and from MCP content, `IsError`), so this ADR fixes the
module and the seam they share, and the client must not hard-wire anything
the server needs to reverse. They release together.

The official Go SDK, `github.com/modelcontextprotocol/go-sdk` (v1.8.0 at
the time of writing), implements the protocol, both transports, and the
server-side OAuth resource-server pieces #181 needs. It meets the
third-party bar (a protocol implementation, not a convenience wrapper).

## Decision

### 1. Packaging: a nested module at v0

A separate Go module, `github.com/SnapdragonPartners/maestro-llms/mcp`,
with its own `go.mod` in `mcp/`:

```
mcp/
  go.mod                 module github.com/SnapdragonPartners/maestro-llms/mcp
  mcpclient/             this ADR (#150)
  mcpserver/             ADR-0050 (#181)
  internal/convert/      the shared translation (§3)
```

- **Why a module, not a package in the v1 module.** Every dependency of the
  root module becomes a dependency of every consumer's build graph, and
  maestro-agents passes its own on to every agent that imports it (#150
  comments). A separate module keeps the go-sdk, and its JSON schema, JWT
  and OAuth dependencies, out of the root module's graph.
- **Why v0.** MCP moves fast (a new protocol revision roughly twice a
  year). The module starts at v0 and is outside the README "Stability
  contract (v1)" until it has users and a stable shape; its own v1 is a
  later decision. The README says so.
- **Tags** are `mcp/vX.Y.Z` (Go's prefix rule for nested modules). The
  first release is `mcp/v0.1.0`, containing both `mcpclient` and
  `mcpserver`. The root module is released only if a change to it is
  needed.
- **Root dependency.** `mcp/go.mod` requires the root module at a released
  version (`v1.7.0` now) with **no `replace`** directive, so what CI tests
  is what consumers get. If an `mcp` change ever needs something unreleased
  in the root module, the root module releases first. A CI step also builds
  and tests `mcp/` against the root at HEAD through a temporary `go.work`
  (not committed), so a root change that breaks `mcp` fails its own PR
  rather than the next `mcp` release.
- **Imports.** `mcp` packages import `llms` and `llms/toolloop` only. They
  cannot import `llms/internal/...` (Go's internal rule), so the structural
  media checks stay where they are: the loop and adapters run them (§5).

### 2. No SDK types in the API

No go-sdk type appears in an exported identifier of `mcpclient`,
`mcpserver` or anything else in the module. The SDK can then change, or be
replaced, without breaking callers, and the module can reach its own v1
without inheriting the SDK's contract. There is no escape hatch in v0; one
can be added when a consumer needs a setting this API doesn't expose.

### 3. The shared translation: `mcp/internal/convert`

One package owns every rule that crosses the boundary, as pure functions
with tests per case, so the client and server can't drift:

- MCP tool → `llms.ToolDefinition` (name check, schema normalization, §4.3,
  §4.4) and, for ADR-0050, the reverse.
- MCP call result → `toolloop.ToolResult` (§4.5) and, for ADR-0050, the
  reverse.
- The portable tool-name rule (§4.3), shared by both sides.

### 4. `mcpclient`

```go
package mcpclient

// Transport says how to reach a server. It is opaque: build one with Command
// or StreamableHTTP.
type Transport struct{ /* unexported */ }

// Command runs cmd as a local server and talks to it over stdin/stdout.
// The session owns the process: Close stops it.
func Command(cmd *exec.Cmd) Transport

// StreamableHTTP connects to a Streamable HTTP endpoint. client (nil =
// http.DefaultClient) carries any credentials, for example through its
// Transport; the package adds none.
func StreamableHTTP(endpoint string, client *http.Client) Transport

type Options struct {
    // Name maps a server's tool name to the name offered to the model.
    // Nil keeps the server's name. Execute always calls the server's name.
    Name func(serverName string) string
    // Filter selects tools by the server's name. Nil keeps every tool.
    Filter func(serverName string) bool
}

// Connect opens a session: it starts or dials the server and runs the MCP
// initialize handshake.
func Connect(ctx context.Context, t Transport, opts Options) (*Session, error)

type Session struct{ /* unexported */ }

// Tools lists the server's tools (every page) and returns them as
// toolloop.Tool values in the server's order.
func (s *Session) Tools(ctx context.Context) ([]toolloop.Tool, error)

// Close ends the session (and stops a Command server). Execute on a tool
// from a closed session returns an error.
func (s *Session) Close() error

// InvalidToolError reports a server tool that can't be offered to a model.
type InvalidToolError struct {
    Err  error
    Tool string // the server's name
}
```

#### 4.1 Lifecycle and concurrency

A `Session` is one connection. `toolloop.Tool` has no lifecycle, so the
tools close over their session, and the caller owns `Close` (maestro-agents'
registry will). A `Session` is safe for concurrent use: tools from one
session may run at once, and `Tools` may be called again while they do. A
server that dies (process exit, dropped connection) makes later calls fail;
reconnecting is the caller's job (the go-sdk's Streamable transport retries
a dropped stream itself first).

The client identifies itself in `initialize` as
`maestro-llms-mcp` with the module version. It offers no client
capabilities (no sampling, roots or elicitation) in this version.

#### 4.2 Snapshot, not live

`Tools` returns the list at the time of the call. The package does not
follow `notifications/tools/list_changed`; a caller that wants the change
calls `Tools` again. A call to a tool the server has since removed comes
back as the server's error (§4.6).

#### 4.3 Names

The name offered to the model must satisfy the portable rule
`^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$`: the intersection of Anthropic's and
OpenAI's `^[a-zA-Z0-9_-]{1,64}$` and Gemini's rule that a name starts with
a letter or underscore. The check runs after `Options.Name`. A name that
fails it is an `InvalidToolError`, never silently rewritten: a rewritten
name would be invisible to the app's grants and logs, and two rewrites
could collide. Two server tools mapped to the same name are an
`InvalidToolError` for each. Collisions *between* servers are the caller's
(toolloop already rejects duplicate names, and maestro-agents' registry
namespaces them).

The definition's description is the tool's `description`, falling back to
its `title`, then `annotations.title`.

#### 4.4 Input schemas

The schema is sent as JSON, normalized in one way only: local references
(`$ref` to `#/$defs/...` or `#/definitions/...`) are inlined and the
definitions removed, because Gemini doesn't accept them and the Google
adapter's schema conversion would otherwise drop the referenced structure
without an error. A recursive reference, a remote reference, or a pointer
that doesn't resolve is an `InvalidToolError`. A tool with no schema gets
`{"type":"object"}`. A top-level `type` other than `"object"` is an
`InvalidToolError` (MCP requires an object schema, and so does every
adapter). Nothing else is rewritten; any other keyword a provider can't use
is that adapter's concern, as it is for in-process tools.

#### 4.5 Fail closed, filter to recover

`Tools` returns an error if any tool that passed `Filter` is invalid. The
error joins one `InvalidToolError` per bad tool (so `errors.As` finds them,
and the message names every one), and no tools are returned. A caller that
wants the rest filters the bad ones out and calls again. Failing the whole
list keeps a misconfigured server from silently shrinking the tool set.

#### 4.6 Calls and errors

`Execute` sends `tools/call` with the server's tool name and
`ToolCall.Parameters` as the arguments (`{}` if empty), under the `ctx` the
loop passes.

- A transport or protocol failure (the server is gone, a JSON-RPC error
  such as an unknown tool, a malformed result) is an `Execute` **error**: a
  loop-visible stop, `OutcomeToolError`, or `OutcomeCanceled` /
  `OutcomeDeadline` when the loop's context ended it (ADR-0011, ADR-0019).
- A result with `isError: true` is a `ToolResult` with `IsError: true`:
  model-visible, and the loop continues.
- `Stop` is never set: a remote tool can't end the loop. An app that wants
  a terminal MCP tool wraps it.

#### 4.7 Results

The result's content blocks map in order:

| MCP content | `toolloop.ToolResult` |
|---|---|
| text | text |
| image | `ContentImage` part with its MIME type |
| embedded resource with `text` | text |
| embedded resource with `blob` | `ContentImage` if its MIME type is `image/*`, else `ContentDocument` |
| resource link | a text line naming the link (`name`, `uri`, MIME type if given) |
| audio | not supported: the whole result is `IsError: true` with a message saying the tool returned audio |

Empty text is dropped first: a text block or embedded text resource whose
text is empty contributes nothing. MCP permits empty text, but ADR-0041
rejects an empty text part, so keeping it would turn a successful call into
`OutcomeToolError`. Then, if every remaining block is text, the result is
`Content`: the texts joined with a newline (`""` if none remain). If any
block is an image or document, the result is `Parts` in block order
(ADR-0041), and the loop's and adapters' existing media checks apply. A provider that doesn't take that media in tool results rejects it
pre-call as today (ADR-0041); the client doesn't predict per provider.

A result with no content blocks and a `structuredContent` value gets that
value as JSON text. When there are content blocks, `structuredContent` is
not added (MCP asks servers to include a text form of it already).

A block the SDK can't decode, or an image or blob with no data or no MIME
type, makes the result malformed: an `Execute` error (§4.6), because it is a
server bug the app should see rather than something the model can fix.

#### 4.8 Credentials and OAuth

The package sources no credentials. For HTTP, the caller's `http.Client`
carries them. For stdio, the caller's `exec.Cmd` sets the environment. The
go-sdk's client-side OAuth flow (authorization-code with a browser
redirect) is not exposed in this version; it is app UI territory and no
consumer has asked.

### 5. What this ADR reserves for `mcpserver` (ADR-0050)

So the client doesn't preclude it, the server is expected to:

- take `[]toolloop.Tool` (the same values a `toolloop.Run` takes) and serve
  them over Streamable HTTP as an `http.Handler`, and over stdio;
- translate through the same `internal/convert` package, in reverse (an
  `Execute` error becomes an MCP error response; `IsError` becomes
  `isError`; `Parts` become image/embedded-resource blocks);
- act as an OAuth **resource server** only: validate bearer tokens with an
  app-supplied verifier and publish protected-resource metadata (the
  go-sdk's `auth.RequireBearerToken` and metadata handler), and hand the
  verified token's identity to `Execute` through the context. The
  authorization server (client registration, consent, token issuing)
  belongs to the app or its identity provider, not this package.

ADR-0050 decides these; nothing here commits the server's API.

### 6. CI and tooling

- `make lint`, `make test` and `make tidy` cover `mcp/` as well as the
  root, so the pre-push hook and CI's `build-lint-test` do too.
- The CI step in §1 tests `mcp/` against the root at HEAD.
- Dependabot watches `/mcp` (gomod), with go-sdk bumps ungrouped like the
  provider SDKs.
- `govulncheck` runs in both modules.
- buggrit and CodeQL already scan the whole repository.

### 7. Testing

Unit tests, no network and no keys:

- `internal/convert`: the name rule (valid, too long, leading digit,
  mapping, duplicates after mapping), schema normalization (inlining
  nested and repeated refs, `definitions`, recursion, remote and dangling
  refs, missing schema, non-object type) and every row of §4.7, plus
  `structuredContent`, malformed blocks, and empty text (alone, between
  texts, and beside an image or document, where the built `Parts` must
  pass toolloop's `ToolResult.Parts` check).
- `mcpclient` end to end over the go-sdk's in-memory transport against a
  real go-sdk server: listing across pages, `Filter`, `Name`, the
  fail-closed error, a call's arguments and result, `isError`, a JSON-RPC
  error, context cancellation, concurrent calls, and use after `Close`.
- A stdio test: the test binary re-executes itself as a small server
  (a `TestMain` helper), so `Command` and process shutdown on `Close` are
  covered.
- A `toolloop.Run` with `testllm` driving an MCP tool through the loop, so
  the outcome mapping in §4.6 is checked where it matters.

## Consequences

- maestro-agents (and Maestro or Morris later) gets MCP tools by adding one
  module; nobody else's build graph changes.
- Two release lines in one repository: root `vX.Y.Z` and `mcp/vX.Y.Z`.
  Release notes and the README "Versioning & license" section cover both.
- A future `toolloop` change that the client depends on means a root
  release before the `mcp` release that uses it.
- No `MAESTRO_DIVERGENCES.md` row: there is no Maestro MCP client to diverge
  from (Maestro bridges MCP outward, to Claude Code).
- The module is v0: its API can change in a minor release until its own v1.

## References

- Issues #150 (client) and #181 (server).
- ADR-0011 (toolloop scope; MCP adapters stay out of `toolloop`), ADR-0017
  (terminal tools), ADR-0019 (deadline outcome), ADR-0041 (tool-result
  parts), ADR-0047 (toolboxes: servers with many tools pair with it).
- README "Stability contract (v1)".
- MCP specification, tools and authorization sections; go-sdk v1.8.0.
