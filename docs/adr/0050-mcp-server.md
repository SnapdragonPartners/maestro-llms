# 0050. `mcpserver`: serve `toolloop.Tool` over MCP, as an OAuth resource server

- **Status:** Proposed
- **Date:** 2026-11-17

## Context

ADR-0049 created the nested v0 module `mcp/` and its client, and reserved
the server shape (§5). #181 asks for the server: a ChatGPT "Dot" plugin,
written in Go, will serve its tools (examples: a basic password manager, a
ProtonMail Bridge wrapper) to ChatGPT and other external agents. Those tools
are `toolloop.Tool` values, like Morris's and maestro-agents'. Because the
tools act on a user's own data, the HTTP endpoint needs OAuth.

What the two specifications ask of the server:

- **MCP authorization (2026-07-28).** A protected server is an OAuth 2.1
  *resource server*. It MUST publish OAuth 2.0 Protected Resource Metadata
  (RFC 9728). It MUST validate access tokens, including "that access tokens
  were issued specifically for them as the intended audience" (RFC 8707),
  and MUST NOT accept or pass on any other token. Invalid or expired tokens
  MUST get a 401. A 401 SHOULD carry `WWW-Authenticate: Bearer` with
  `resource_metadata` and `scope`. A token with too few scopes SHOULD get a
  403 with `error="insufficient_scope"`, `scope` and `resource_metadata`.
  stdio servers SHOULD NOT use this flow; they take credentials from the
  environment. The authorization server is "beyond the scope of this
  specification".
- **ChatGPT (OpenAI Apps SDK auth guide).** ChatGPT discovers the server's
  metadata at `/.well-known/oauth-protected-resource` (or via the 401
  challenge) and sends `resource` as the RFC 8707 `resource` parameter, so
  the authorization server can mint a token whose audience is that
  resource. It expects the server to check signature, `iss`, `exp`/`nbf`,
  audience and scopes on every request. Everything else ChatGPT needs is
  required of the **authorization server**: RFC 8414 or OIDC discovery,
  PKCE with `S256`, client registration by CIMD and/or DCR, RFC 9207 `iss`
  in authorization responses, the `resource` copied into the token's `aud`,
  and `openid`/`email` for workspace restrictions. Per-tool auth
  (`_meta.securitySchemes`, and `_meta["mcp/www_authenticate"]` on an
  `isError` result to start linking from a tool call) and mTLS client
  identification are optional.

The go-sdk (v1.8.0) has a Streamable HTTP handler, a stdio transport,
`auth.RequireBearerToken` (bearer middleware over an app `TokenVerifier`)
and `auth.ProtectedResourceMetadataHandler`. Two details shape the design.
First, `RequireBearerToken` writes a verifier's error text into the
response body of a 500, and its 403 has no `error="insufficient_scope"`.
Second, the SDK's own request plumbing (`RequestExtra.TokenInfo`, and the
stateful transport's session-to-user binding) reads token info only from a
context key that only `RequireBearerToken` can set.

## Decision

### 1. API

