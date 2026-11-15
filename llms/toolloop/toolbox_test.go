package toolloop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// Toolbox tests (ADR-0047). Tools are named by role: core tools c*, box
// tools by box letter, shared tools s*.

const loader = "get_toolbox"

// recTool is a tool that records each execution and returns "ran <name>".
func recTool(name string, ran *[]string) toolloop.Tool {
	return toolloop.Tool{
		Definition: llms.ToolDefinition{Name: name, Description: "Does " + name + ".", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Execute: func(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			if ran != nil {
				*ran = append(*ran, call.Name)
			}
			return toolloop.ToolResult{Content: "ran " + call.Name}, nil
		},
	}
}

func recTools(ran *[]string, names ...string) []toolloop.Tool {
	out := make([]toolloop.Tool, len(names))
	for i, n := range names {
		out[i] = recTool(n, ran)
	}
	return out
}

// callsResp is an assistant turn with several tool calls, in order. Each
// call is "name" or "name:json-args".
func callsResp(calls ...string) llms.ChatResponse {
	r := llms.ChatResponse{Message: llms.Message{Role: llms.RoleAssistant}, StopReason: "tool_use"}
	for i, c := range calls {
		name, args, ok := strings.Cut(c, ":")
		if !ok {
			args = "{}"
		}
		tc := llms.ToolCall{ID: fmt.Sprintf("call_%d_%s", i, name), Name: name, Parameters: json.RawMessage(args)}
		r.Message.Content = append(r.Message.Content, llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &tc})
		r.ToolCalls = append(r.ToolCalls, tc)
	}
	return r
}

func loadCall(box string) string { return loader + `:{"name":"` + box + `"}` }

// toolNames lists the tool names of each recorded request.
func toolNames(client *testllm.FakeChatClient) [][]string {
	var out [][]string
	for _, req := range client.Calls() {
		names := make([]string, len(req.Tools))
		for i := range req.Tools {
			names[i] = req.Tools[i].Name
		}
		out = append(out, names)
	}
	return out
}

// resultsOf returns the tool results the loop appended, in order.
func resultsOf(msgs []llms.Message) []*llms.ToolResult {
	var out []*llms.ToolResult
	for i := range msgs {
		for j := range msgs[i].Content {
			if p := &msgs[i].Content[j]; p.Type == llms.ContentToolResult {
				out = append(out, p.ToolResult)
			}
		}
	}
	return out
}

func box(name string, tools ...string) toolloop.Toolbox {
	return toolloop.Toolbox{Name: name, Description: "The " + name + " tools.", Tools: tools}
}

func runBoxes(t *testing.T, tools []toolloop.Tool, tb *toolloop.Toolboxes, resps ...llms.ChatResponse) (toolloop.Outcome, *testllm.FakeChatClient, []toolloop.ToolCallEvent) {
	t.Helper()
	client := scriptedResponses(resps...)
	var events []toolloop.ToolCallEvent
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:        client,
		Request:       userReq("go"),
		Tools:         tools,
		Toolboxes:     tb,
		MaxIterations: 10,
		OnToolCall:    func(e toolloop.ToolCallEvent) { events = append(events, e) },
	})
	return out, client, events
}

