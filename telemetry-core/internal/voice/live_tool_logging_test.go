package voice

import (
	"strings"
	"testing"
	"time"
)

// TestPreviewArgs_TruncatesLong — long JSON args (e.g. an ask_data_analyst
// question) are capped so a single log line stays readable.
func TestPreviewArgs_TruncatesLong(t *testing.T) {
	long := strings.Repeat("X", 500)
	got := previewArgs(map[string]any{"question": long})
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation ellipsis, got tail %q", got[len(got)-10:])
	}
	if len(got) > 250 {
		t.Errorf("preview should stay under ~250 chars, got %d", len(got))
	}
}

// TestPreviewArgs_EmptyAndNil — both nil and empty map render as "{}".
func TestPreviewArgs_EmptyAndNil(t *testing.T) {
	if got := previewArgs(nil); got != "{}" {
		t.Errorf("nil → %q, want {}", got)
	}
	if got := previewArgs(map[string]any{}); got != "{}" {
		t.Errorf("empty → %q, want {}", got)
	}
}

// TestPreviewArgs_PreservesShortContent — short args round-trip verbatim
// so the log line shows the exact values the model sent.
func TestPreviewArgs_PreservesShortContent(t *testing.T) {
	got := previewArgs(map[string]any{"priority": 4, "summary": "box this lap"})
	if !strings.Contains(got, `"summary":"box this lap"`) {
		t.Errorf("missing literal summary: %q", got)
	}
	if !strings.Contains(got, `"priority":4`) {
		t.Errorf("missing literal priority: %q", got)
	}
}

// TestResultByteSize — empty payloads count as 0; populated ones return
// a serialised byte length (rough match to what would land in the
// FunctionResponse envelope).
func TestResultByteSize(t *testing.T) {
	if got := resultByteSize(nil); got != 0 {
		t.Errorf("nil → %d, want 0", got)
	}
	if got := resultByteSize(map[string]any{}); got != 2 { // "{}"
		t.Errorf("empty → %d, want 2", got)
	}
	if got := resultByteSize(map[string]any{"text": "hi"}); got <= 8 {
		t.Errorf("populated → %d, expected >8", got)
	}
}

// TestEmitToolEvent_BroadcastsAndDropsWhenFull — successful sends land on
// the channel; once the buffer is full further calls drop rather than
// block.
func TestEmitToolEvent_BroadcastsAndDropsWhenFull(t *testing.T) {
	ch := make(chan ToolEvent, 2)
	a := &LiveAgent{cfg: LiveAgentConfig{ToolEvents: ch}}

	a.emitToolEvent(ToolEvent{Kind: "tool_call", Name: "get_race_state"})
	a.emitToolEvent(ToolEvent{Kind: "tool_result", Name: "get_race_state", ElapsedMs: 12})
	// Third one — buffer full, must not block or panic.
	a.emitToolEvent(ToolEvent{Kind: "tool_call", Name: "ask_data_analyst"})

	select {
	case ev := <-ch:
		if ev.Kind != "tool_call" || ev.Name != "get_race_state" {
			t.Errorf("first event mismatch: %+v", ev)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected first event")
	}
	select {
	case ev := <-ch:
		if ev.Kind != "tool_result" || ev.ElapsedMs != 12 {
			t.Errorf("second event mismatch: %+v", ev)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected second event")
	}
	// Third event was dropped — channel must be empty now.
	select {
	case ev := <-ch:
		t.Errorf("expected dropped 3rd event, got %+v", ev)
	case <-time.After(20 * time.Millisecond):
		// good — drop path worked
	}
}

// TestEmitToolEvent_NilChannelIsNoOp — when ToolEvents isn't wired the
// emit path silently returns. Critical because tests + headless setups
// commonly leave it nil.
func TestEmitToolEvent_NilChannelIsNoOp(t *testing.T) {
	a := &LiveAgent{cfg: LiveAgentConfig{}}
	// Must not panic.
	a.emitToolEvent(ToolEvent{Kind: "tool_call", Name: "get_race_state"})
}
