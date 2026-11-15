# 0047. Tool loop toolboxes: `get_toolbox` loads named tool sets mid-run

- **Status:** Accepted
- **Date:** 2026-11-14

## Context

`toolloop.Run` fixes the offered tools once: `validateConfig` turns
`Config.Tools` into the request's definitions before the first turn
(ADR-0011). Model tool selection degrades as the offered set grows, and
every definition is sent on every turn. Morris has adopted a policy that
every user-facing feature must be usable through chat (morris ADR 0059), so
its chat tool count is growing quickly past the ~20 where that ADR names
toolboxes as the leading candidate. Maestro wants the same thing for its
coder's container tools. Both want the design Maestro agreed in a
2026-03-06 brainstorm, recorded in Morris's
`docs/design/toolbox-prior-art.md` and in issue #145: a small set of core
tools plus a `get_toolbox(name)` tool that loads a named specialty set
mid-run.

Only the loop can change the tool set between turns, so the mechanism
belongs here (Dan, 2026-11-04). The rest stays with the app: which boxes
exist, which tools a user may have, what the self-knowledge tool says
about them, and the loaded set between runs. Loading a box affects only
what the model is shown, never what it is allowed to do. In Morris every
tool still checks access on every call.

The design guidance on #145 shaped this ADR: boxes overlap, availability
is the union of what is loaded, a box is a complete task bundle, and the
cap and eviction rule are configurable hypotheses to test with consumers.

## Decision

### 1. API (additive to v1)

```go
type Config struct {
    // ... existing fields ...
    Toolboxes *Toolboxes // nil: today's behavior, every Config.Tools entry offered on every turn
}

type Toolboxes struct {
    Guidance string    // optional text appended to get_toolbox's description
    Core     []string  // names of Config.Tools entries always offered, never put away
    Boxes    []Toolbox // ordered: the order of get_toolbox's description
    Loaded   []string  // boxes loaded at start, least recently used first (normally a previous Outcome.LoadedToolboxes)
    MaxTools int       // cap on unique offered tools; 0 = DefaultMaxTools (20); negative = NoToolLimit
}

type Toolbox struct {
    Name        string
    Description string
    Tools       []string // names of Config.Tools entries
}

const DefaultMaxTools = 20
const NoToolLimit = -1

type Outcome struct {
    // ... existing fields ...
    LoadedToolboxes []string // loaded set when Run stopped, least recently used first; nil when Toolboxes is nil
}

type ToolCallEvent struct {
    // ... existing fields ...
    Unadvertised bool // the call named a declared tool missing from the turn's request (section 5)
}
```

With `Toolboxes` set, `Config.Tools` is the **single declaration** of every
tool: one definition, one executor. `Core` and `Toolbox.Tools` refer to
those tools by name, so a tool in several boxes, or in core and in a box,
cannot drift between copies. Names, not slice indexes: inserting into a
slice would silently change what every index means, and names make error
messages readable. `Boxes` is a slice, not a map, because its order fixes
the bytes of `get_toolbox`'s description.

`ChatClient`, `Tool` and `ToolResult` do not change. A nil `Toolboxes`
leaves Run's behavior exactly as it is today.

### 2. Validation (one time, before any provider call; `*ConfigError`)

With `Toolboxes` set, on top of today's rules:

- `Boxes` is non-empty. Box names are non-empty and unique. Each box has at
  least one tool, with no duplicates.
- Every name in `Core`, `Toolbox.Tools` and `Loaded` resolves, and none
  repeats within its list.
- Every `Config.Tools` entry is in `Core` or in at least one box. A tool in
  neither could never be offered.
- No `Config.Tools` entry is named `get_toolbox`, the reserved name.
- **Cap rule (fail early):** with a cap in force, for every box, the unique
  tools in core ∪ box, counting `get_toolbox`, must be at most `MaxTools`.
  This is the exact worst case: a box is always loadable whole, and the box
  being loaded never has to be trimmed or evicted. A set of boxes or a cap
  the app never meant would otherwise fail mid-conversation. The default
  cap of 20 is checked like an explicit one, so an app with a bigger box
  must set `MaxTools` itself.
- A forced `ToolChoice{Type: tool}` must name a core tool or `get_toolbox`.
  A forced box tool could be put away mid-run. `required` and `none` are
  allowed. `get_toolbox` is always offered, so `required` always has a tool.

### 3. Offered set and definition order