func TestToolboxes_ConfigValidation(t *testing.T) {
	tools := recTools(nil, "c1", "a1", "a2", "b1")
	valid := func() *toolloop.Toolboxes {
		return &toolloop.Toolboxes{
			Core:  []string{"c1"},
			Boxes: []toolloop.Toolbox{box("a", "a1", "a2"), box("b", "b1")},
		}
	}
	big := make([]string, 0, 20)
	for i := range 20 {
		big = append(big, fmt.Sprintf("t%02d", i))
	}
	cases := []struct {
		mutate func(*toolloop.Toolboxes, *[]toolloop.Tool, *llms.ToolChoice)
		name   string
		want   string // "" = valid
	}{
		{name: "valid", mutate: func(*toolloop.Toolboxes, *[]toolloop.Tool, *llms.ToolChoice) {}},
		{name: "reserved name", want: "reserved for the toolbox loader", mutate: func(tb *toolloop.Toolboxes, ts *[]toolloop.Tool, _ *llms.ToolChoice) {
			*ts = append(*ts, recTool(loader, nil))
			tb.Core = append(tb.Core, loader)
		}},
		{name: "unknown core", want: `Toolboxes.Core names unknown tool "zz"`, mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Core = append(tb.Core, "zz")
		}},
		{name: "duplicate core", want: `names tool "c1" twice`, mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Core = append(tb.Core, "c1")
		}},
		{name: "no boxes", want: "at least one toolbox", mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Boxes = nil
			tb.Core = []string{"c1", "a1", "a2", "b1"}
		}},
		{name: "empty box name", want: "Boxes[1].Name is empty", mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Boxes[1].Name = ""
		}},
		{name: "duplicate box", want: `duplicate toolbox name "a"`, mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Boxes[1].Name = "a"
		}},
		{name: "empty box", want: `toolbox "b" has no tools`, mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Boxes[1].Tools = nil
			tb.Core = append(tb.Core, "b1")
		}},
		{name: "unknown box tool", want: `toolbox "b" names unknown tool "zz"`, mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Boxes[1].Tools = append(tb.Boxes[1].Tools, "zz")
		}},
		{name: "duplicate box tool", want: `toolbox "a" names tool "a1" twice`, mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Boxes[0].Tools = append(tb.Boxes[0].Tools, "a1")
		}},
		{name: "unreachable tool", want: `tool "x1" is neither core nor in any toolbox`, mutate: func(_ *toolloop.Toolboxes, ts *[]toolloop.Tool, _ *llms.ToolChoice) {
			*ts = append(*ts, recTool("x1", nil))
		}},
		{name: "box over default cap", want: `toolbox "big" with the core tools and get_toolbox offers 22 tools, over the cap of 20`, mutate: func(tb *toolloop.Toolboxes, ts *[]toolloop.Tool, _ *llms.ToolChoice) {
			*ts = append(*ts, recTools(nil, big...)...)
			tb.Boxes = append(tb.Boxes, box("big", big...))
		}},
		{name: "box over explicit cap", want: `toolbox "a" with the core tools and get_toolbox offers 4 tools, over the cap of 3`, mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.MaxTools = 3
		}},
		{name: "cap counts core overlap once", mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Boxes[0].Tools = append(tb.Boxes[0].Tools, "c1") // c1 already core: a = {get_toolbox, c1, a1, a2}
			tb.MaxTools = 4
		}},
		{name: "no limit allows big box", mutate: func(tb *toolloop.Toolboxes, ts *[]toolloop.Tool, _ *llms.ToolChoice) {
			*ts = append(*ts, recTools(nil, big...)...)
			tb.Boxes = append(tb.Boxes, box("big", big...))
			tb.MaxTools = toolloop.NoToolLimit
		}},
		{name: "forced box tool", want: `ToolChoice names tool "a1", which is not core`, mutate: func(_ *toolloop.Toolboxes, _ *[]toolloop.Tool, tc *llms.ToolChoice) {
			*tc = llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "a1"}
		}},
		{name: "forced core tool", mutate: func(_ *toolloop.Toolboxes, _ *[]toolloop.Tool, tc *llms.ToolChoice) {
			*tc = llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "c1"}
		}},
		{name: "forced loader", mutate: func(_ *toolloop.Toolboxes, _ *[]toolloop.Tool, tc *llms.ToolChoice) {
			*tc = llms.ToolChoice{Type: llms.ToolChoiceTool, Name: loader}
		}},
		{name: "unknown loaded", want: `Toolboxes.Loaded names unknown toolbox "zz"`, mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Loaded = []string{"a", "zz"}
		}},
		{name: "duplicate loaded", want: `Toolboxes.Loaded contains duplicate toolbox "a"`, mutate: func(tb *toolloop.Toolboxes, _ *[]toolloop.Tool, _ *llms.ToolChoice) {
			tb.Loaded = []string{"a", "a"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tb := valid()
			ts := slices.Clone(tools)
			var choice llms.ToolChoice
			tc.mutate(tb, &ts, &choice)
			client := scriptedResponses(finalTextResp("done", llms.Usage{}))
			out := toolloop.Run(context.Background(), toolloop.Config{
				Client: client, Request: userReq("go"), Tools: ts, Toolboxes: tb, ToolChoice: choice,
			})
			if tc.want == "" {
				if out.Kind != toolloop.OutcomeFinalAnswer {
					t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
				}
				return
			}
			if out.Kind != toolloop.OutcomeToolError {
				t.Fatalf("kind = %q, want tool_error", out.Kind)
			}
			if got := outcomeReason(t, out); !strings.Contains(got, tc.want) {
				t.Fatalf("reason = %q, want it to contain %q", got, tc.want)
			}
			if out.LoadedToolboxes != nil {
				t.Errorf("LoadedToolboxes = %v on a rejected config, want nil", out.LoadedToolboxes)
			}
			if len(client.Calls()) != 0 {
				t.Errorf("provider called %d times on a rejected config", len(client.Calls()))
			}
		})
	}
}

