package llms

import (
	"reflect"
	"testing"
)

func reasoningPart(p ProviderID, data string) ContentPart {
	return ContentPart{Type: ContentReasoning, Reasoning: &Reasoning{Provider: p, ProviderData: []byte(data)}}
}

func reasoningTranscript() []Message {
	return []Message{
		UserText("q"),
		{Role: RoleAssistant, Content: []ContentPart{
			reasoningPart(ProviderAnthropic, "a1"),
			Text("calling"),
			{Type: ContentToolCall, ToolCall: &ToolCall{ID: "t1", Name: "f"}},
		}},
		ToolResultMessage(ToolResult{ToolCallID: "t1", Content: "r"}),
		{Role: RoleAssistant, Content: []ContentPart{
			reasoningPart(ProviderGoogle, "g1"),
			Text("answer"),
		}},
	}
}

func TestReasoningFromKeepsOwnDropsForeign(t *testing.T) {
	in := reasoningTranscript()
	snapshot := reasoningTranscript()

	got := ReasoningFrom(ProviderAnthropic, in)

	if !reflect.DeepEqual(in, snapshot) {
		t.Fatal("ReasoningFrom modified its input")
	}
	if len(got) != len(in) {
		t.Fatalf("message count = %d, want %d", len(got), len(in))
	}
	if !reflect.DeepEqual(got[1], in[1]) {
		t.Fatalf("own reasoning changed: %+v", got[1])
	}
	want3 := Message{Role: RoleAssistant, Content: []ContentPart{Text("answer")}}
	if !reflect.DeepEqual(got[3], want3) {
		t.Fatalf("foreign reasoning kept: %+v", got[3])
	}
}

func TestReasoningFromNoChangeReturnsInput(t *testing.T) {
	in := []Message{UserText("q"), {Role: RoleAssistant, Content: []ContentPart{reasoningPart(ProviderAnthropic, "a"), Text("x")}}}
	got := ReasoningFrom(ProviderAnthropic, in)
	if &got[0] != &in[0] {
		t.Fatal("expected the input slice back when nothing is dropped")
	}
}

func TestReasoningFromNilReasoningIsDropped(t *testing.T) {
	in := []Message{{Role: RoleAssistant, Content: []ContentPart{{Type: ContentReasoning}, Text("x")}}}
	got := ReasoningFrom(ProviderAnthropic, in)
	if len(got[0].Content) != 1 || got[0].Content[0].Type != ContentText {
		t.Fatalf("a reasoning part with nil Reasoning must be dropped: %+v", got[0])
	}
}

func TestWithoutReasoningRemovesAll(t *testing.T) {
	in := reasoningTranscript()
	got := WithoutReasoning(in)
	for i := range got {
		for _, p := range got[i].Content {
			if p.Type == ContentReasoning {
				t.Fatalf("message %d still has a reasoning part", i)
			}
		}
	}
	if len(got[1].Content) != 2 || len(got[3].Content) != 1 {
		t.Fatalf("non-reasoning parts changed: %+v", got)
	}
	if !reflect.DeepEqual(in, reasoningTranscript()) {
		t.Fatal("WithoutReasoning modified its input")
	}
}

func TestWithoutReasoningKeepsEmptiedMessage(t *testing.T) {
	in := []Message{UserText("q"), {Role: RoleAssistant, Content: []ContentPart{reasoningPart(ProviderAnthropic, "a")}}}
	got := WithoutReasoning(in)
	if len(got) != 2 || got[1].Role != RoleAssistant || len(got[1].Content) != 0 {
		t.Fatalf("emptied message must be kept with no content: %+v", got)
	}
}
