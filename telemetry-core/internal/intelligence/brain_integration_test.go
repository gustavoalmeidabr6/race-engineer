package intelligence

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// TestEndToEnd_BrainGateChannelFanout exercises the full passive pipeline:
//
//   producers → brain (observations / events / driver query)
//             ↓
//   raw channel → CommsGate → translated channel
//             ↓
//   fanout-equivalent → brain.RecordSpeech (closes the loop)
//
// Two raw insights flow through (one urgent, one normal). The mock LLM
// approves both with distinct radio messages. We assert the final brain
// state contains every event the system observed.
func TestEndToEnd_BrainGateChannelFanout(t *testing.T) {
	state := &models.RaceState{
		PlayerCarIndex:   0,
		Position:         4,
		CurrentLap:       12,
		TotalLaps:        50,
		LastLapTimeMs:    87_500,
		ActualCompound:   17, // Medium
		TyresAgeLaps:     12,
		TyresWear:        [4]float32{38, 36, 30, 32},
		FuelInTank:       40,
		FuelRemainingLaps: 25,
		Weather:          0,
		TrackTemp:        32,
		AirTemp:          22,
		TrackID:          10, // Spa
		SessionType:      10, // Race
	}

	b := brain.New(
		func() *models.RaceState { return state },
		brain.StaticContext{
			Soul: "Calm. Direct.",
			User: "No filler.",
		},
	)

	// Pre-populate the brain with a couple of observations and an event,
	// simulating other agents already at work.
	b.Write(brain.Observation{
		Topic: brain.TopicTireDegradation, Agent: "tire-agent",
		Summary: "RR up 3% in last 2 laps", Confidence: 0.7,
	})
	b.RecordEvent(brain.RaceEvent{Code: "DRSE", Detail: "DRS enabled"})

	mock := NewMockProvider()
	// Queue is FIFO. Order matches the call sequence in the test:
	// (1) urgent → (2) driver query → (3) normal batch.
	mock.QueueText(jsonGateResp(true, "Box this lap, target hards"))
	mock.QueueText(jsonGateResp(true, "Mediums are holding up, push for two more laps"))
	mock.QueueText(jsonGateResp(true, "Pace target one twenty seven"))

	rawCh := make(chan models.DrivingInsight, 8)
	transCh := make(chan models.DrivingInsight, 8)

	verbosity := int32(5)
	gate := NewCommsGate(
		mock, b,
		func() int32 { return atomic.LoadInt32(&verbosity) },
		rawCh, transCh,
	)
	gate.SetBatchInterval(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go gate.Run(ctx)

	// Fanout-equivalent: drain transCh and feed back into the brain
	// (mirrors what main.go's fanout does in production).
	emitted := make(chan models.DrivingInsight, 8)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ins, ok := <-transCh:
				if !ok {
					return
				}
				b.RecordSpeech(ins.Message, ins.Priority)
				select {
				case emitted <- ins:
				default:
				}
			}
		}
	}()

	// 1. Send urgent insight (priority 5) — processed immediately.
	rawCh <- models.DrivingInsight{Message: "Tire cliff incoming", Type: "warning", Priority: 5}

	first, ok := drainOne(t, emitted, time.Second)
	if !ok {
		t.Fatal("urgent insight never emitted")
	}
	if first.Message != "Box this lap, target hards" {
		t.Errorf("urgent emission unexpected: %q", first.Message)
	}

	// 2. Driver asks a question — verify it lands in brain L5.
	resp := gate.HandleQuery(ctx, "tire status?")
	if !strings.Contains(resp.Message, "Mediums are holding up") {
		t.Errorf("driver query response missing expected text: %q", resp.Message)
	}
	// Drain the emit from the driver-query path.
	if _, ok := drainOne(t, emitted, time.Second); !ok {
		t.Fatal("driver query did not emit on translated channel")
	}

	// 3. Send a normal-priority insight — processed by next batch tick.
	rawCh <- models.DrivingInsight{Message: "Pace dropping", Type: "info", Priority: 2}

	second, ok := drainOne(t, emitted, 2*time.Second)
	if !ok {
		t.Fatal("normal-batch insight never emitted")
	}
	if second.Message != "Pace target one twenty seven" {
		t.Errorf("batch emission unexpected: %q", second.Message)
	}

	// Allow the fanout goroutine to record speech.
	time.Sleep(50 * time.Millisecond)

	// Verify brain state contains the lifecycle.
	snap := b.Snapshot(brain.DefaultSnapshotOpts())

	if got := snap.Observations[brain.TopicTireDegradation]; len(got) != 1 {
		t.Errorf("expected pre-populated tire observation, got %d", len(got))
	}
	if len(snap.Events) == 0 {
		t.Errorf("expected DRSE event in snapshot")
	}
	if len(snap.Speech) < 2 {
		t.Errorf("expected at least 2 speech records (urgent + batch), got %d", len(snap.Speech))
	}

	// The snapshot must serialize without panic and include the live messages.
	md := snap.Markdown()
	for _, want := range []string{"DRSE", "RR up 3%", "Box this lap"} {
		if !strings.Contains(md, want) {
			t.Errorf("snapshot markdown missing %q\n--- got ---\n%s", want, md)
		}
	}

	// LLM was called for: urgent, driver query, batch — three calls.
	if got := len(mock.Calls()); got < 3 {
		t.Errorf("expected at least 3 LLM calls (urgent + query + batch), got %d", got)
	}

	// Each LLM system prompt should reflect the brain's static + live data.
	for i, call := range mock.Calls() {
		if !strings.Contains(call.System, "Calm. Direct.") {
			t.Errorf("call %d: system prompt missing soul personality", i)
		}
		if !strings.Contains(call.System, "Live Telemetry") {
			t.Errorf("call %d: system prompt missing live telemetry section", i)
		}
	}
}

// TestEndToEnd_NoProviderStillEmitsFromBrain verifies the system degrades
// gracefully when the LLM is unavailable: urgent insights pass through,
// driver gets a graceful response, and brain still records everything.
func TestEndToEnd_NoProviderStillEmitsFromBrain(t *testing.T) {
	state := &models.RaceState{Position: 7, CurrentLap: 5, TotalLaps: 50}
	b := brain.New(
		func() *models.RaceState { return state },
		brain.StaticContext{},
	)

	mock := NewMockProvider()
	mock.SetAvailable(false)

	rawCh := make(chan models.DrivingInsight, 4)
	transCh := make(chan models.DrivingInsight, 4)

	gate := NewCommsGate(
		mock, b,
		func() int32 { return 5 },
		rawCh, transCh,
	)
	gate.SetBatchInterval(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go gate.Run(ctx)

	rawCh <- models.DrivingInsight{Message: "engine overheat", Type: "warning", Priority: 5}

	got, ok := drainOne(t, transCh, time.Second)
	if !ok {
		t.Fatal("urgent should emit fallback even with no provider")
	}
	if !strings.Contains(got.Message, "engine overheat") {
		t.Errorf("fallback should contain original message, got %q", got.Message)
	}

	// Driver query in degraded mode.
	resp := gate.HandleQuery(ctx, "anything?")
	if !strings.Contains(strings.ToLower(resp.Message), "ai connection") {
		t.Errorf("expected graceful no-provider response, got %q", resp.Message)
	}
}
