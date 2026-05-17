package voice

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/genai"
)

// A GoAway frame must propagate as ErrLiveGoAway so the supervisor in
// api/live_ws.go can classify the Run() exit as an expected rotation
// (resets give-up window, reconnects immediately with resume handle).
// Without this, GoAway was logged-and-ignored, the server force-closed
// with a 1008 policy violation, and the supervisor's 60s give-up budget
// burnt through during reconnect attempts.
func TestHandleServerMessage_GoAwayReturnsSentinel(t *testing.T) {
	a := &LiveAgent{}
	msg := &genai.LiveServerMessage{
		GoAway: &genai.LiveServerGoAway{TimeLeft: 0},
	}
	err := a.handleServerMessage(context.Background(), nil, msg)
	if !errors.Is(err, ErrLiveGoAway) {
		t.Fatalf("expected ErrLiveGoAway, got %v", err)
	}
}

// SessionResumptionUpdate frames with Resumable=true must capture the
// handle so the next Run() Connect can resume the conversation. Without
// this, every GoAway rotation would restart from scratch and lose all
// prior driver/engineer turns + tool-call memory.
func TestHandleServerMessage_CapturesResumptionHandle(t *testing.T) {
	a := &LiveAgent{}
	msg := &genai.LiveServerMessage{
		SessionResumptionUpdate: &genai.LiveServerSessionResumptionUpdate{
			NewHandle: "handle-abc-123",
			Resumable: true,
		},
	}
	if err := a.handleServerMessage(context.Background(), nil, msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := a.resumeHandle.Load()
	if got == nil || *got != "handle-abc-123" {
		t.Fatalf("expected resumeHandle=handle-abc-123, got %v", got)
	}
}

// Non-resumable updates must be ignored — replaying them on the next
// Connect would produce a server-side rejection.
func TestHandleServerMessage_IgnoresNonResumableUpdate(t *testing.T) {
	a := &LiveAgent{}
	msg := &genai.LiveServerMessage{
		SessionResumptionUpdate: &genai.LiveServerSessionResumptionUpdate{
			NewHandle: "handle-not-usable",
			Resumable: false,
		},
	}
	if err := a.handleServerMessage(context.Background(), nil, msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := a.resumeHandle.Load(); got != nil {
		t.Fatalf("expected nil resumeHandle, got %v", *got)
	}
}