func TestToolboxes_LoadThenUse(t *testing.T) {
	var ran []string
	tools := recTools(&ran, "a1", "c1", "a2", "b1") // declaration order: a1 before c1
	out, client, events := runBoxes(t, tools,
		&toolloop.Toolboxes{Core: []string{"c1"}, Boxes: []toolloop.Toolbox{box("a", "a1", "a2"), box("b", "b1")}},
		callsResp(loadCall("a")), callsResp("a2"), finalTextResp("done", llms.Usage{}))

	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
	}
	want := [][]string{{loader, "c1"}, {loader, "a1", "c1", "a2"}, {loader, "a1", "c1", "a2"}}
	if got := toolNames(client); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("offered tools per turn = %v, want %v", got, want)
	}
	if fmt.Sprint(ran) != "[a2]" {
		t.Errorf("executed = %v, want [a2]", ran)
	}
	res := resultsOf(out.Messages)[0]
	wantText := "Loaded toolbox \"a\".\nNew tools:\n- a1: Does a1.\n- a2: Does a2.\nLoaded toolboxes: a."
	if res.IsError || res.Content != wantText {
		t.Errorf("get_toolbox result = %q (IsError=%v), want %q", res.Content, res.IsError, wantText)
	}
	if fmt.Sprint(out.LoadedToolboxes) != "[a]" {
		t.Errorf("LoadedToolboxes = %v, want [a]", out.LoadedToolboxes)
	}
	for _, e := range events {
		if e.Unadvertised {
			t.Errorf("event for %q marked Unadvertised", e.Call.Name)
		}
	}
}