```go
package mcpserver

// New checks every tool and builds a server. It never panics on a bad tool.
func New(tools []toolloop.Tool, opts Options) (*Server, error)

type Options struct {
    // Name and Version identify the server in initialize. Empty Name is
    // "maestro-llms-mcp"; empty Version is the module version.
    Name, Version string
    // Instructions is sent in the initialize result. Optional.
    Instructions string
    // Annotations holds behavior hints by tool name. A name not in tools is
    // an error from New.
    Annotations map[string]Annotations
    // Auth protects the HTTP endpoint. Nil serves it without auth.
    Auth *Auth
    // OnError receives errors the client is not shown in full: an Execute
    // error, an invalid ToolResult, a verifier's internal error. Optional.
    OnError func(ctx context.Context, err error)
}

// Handler is the Streamable HTTP endpoint, behind Auth when it is set.
// The app mounts it at the path of Auth.Resource.
func (s *Server) Handler() http.Handler

// ServeStdio serves over stdin/stdout until ctx ends or the client closes.
// Auth does not apply (MCP: stdio takes credentials from the environment).
func (s *Server) ServeStdio(ctx context.Context) error

// MetadataHandler serves the protected resource metadata (RFC 9728), and
// MetadataPath is where the app mounts it, at the root of Auth.Resource's
// origin. Both are for a server with Auth: MetadataHandler is nil and
// MetadataPath empty without it.
func (s *Server) MetadataHandler() http.Handler
func (s *Server) MetadataPath() string

type Auth struct {
    // Resource is the server's canonical URI (RFC 8707/9728): https (http
    // only for a loopback host), absolute, no fragment, no query. Required.
    Resource string
    // AuthorizationServers are issuer URLs for the metadata. At least one.
    AuthorizationServers []string
    // Verifier checks a bearer token. Required.
    Verifier TokenVerifier
    // Audience is the value a token's Audience must contain. Empty means
    // Resource, as MCP requires; set it only for an authorization server
    // that names this resource by another identifier.
    Audience string
    // Scopes are required on every request; all must be present.
    Scopes []string
    // ScopesSupported is published in the metadata. Nil publishes Scopes.
    ScopesSupported []string
    // ResourceName and ResourceDocumentation go in the metadata. Optional.
    ResourceName, ResourceDocumentation string
    // AllowMissingExpiration accepts a token whose TokenInfo has a zero
    // Expiration. Default false: such a token is rejected.
    AllowMissingExpiration bool
    // ClockSkew is tolerance on Expiration. Default 0.
    ClockSkew time.Duration
}

// TokenVerifier verifies a bearer token. It checks the token's signature
// (or introspects it), issuer, expiry and not-before, and returns what it
// asserts. An error wrapping ErrInvalidToken is the token's fault (401);
// any other error is the server's (500).
type TokenVerifier func(ctx context.Context, token string, r *http.Request) (TokenInfo, error)

var ErrInvalidToken = errors.New("invalid token")

type TokenInfo struct {
    Expiration time.Time
    Extra      map[string]any
    Subject    string   // the user; required
    Audience   []string // the token's audiences (aud, or resource)
    Scopes     []string
}

// TokenInfoFromContext returns the verified token's info inside Execute.
// ok is false for an unauthenticated server and over stdio.
func TokenInfoFromContext(ctx context.Context) (TokenInfo, bool)

type Annotations struct {
    ReadOnly, Destructive, Idempotent, OpenWorld *bool
    Title string
}

// InvalidToolError reports a tool New can't serve.
type InvalidToolError struct {
    Err  error
    Tool string
}
```

No go-sdk type appears in the API (ADR-0049 §2). `TokenVerifier`,
`TokenInfo` and `ErrInvalidToken` are this package's own, and are
translated to the SDK's at the boundary.

### 2. Tools

`New` validates every tool and returns an error joining one
`InvalidToolError` per bad tool, so `errors.As` finds them and the message
names every one. A bad tool never reaches the SDK, whose `AddTool` panics
on a schema it dislikes. A tool is invalid when:

- its name fails the portable rule `^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$`
  (`convert.CheckName`, the rule the client applies). Anything this
  server serves, `mcpclient` can consume, and any provider can call. Names
  are never rewritten;
- two tools have the same name;
- `Execute` is nil;
- its input schema is not a JSON object, or its top-level `type` isn't
  `"object"`. An empty schema is served as `{"type":"object"}`. An object
  with no `type` is rejected rather than rewritten, as MCP requires the
  object type;
- the go-sdk refuses to register it. `AddTool` has rejection paths beyond
  the root shape: a schema that won't decode into `map[string]any` (for
  example a number outside float64's range, such as `"default": 1e1000`),
  and malformed, misplaced or duplicate `x-mcp-header` annotations. Its
  rejections are panics, and they can grow in any SDK release. So `New`
  doesn't copy that list. After its own checks, it registers each tool on
  a throwaway SDK server under `recover`, and a panic becomes that tool's
  `InvalidToolError`, with the panic value as `Err`. Only tools that
  register cleanly are added to the real server. This one choke point
  covers every SDK rejection, present or future, and the checks above stay
  only so the common mistakes get clear messages. (The same `x-mcp-header`
  rules are why the go-sdk client drops such tools from a listing, which
  `mcpclient` reports as invalid, ADR-0049 §4.5. Rejecting them here keeps
  the two halves consistent.)