Each turn offers core ∪ every loaded box, deduplicated by name, plus
`get_toolbox`. The invariant: **every loaded box has all its tools offered.**
An unloaded box can still have some of its tools offered through core or
another loaded box. That is ordinary overlap, not a partly loaded box.

Definitions are sent in a fixed order: `get_toolbox` first, then the offered
tools in `Config.Tools` declaration order. Recency never reorders them, so
the same offered set always produces the same bytes. Tool definitions sit
at the front of the cacheable prompt, so any change to the offered set
invalidates the cache from there. The design keeps changes rare: boxes
accumulate, and nothing is put away until the cap requires it.

A `get_toolbox` call takes effect on the next request. The calls in a turn
were produced against that turn's request, so later calls in the same turn
are dispatched against it (section 5).

### 4. `get_toolbox` and eviction

The loop generates the tool. Its definition is:

- **Name:** `get_toolbox`.
- **Parameters:** one required string `name`, an `enum` of the box names in
  `Boxes` order.
- **Description:** a fixed sentence, then one line per box (`name: Description`),
  then `Guidance` if set.

Calls go through the normal execution path (section 5). `get_toolbox` never
sets `Stop`.

Recency is a logical clock owned by Run. `Loaded` seeds it in order, least
recent first. A box is marked used:

- when it is loaded, after any evictions, so a box just loaded is never the
  next one put away;
- when a turn's call executes a tool the box supplies, if the box is loaded
  and the tool is advertised in that turn's request and **not core**. A call
  to a core tool needs no box, so it says nothing about which box is in use.
  A call to a shared non-core tool marks every loaded box that supplies it.
  This rule is conservative, accepted for now.

Calls are stamped in turn order, then by position in the turn. Boxes
stamped by the same call are equally recent, and ties go to `Boxes`
order: the earlier box counts as less recent. Eviction and
`Outcome.LoadedToolboxes` use the same order, so the order survives into
the next run.

Loading box B:

1. **Unknown name, or arguments that don't parse:** a model-visible
   `IsError` listing the valid names. Nothing changes.
2. **Already loaded:** mark B used and report that it is already loaded.
   The offered set doesn't change.
3. **Otherwise:** add B. While a cap is in force and the unique offered set
   exceeds `MaxTools`, put away the least recently used loaded box other
   than B. Put-away removes the box's membership, and then the offered set
   is recomputed from core and the boxes still loaded. Tools that another
   loaded box or core still supplies stay offered. The cap rule (section 2)
   guarantees the loop ends before it reaches B.

The result is deterministic plain text. It names the loaded box and lists
the tools that became newly offered, each with its description. Tools that
were already offered are listed by name. If any boxes were put away, one
sentence names them, gives the reason (the cap), and lists the tools
actually **no longer offered**: tools that core or a remaining box still
supplies are left out. It also says the boxes can be loaded again. The
lost tools are not split per box, because in a cascade a tool can
disappear only once a second box goes. The list is never empty: eviction
stops only once the count drops. The result ends with the boxes now
loaded.

**Over-cap `Loaded` at start.** `Loaded` is stored state, so the app may
lower `MaxTools` after it was saved. If the starting set exceeds the cap,
the loop applies the same eviction from the least recent end until it fits.
The result shows in `Outcome.LoadedToolboxes`. No turn reports it, because
the tool definitions themselves tell the model what is offered. An unknown
or duplicate name in `Loaded` is still a `ConfigError`: the library can't
tell a stale name from a typo, and the app can filter against its own box
list.

### 5. Dispatch

Advertising decides what the model is shown. Declaration decides what can
execute.

| Call | Behavior |
|---|---|
| Declared (`Config.Tools`) and advertised in this turn's request | Execute; update recency (section 4). |
| Declared but not advertised | Execute; recency and the loaded set unchanged; `ToolCallEvent.Unadvertised = true`. |
| `get_toolbox` | The loop's loader (section 4). |
| Undeclared | Today's unknown-tool recovery: an `IsError` "not available" result. |

"Execute" is the one existing path: the same executor and observation hook,
the same result checks and error classification, cancellation and
terminal-tool rules. A call to an unadvertised tool does **not** load a box.
With overlapping boxes, one call can't say which box to load, and loading
one could put away tools the model still needs.

Honoring these calls matches what loading means: it only affects what the
model is shown. A call can reach an unadvertised tool only if the provider
returns it. If a provider then rejects the follow-up request, which holds a
call and result for a tool absent from `Tools`, the error reaches the
caller as `OutcomeLLMError`. The same history exists after any put-away,
whichever dispatch rule applies, so the live tests check it on every
adapter (section 7).