func TestToolboxes_LoaderDefinition(t *testing.T) {
	client := scriptedResponses(finalTextResp("done", llms.Usage{}))
	toolloop.Run(context.Background(), toolloop.Config{
		Client: client, Request: userReq("go"), Tools: recTools(nil, "a1", "b1"),
		Toolboxes: &toolloop.Toolboxes{
			Boxes:    []toolloop.Toolbox{box("files", "a1"), {Name: "bare", Tools: []string{"b1"}}},
			Guidance: "See the help topic for details.",
		},
	})
	def := client.Calls()[0].Tools[0]
	if def.Name != loader {
		t.Fatalf("first tool = %q, want %q", def.Name, loader)
	}
	for _, want := range []string{"\n- files: The files tools.", "\n- bare\n", "\nSee the help topic for details."} {
		if !strings.Contains(def.Description+"\n", want) {
			t.Errorf("description %q lacks %q", def.Description, want)
		}
	}
	var schema struct {
		Properties struct {
			Name struct {
				Enum []string `json:"enum"`
			} `json:"name"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if fmt.Sprint(schema.Properties.Name.Enum) != "[files bare]" || fmt.Sprint(schema.Required) != "[name]" {
		t.Errorf("schema = %s", def.InputSchema)
	}
}

func TestToolboxes_LoaderErrorsAndAlreadyLoaded(t *testing.T) {
	tools := recTools(nil, "a1", "b1")
	out, client, _ := runBoxes(t, tools,
		&toolloop.Toolboxes{Boxes: []toolloop.Toolbox{box("a", "a1"), box("b", "b1")}, Loaded: []string{"a", "b"}},
		callsResp(loadCall("nope"), loader+`:{"box":"a"}`, loader+`:not json`, loadCall("a")),
		finalTextResp("done", llms.Usage{}))

	res := resultsOf(out.Messages)
	if len(res) != 4 {
		t.Fatalf("got %d results, want 4", len(res))
	}
	if !res[0].IsError || !strings.Contains(res[0].Content, `unknown toolbox "nope". Toolboxes: a, b.`) {
		t.Errorf("unknown box result = %+v", res[0])
	}
	for _, r := range res[1:3] {
		if !r.IsError || !strings.Contains(r.Content, `get_toolbox needs {"name": "<toolbox>"}`) {
			t.Errorf("bad-args result = %+v", r)
		}
	}
	if res[3].IsError || res[3].Content != "Toolbox \"a\" is already loaded; its tools are available: a1.\nLoaded toolboxes: b, a." {
		t.Errorf("already-loaded result = %+v", res[3])
	}
	names := toolNames(client)
	if fmt.Sprint(names[0]) != fmt.Sprint(names[1]) {
		t.Errorf("offered set changed: %v -> %v", names[0], names[1])
	}
	// Already loaded still counts as use: a is now the most recent.
	if fmt.Sprint(out.LoadedToolboxes) != "[b a]" {
		t.Errorf("LoadedToolboxes = %v, want [b a]", out.LoadedToolboxes)
	}
}

// A shared tool survives when one of the boxes supplying it is put away, and
// the report names only the tools actually gone.
func TestToolboxes_EvictionKeepsSharedTools(t *testing.T) {
	tools := recTools(nil, "c1", "a1", "s1", "b1")
	out, client, _ := runBoxes(t, tools,
		&toolloop.Toolboxes{
			Core:     []string{"c1"},
			Boxes:    []toolloop.Toolbox{box("a", "a1", "s1"), box("b", "b1", "s1")},
			Loaded:   []string{"a"},
			MaxTools: 4, // get_toolbox + c1 + two box tools: a and b don't fit together
		},
		callsResp(loadCall("b")), finalTextResp("done", llms.Usage{}))

	want := [][]string{{loader, "c1", "a1", "s1"}, {loader, "c1", "s1", "b1"}}
	if got := toolNames(client); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("offered tools per turn = %v, want %v", got, want)
	}
	wantText := "Loaded toolbox \"b\".\nNew tools:\n- b1: Does b1.\nAlready available: s1.\n" +
		"Put away toolbox \"a\" to stay within 4 tools; no longer available: a1. Call get_toolbox to load one again.\n" +
		"Loaded toolboxes: b."
	if got := resultsOf(out.Messages)[0].Content; got != wantText {
		t.Errorf("result =\n%s\nwant\n%s", got, wantText)
	}
}

func TestToolboxes_EvictionCascade(t *testing.T) {
	tools := recTools(nil, "s1", "s2", "b1")
	out, _, _ := runBoxes(t, tools,
		&toolloop.Toolboxes{
			Boxes:    []toolloop.Toolbox{box("a", "s1"), box("b", "s1", "s2"), box("c", "b1")},
			Loaded:   []string{"a", "b"},
			MaxTools: 3, // get_toolbox + two tools
		},
		callsResp(loadCall("c")), finalTextResp("done", llms.Usage{}))
	got := resultsOf(out.Messages)[0].Content
	// a is least recent; putting it away frees nothing (b supplies s1), so b
	// goes too.
	want := "Loaded toolbox \"c\".\nNew tools:\n- b1: Does b1.\n" +
		"Put away toolboxes \"a\", \"b\" to stay within 3 tools; no longer available: s1, s2. Call get_toolbox to load one again.\n" +
		"Loaded toolboxes: c."
	if got != want {
		t.Errorf("result =\n%s\nwant\n%s", got, want)
	}
}

// Least recently used, not first loaded: a call to an old box's tool keeps
// it, and the requested box is never the one put away.
func TestToolboxes_EvictsLeastRecentlyUsed(t *testing.T) {
	tools := recTools(nil, "a1", "b1", "d1")
	tb := func() *toolloop.Toolboxes {
		return &toolloop.Toolboxes{
			Boxes:    []toolloop.Toolbox{box("a", "a1"), box("b", "b1"), box("d", "d1")},
			Loaded:   []string{"a", "b"},
			MaxTools: 3, // get_toolbox + two boxes
		}
	}
	// FIFO would put away a; a1's call made a the most recent.
	out, _, _ := runBoxes(t, tools, tb(), callsResp("a1"), callsResp(loadCall("d")), finalTextResp("done", llms.Usage{}))
	if fmt.Sprint(out.LoadedToolboxes) != "[a d]" {
		t.Errorf("after using a: LoadedToolboxes = %v, want [a d]", out.LoadedToolboxes)
	}
	// Without the call, a is least recent.
	out, _, _ = runBoxes(t, tools, tb(), callsResp(loadCall("d")), finalTextResp("done", llms.Usage{}))
	if fmt.Sprint(out.LoadedToolboxes) != "[b d]" {
		t.Errorf("no use: LoadedToolboxes = %v, want [b d]", out.LoadedToolboxes)
	}
}

// A call to a core tool marks no box, even a box that also lists it.
func TestToolboxes_CoreCallsDoNotMarkUse(t *testing.T) {
	tools := recTools(nil, "c1", "a1", "b1", "d1")
	out, _, _ := runBoxes(t, tools,
		&toolloop.Toolboxes{
			Core:     []string{"c1"},
			Boxes:    []toolloop.Toolbox{box("a", "a1", "c1"), box("b", "b1"), box("d", "d1")},
			Loaded:   []string{"a", "b"},
			MaxTools: 4, // get_toolbox + c1 + two box tools
		},
		callsResp("c1"), callsResp(loadCall("d")), finalTextResp("done", llms.Usage{}))
	if fmt.Sprint(out.LoadedToolboxes) != "[b d]" {
		t.Errorf("LoadedToolboxes = %v, want [b d] (a put away despite the c1 call)", out.LoadedToolboxes)
	}
}

// A call to a shared non-core tool marks every loaded box that supplies it.
func TestToolboxes_SharedCallMarksEverySupplier(t *testing.T) {
	tools := recTools(nil, "a1", "b1", "s1", "d1", "e1")
	out, _, _ := runBoxes(t, tools,
		&toolloop.Toolboxes{
			Boxes:    []toolloop.Toolbox{box("a", "a1", "s1"), box("b", "b1", "s1"), box("d", "d1"), box("e", "e1")},
			Loaded:   []string{"a", "b", "d"},
			MaxTools: 5, // get_toolbox + a1, b1, s1, d1
		},
		callsResp("s1"), callsResp(loadCall("e")), finalTextResp("done", llms.Usage{}))
	if fmt.Sprint(out.LoadedToolboxes) != "[a b e]" {
		t.Errorf("LoadedToolboxes = %v, want [a b e] (d put away)", out.LoadedToolboxes)
	}
}

// Stored state over a lowered cap: the least recently used boxes are put
// away before the first turn.
func TestToolboxes_LoadedOverCapAtStart(t *testing.T) {
	tools := recTools(nil, "a1", "b1", "d1")
	out, client, _ := runBoxes(t, tools,
		&toolloop.Toolboxes{
			Boxes:    []toolloop.Toolbox{box("a", "a1"), box("b", "b1"), box("d", "d1")},
			Loaded:   []string{"a", "b", "d"},
			MaxTools: 3,
		},
		finalTextResp("done", llms.Usage{}))
	if got := toolNames(client)[0]; fmt.Sprint(got) != fmt.Sprint([]string{loader, "b1", "d1"}) {
		t.Errorf("first turn offered %v", got)
	}
	if fmt.Sprint(out.LoadedToolboxes) != "[b d]" {
		t.Errorf("LoadedToolboxes = %v, want [b d]", out.LoadedToolboxes)
	}
}

// Declared but unadvertised tools execute without loading anything;
// undeclared ones get the unknown-tool result.
func TestToolboxes_UnadvertisedCall(t *testing.T) {
	var ran []string
	tools := recTools(&ran, "c1", "a1")
	out, client, events := runBoxes(t, tools,
		&toolloop.Toolboxes{Core: []string{"c1"}, Boxes: []toolloop.Toolbox{box("a", "a1")}},
		callsResp("a1", "ghost"), finalTextResp("done", llms.Usage{}))

	if fmt.Sprint(ran) != "[a1]" {
		t.Fatalf("executed = %v, want [a1]", ran)
	}
	if len(events) != 2 || !events[0].Unadvertised || events[1].Unadvertised {
		t.Fatalf("events = %+v, want a1 Unadvertised and ghost not", events)
	}
	res := resultsOf(out.Messages)
	if res[0].IsError || res[0].Content != "ran a1" {
		t.Errorf("a1 result = %+v", res[0])
	}
	if !res[1].IsError || res[1].Content != `tool "ghost" is not available` {
		t.Errorf("ghost result = %+v", res[1])
	}
	if len(out.LoadedToolboxes) != 0 {
		t.Errorf("LoadedToolboxes = %v, want none (an unadvertised call loads nothing)", out.LoadedToolboxes)
	}
	if got := toolNames(client)[1]; fmt.Sprint(got) != fmt.Sprint([]string{loader, "c1"}) {
		t.Errorf("second turn offered %v, want unchanged", got)
	}
}

// Calls are dispatched against the request that produced them: after a
// get_toolbox call puts box a away mid-turn, a later a1 call in the same
// turn was still advertised.
func TestToolboxes_MidTurnLoad(t *testing.T) {
	var ran []string
	tools := recTools(&ran, "a1", "b1")
	out, client, events := runBoxes(t, tools,
		&toolloop.Toolboxes{Boxes: []toolloop.Toolbox{box("a", "a1"), box("b", "b1")}, Loaded: []string{"a"}, MaxTools: 2},
		callsResp(loadCall("b"), "a1"), finalTextResp("done", llms.Usage{}))

	if fmt.Sprint(ran) != "[a1]" {
		t.Fatalf("executed = %v, want [a1]", ran)
	}
	if events[1].Unadvertised {
		t.Error("a1 marked Unadvertised, but the turn's request offered it")
	}
	if fmt.Sprint(out.LoadedToolboxes) != "[b]" {
		t.Errorf("LoadedToolboxes = %v, want [b] (the a1 call does not reload a)", out.LoadedToolboxes)
	}
	if got := toolNames(client)[1]; fmt.Sprint(got) != fmt.Sprint([]string{loader, "b1"}) {
		t.Errorf("next turn offered %v", got)
	}
}

// A terminal tool in a box ends the run as usual, and the outcome carries
// the loaded set; a Stop earlier in the turn skips a later get_toolbox.
func TestToolboxes_TerminalTool(t *testing.T) {
	submit := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "submit"},
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{Content: "ok", Stop: true}, nil
		},
	}
	tools := append(recTools(nil, "b1"), submit)
	out, _, _ := runBoxes(t, tools,
		&toolloop.Toolboxes{Boxes: []toolloop.Toolbox{box("a", "submit"), box("b", "b1")}, Loaded: []string{"a"}},
		callsResp("submit", loadCall("b")))
	if out.Kind != toolloop.OutcomeTerminalTool {
		t.Fatalf("kind = %q, want terminal_tool; err=%v", out.Kind, out.Err)
	}
	if fmt.Sprint(out.LoadedToolboxes) != "[a]" {
		t.Errorf("LoadedToolboxes = %v, want [a] (the get_toolbox call after Stop is not executed)", out.LoadedToolboxes)
	}
}

// Compaction rewrites the transcript, not the loaded set.
func TestToolboxes_SurviveCompaction(t *testing.T) {
	tools := recTools(nil, "a1")
	client := scriptedResponses(callsResp(loadCall("a")), finalTextResp("done", llms.Usage{}))
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client: client, Request: userReq("go"), Tools: tools,
		Toolboxes: &toolloop.Toolboxes{Boxes: []toolloop.Toolbox{box("a", "a1")}},
		Compact: func(_ context.Context, in toolloop.CompactInput) (toolloop.CompactResult, error) {
			if len(in.Messages) <= 1 {
				return toolloop.CompactResult{}, nil
			}
			return toolloop.CompactResult{Messages: []llms.Message{llms.UserText("summary: loaded toolbox a")}}, nil
		},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer || out.Compactions != 1 {
		t.Fatalf("kind = %q compactions = %d; err=%v", out.Kind, out.Compactions, out.Err)
	}
	if got := toolNames(client)[1]; fmt.Sprint(got) != fmt.Sprint([]string{loader, "a1"}) {
		t.Errorf("after compaction offered %v, want a still loaded", got)
	}
}

// Without Toolboxes nothing changes: a tool may be named get_toolbox, every
// tool is offered, and the new outputs stay zero.
func TestToolboxes_NilIsUnchanged(t *testing.T) {
	var ran []string
	tools := recTools(&ran, loader, "a1")
	out, client, events := runBoxes(t, tools, nil, callsResp(loader), finalTextResp("done", llms.Usage{}))
	if fmt.Sprint(ran) != "[get_toolbox]" {
		t.Fatalf("executed = %v, want the app's own get_toolbox", ran)
	}
	if got := toolNames(client)[0]; fmt.Sprint(got) != fmt.Sprint([]string{loader, "a1"}) {
		t.Errorf("offered %v", got)
	}
	if out.LoadedToolboxes != nil || events[0].Unadvertised {
		t.Errorf("LoadedToolboxes = %v, Unadvertised = %v; want zero values", out.LoadedToolboxes, events[0].Unadvertised)
	}
}

// LoadedToolboxes is set on error outcomes too, reflecting loads so far.
func TestToolboxes_LoadedOnErrorOutcome(t *testing.T) {
	failing := toolloop.Tool{
		Definition: llms.ToolDefinition{Name: "boom"},
		Execute: func(context.Context, llms.ToolCall) (toolloop.ToolResult, error) {
			return toolloop.ToolResult{}, fmt.Errorf("exploded")
		},
	}
	out, _, _ := runBoxes(t, []toolloop.Tool{failing},
		&toolloop.Toolboxes{Boxes: []toolloop.Toolbox{box("a", "boom")}},
		callsResp(loadCall("a"), "boom"))
	if out.Kind != toolloop.OutcomeToolError {
		t.Fatalf("kind = %q, want tool_error", out.Kind)
	}
	if fmt.Sprint(out.LoadedToolboxes) != "[a]" {
		t.Errorf("LoadedToolboxes = %v, want [a]", out.LoadedToolboxes)
	}
}