The schema is otherwise served byte for byte (`json.RawMessage`), with its
numbers as written. `$ref`s are not inlined here: that is the client's
normalization for its providers (ADR-0049 §4.4), and an MCP client may
handle references itself. The description is `Definition.Description`.
`Annotations` maps to the tool's MCP annotations (`readOnlyHint`,
`destructiveHint`, `idempotentHint`, `openWorldHint`, `title`). ChatGPT,
for example, uses `readOnlyHint` to decide whether to ask the user before a
call. A name in `Annotations` that isn't a tool is an error. The tool list
is fixed at `New`: there is no `list_changed`.

`get_toolbox` gets no special treatment. Toolboxes (ADR-0047) are a loop
concept; the server serves a flat list, and an app that wants a subset
passes one.

### 3. Calls

A `tools/call` builds `llms.ToolCall{Name, Parameters}` with the request's
`arguments`: `{}` when absent or null, and a JSON-RPC invalid-params error
(-32602), without calling `Execute`, when they are not a JSON object (MCP
requires an object, and `Execute` should never see anything else). `ID` is
empty: MCP has no tool-call ID, and the JSON-RPC request ID belongs to the
transport. As in the loop, the server does not check the arguments against
the schema; `Execute` validates them (ADR-0011).

`Execute` runs under the go-sdk's handler context for the call, which the
SDK cancels on `notifications/cancelled` and when the session closes. The
SDK detaches that context from the HTTP request's, so whether a dropped
connection cancels an in-flight call is the SDK's behavior; a test pins
what it does for v1.8.0, and the package adds no cancellation of its own.
With `Auth`, the context carries the verified `TokenInfo`, which reaches
the handler through the SDK's per-request `RequestExtra`, not through
context values that the detached context may or may not keep.
The bearer token itself is never put in the context or handed to tools:
MCP forbids passing it on, and a tool that calls another service gets its
own credentials (token exchange is the app's).

### 4. Results: the reverse of ADR-0049 §4.7

Through `mcp/internal/convert`, so each direction is the inverse of the
other:

| `toolloop.ToolResult` | MCP |
|---|---|
| `Content` (with no `Parts`) | one text block, even when empty |
| `Parts`: text | text block |
| `Parts`: `ContentImage` | image block (base64 data, MIME type) |
| `Parts`: `ContentDocument` | embedded resource with `blob` and `mimeType`, and the synthetic URI `part:<index>` (MCP requires a URI; this one names the part's position and is not meant to be fetched) |
| `IsError` | `isError: true` |
| `Stop` | dropped (a terminal tool ends a local loop; nothing on the MCP side honors it) |

`structuredContent` is never set: a `ToolResult` has no structured form.

A `ToolResult` the loop would reject is an invalid executor result here
too (ADR-0041). The rules are the structural checks of
`llms/internal/media.Check`, in full:

- `Content` and `Parts` both set;
- a part that isn't text, image or document, or an empty text part;
- an image or document part with nil `Media` or empty `Data`;
- a media type that is empty, doesn't parse (`mime.ParseMediaType`), or
  has no `type/subtype`;
- an image part whose base type isn't `image/*`, or a document part whose
  base type is `image/*`. The second rule also protects the round trip:
  the client turns every `image/*` blob into `ContentImage`, so an
  image-typed document would come back as a different part type.

The server can't import `llms/internal/media`, so convert carries the
same rules, and a test table pins each case against them. The media type
is sent as given; only the check uses the parsed base type. Per-provider
media support is not checked: these are provider-neutral structural rules,
and the remote client's model is unknown.

The client maps these blocks back to the same `ToolResult` (with `Stop`
false), and a test round-trips each row through both directions.

### 5. Errors

- An **`Execute` error**, or an invalid `ToolResult`, is a JSON-RPC error
  (internal error, -32603) with the fixed message `tool "<name>" failed`.
  The error itself goes to `OnError` and never to the client: it is the
  loop-visible, infrastructure kind of failure (ADR-0011), its text may
  name internal hosts or paths, and the client can't act on it.
  `mcpclient` maps a JSON-RPC error back to an `Execute` error, so the
  meaning survives the round trip.
- `IsError` is the model-visible failure and carries the tool's own text,
  as in the loop.
- A **panic** in `Execute` is not recovered, as in `toolloop`.

### 6. HTTP transport

`Handler` is the go-sdk's Streamable HTTP handler in **stateless** mode
with JSON responses (`Stateless`, `JSONResponse`):

- Stateless suits Cloud Run and any multi-instance deployment: no session
  lives in one instance's memory, and the MCP 2026-07-28 direction is
  sessionless (SEP-2567). The server sends no server-to-client requests
  (no sampling, roots or elicitation), so it loses nothing.
- JSON responses: the server streams nothing (no progress notifications),
  and plain JSON passes through proxies that buffer or cut SSE.
- The SDK's defaults stay: DNS-rebinding protection for loopback
  addresses, and its request body limit.
- CORS, rate limiting, IP allowlisting and ChatGPT's optional mTLS are the
  app's middleware around `Handler`.

Either mode can become an `Options` field later without breaking callers.

### 7. Auth: the resource-server boundary

The package is an OAuth **resource server** only. It verifies tokens
through the app's `TokenVerifier` and publishes metadata. It issues no
tokens, registers no clients, and serves no authorization-server metadata:
that is the app's or its identity provider's, and ChatGPT's requirements
for it (PKCE `S256`, CIMD or DCR, RFC 9207 `iss`, `resource` into `aud`)
are listed in the README for whoever picks one.

**Validation runs in this package's middleware**, in front of the SDK's.
For every request to `Handler`:

1. No `Authorization: Bearer <token>` header (case-insensitive scheme, one
   token): **401** with `WWW-Authenticate: Bearer resource_metadata="…"`,
   plus `scope="…"` when `Scopes` is set.
2. `Verifier` returns an error wrapping `ErrInvalidToken`: **401** as in
   step 1, plus `error="invalid_token"`. Any other error: **500** with a
   fixed body; the error goes to `OnError`.
3. The returned `TokenInfo` fails a check: **401** `invalid_token`. The
   checks: `Subject` is non-empty; `Audience` contains `Auth.Audience`
   (default `Resource`), compared exactly; `Expiration` is set (unless
   `AllowMissingExpiration`) and `Expiration + ClockSkew` has not passed.
4. A scope in `Scopes` is missing from `TokenInfo.Scopes`: **403** with
   `error="insufficient_scope"`, `scope` (all of `Scopes`) and
   `resource_metadata`.
5. Otherwise the request proceeds, with the `TokenInfo` in its context.

Response bodies are fixed strings; a verifier's error text never reaches
the client. The audience check runs here, not only in the verifier,
because it is the MCP rule most easily missed and the one that stops a
token minted for another service from being replayed here. It runs once,
at one choke point, whatever verifier the app supplies. That is why
`TokenInfo.Audience` exists.

The package then calls the SDK's `RequireBearerToken` with a verifier that
returns the already-verified info (as an `auth.TokenInfo`, `UserID` =
`Subject`), so the SDK's `RequestExtra.TokenInfo` is populated. Every
decision a client can see is made in step 1 to 5, not by the SDK.

**Metadata.** `MetadataHandler` serves RFC 9728 metadata built from `Auth`:
`resource`, `authorization_servers`, `scopes_supported`,
`bearer_methods_supported: ["header"]`, and the optional name and
documentation. It answers GET (and the CORS preflight; metadata is
public). `MetadataPath` is the RFC 9728 well-known path for `Resource`:
`/.well-known/oauth-protected-resource` with `Resource`'s path appended
(for `https://dot.example.com/mcp`, the path is
`/.well-known/oauth-protected-resource/mcp`). The `resource_metadata` in
challenges is `Resource`'s origin plus that path, so a client that follows
the challenge and a client that probes the well-known path reach the same
document. The app mounts it on its own mux, since the well-known path sits
at the origin root, outside the MCP endpoint's path. An app whose
`Resource` has a path, and that also wants the root well-known path to
work for clients that probe only the root (as OpenAI's guide shows), can
mount the same handler there too.

**`New` checks `Auth`.** `Resource` must be an absolute `https` URI (or
`http` on a loopback host, for local development), with no query or
fragment. There must be at least one authorization server, each an
absolute URI. `Verifier` must be non-nil. Any failure is an error from
`New`.

### 8. Not in this version

- **Per-tool scopes.** ChatGPT's `_meta.securitySchemes` and the
  `_meta["mcp/www_authenticate"]` step-up result need a per-tool policy.
  The likely shape is an `Options` map from tool name to required scopes:
  the server would publish `securitySchemes`, check the scopes per call,
  and return the step-up result. That is additive, and waits for a
  consumer that needs it. Today every scope is endpoint-wide (`Scopes`),
  and a tool can still check `TokenInfoFromContext(ctx).Scopes` itself and
  return `IsError`.
- Resources, prompts, completions, `list_changed`, `structuredContent`,
  output schemas, and server-to-client requests.
- The `openai/profile` tool and other OpenAI `_meta` keys. An app can
  serve a profile tool as an ordinary tool; its `_meta` flag waits for a
  generic `_meta` seam.
- Stateful sessions, SSE responses and resumability (§6).

## Consequences

- The Dot plugin, or any app with `toolloop.Tool` values, serves them to
  ChatGPT and other MCP clients with `New`, `Handler`, a `TokenVerifier`
  for its identity provider, and the metadata handler mounted at
  `MetadataPath`.
- `mcpserver` and `mcpclient` share every translation rule, and a test
  runs the client against the server over the go-sdk in-memory transport.
  So the module's two halves keep each other honest.
- The audience rule is enforced by the package, not left to each app's
  verifier. A verifier must report the token's audiences: for a JWT, the
  `aud` claim; for introspection, the `aud` (or `resource`) field.
- An app's internal errors stay internal: the client sees a fixed message,
  and the app sees the error through `OnError`.
- The SDK's `RequireBearerToken` stays in the path for its context
  plumbing only. If a later go-sdk exports a way to set that context
  directly, the middleware can drop it without an API change.
- The first `mcp/v0.1.0` release contains both packages (ADR-0049 §1).
- No `MAESTRO_DIVERGENCES.md` row: Maestro has no MCP server built on this
  package to diverge from.

## Testing

Unit tests, no network and no keys:

- `internal/convert` (reverse): every row of §4, each invalid-result case,
  and a round trip through the client's direction for each row.
- `internal/convert` media rules: malformed media type (`"png"`,
  `"image/"`, parameters that don't parse), an image part with a
  non-image type, a document part with an image type, a type with
  parameters accepted.
- `New`: each invalid-tool case (names, duplicates, nil `Execute`,
  schemas), the SDK-only rejections (`"default": 1e1000`, an
  `x-mcp-header` on a non-primitive property, an empty or invalid header
  name, two properties with the same header name), proof that New
  returns errors rather than panicking and that only valid tools reach the
  real server, `Annotations` for an unknown tool, and each invalid `Auth`.
- Auth middleware, table-driven over `httptest`: no header, malformed
  header, `ErrInvalidToken`, another verifier error (500, fixed body,
  `OnError` called), empty subject, wrong audience, custom `Audience`,
  expired, skew, missing expiration with and without the opt-in, missing
  scope (403 and its header), success (`TokenInfoFromContext` inside
  `Execute`). Each challenge's exact `WWW-Authenticate` value is checked.
- `MetadataPath` and the metadata body, for a `Resource` with and without
  a path, and the CORS preflight.
- End to end: `mcpclient` against `Handler` over `httptest` with a bearer
  token in the client's `http.Client`: listing, a call's arguments and
  each result kind, `IsError`, non-object arguments, an `Execute` error
  (fixed message; `OnError` gets the original), client cancellation
  reaching `Execute`'s context, and what a dropped connection does (§3).
- stdio: the test binary re-executes itself as a `ServeStdio` server (the
  `TestMain` helper pattern from `mcpclient`), driven by `mcpclient.Command`.

## References

- Issue #181; ADR-0049 (module, client, reserved server shape), ADR-0011
  (Execute error vs `IsError`), ADR-0041 (tool-result parts), ADR-0047
  (toolboxes).
- MCP specification 2026-07-28, Authorization.
- RFC 6750 §3 (bearer challenges), RFC 8707 (resource indicators),
  RFC 9728 (protected resource metadata).
- OpenAI Apps SDK, "Authentication" (ChatGPT's requirements for MCP
  servers and authorization servers).
- go-sdk v1.8.0: `mcp.NewStreamableHTTPHandler`, `auth.RequireBearerToken`.