**Measured (2026-11-14, `livetest.ToolHistoryAfterRemoval`).** Every adapter
accepts the history: Anthropic direct and on Vertex, OpenAI, Gemini (API
and Vertex), Ollama, and `openaicompat` on LunaRoute. Model behavior varies.
Older Claude models sometimes distrust the result of a tool that is no
longer offered and answer that they lack the tool: Haiku 4.5 did in 1 to 2
of 5 tries per variant, and Opus 4.6 on Vertex once. Sonnet 5.5 and
Opus 5.5 used the result in 5 of 5 tries on every variant, although
Opus 5.5 remarked that the tool was not among its tools. The adapters do
not rewrite history to hide this. `get_toolbox`'s report of what was put
away is the in-transcript explanation, and the scenario asserts only that
the provider accepts the history.

**The loop does not validate arguments against the schema** (ADR-0011:
`Execute` gets the raw call). A call to an unadvertised tool may be built
from a remembered, older idea of the schema. Executors must validate
arguments before acting, whether or not the tool is loaded. The `Tool`
GoDoc will say so.

### 6. Interactions

- **Iterations.** A turn whose calls include `get_toolbox` is a tool-calling
  turn and counts against `MaxIterations`.
- **Terminal tools (ADR-0017).** Unchanged. A `Stop` earlier in a turn skips
  any later `get_toolbox` call, like any other call.
- **Compaction (ADR-0030).** The loaded set is loop state, not transcript,
  so compaction does not change it. After compaction the definitions sent
  each turn still say what is offered, even if old `get_toolbox` results
  were summarized away. `CompactInput` doesn't change.
- **Outcome.** `LoadedToolboxes` is set on every outcome after validation
  passes, including errors, and reflects loads executed so far. On a
  `ConfigError` it is nil.
- **Events.** `get_toolbox` calls emit `OnToolCall` like any call.
  `IterationEvent.Response` shows the request's effect. The offered set
  itself is not an event field.

### 7. Testing

- **Unit tests with `testllm`:** load, already loaded, unknown box, cap
  eviction with overlap (a shared tool survives when one box is put away),
  core never put away, the requested box protected, recency (load stamps,
  core calls don't, shared calls stamp all), declaration-order definitions
  across turns, the over-cap start, unadvertised execution and its event
  flag, a forced `ToolChoice` on a non-core tool rejected, every validation
  rule, and terminal and compaction interplay.
- **Live (`llms/internal/livetest`, every chat adapter):**
  - *ToolboxLoad*: the model must call `get_toolbox`, then a box tool, then
    answer. It asserts that sequence from the tool call events (a
    successful load, then an advertised call), not the final loaded set,
    which depends on what else the model calls or loads.
  - *ToolHistoryAfterRemoval*: a history holding a call and result for a
    tool absent from `Tools` must be accepted (§5), the state after a
    put-away. Unadvertised dispatch itself is loop logic, covered by the
    unit tests; on the wire it produces the same history shape.

  Any provider that rejects the replay is recorded here before acceptance.

## Consequences

- One mechanism serves Morris and Maestro. Apps keep the box catalog,
  permissions, self-knowledge content and the stored loaded set.
- Additive under the v1 stability contract. New fields and constants only;
  `ChatClient`, `Tool` and `ToolResult` do not change, and nil `Toolboxes`
  behaves as before.
- Committed for v1: the dispatch table (unadvertised declared tools
  execute), LRU with stamp-on-load and core calls excluded, the cap rule,
  the definition order, and the reserved name `get_toolbox`. A strict mode
  that refuses unadvertised calls, a pluggable eviction policy, or a
  search-based loader could be added later as optional fields.
- Loading a box costs a turn, and put-away invalidates the prompt cache.
  The default of 20 and the eviction rule are hypotheses to test against
  consumer workflows (the #145 guidance lists the measures).
- Executors must validate their own arguments. That was already the
  contract, and it matters more once remembered schemas can reach them.
- No `MAESTRO_DIVERGENCES.md` row: Maestro never built toolboxes (the
  design exists only as the brainstorm), so there is no behavior to
  diverge from.

## References

- Issue #145 and its design guidance comment; Morris
  `docs/design/toolbox-prior-art.md`; morris ADR 0059.
- ADR-0011 (tool loop scope, `Request.Tools` rule), ADR-0017 (terminal
  tools), ADR-0030 (compaction), ADR-0032 (enum-like types).
- #150 (MCP tools): servers with many tools are a main source of boxes.
