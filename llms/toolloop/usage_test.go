package toolloop_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// usageWithAllInts returns a Usage whose every int field is set to v, using
// reflection so a field added to llms.Usage later is covered automatically.
func usageWithAllInts(t *testing.T, v int) llms.Usage {
	t.Helper()
	var u llms.Usage
	rv := reflect.ValueOf(&u).Elem()
	for i := range rv.NumField() {
		if f := rv.Field(i); f.Kind() == reflect.Int {
			f.SetInt(int64(v))
		}
	}
	return u
}

// TestRun_TotalUsageSumsEveryIntField guards #67: addUsage silently dropped
// ReasoningTokens and BillableOutputTokens, which ADR-0016 added to
// llms.Usage after the tool loop shipped.
func TestRun_TotalUsageSumsEveryIntField(t *testing.T) {
	client := scriptedResponses(
		toolCallResp("c1", echoToolName, `{}`, usageWithAllInts(t, 3)),
		finalTextResp("done", usageWithAllInts(t, 4)),
	)
	out := toolloop.Run(context.Background(), toolloop.Config{
		Client:  client,
		Request: userReq("go"),
		Tools:   []toolloop.Tool{echoTool()},
	})
	if out.Kind != toolloop.OutcomeFinalAnswer {
		t.Fatalf("kind = %q, want final_answer; err=%v", out.Kind, out.Err)
	}
	rv := reflect.ValueOf(out.TotalUsage)
	for i := range rv.NumField() {
		f := rv.Field(i)
		if f.Kind() != reflect.Int {
			continue
		}
		if got := f.Int(); got != 7 {
			t.Errorf("TotalUsage.%s = %d, want 7 (3+4): field not summed", rv.Type().Field(i).Name, got)
		}
	}
}
