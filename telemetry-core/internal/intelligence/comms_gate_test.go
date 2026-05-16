package intelligence

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// --- helpers ---

func mockState() *models.RaceState {
	return &models.RaceState{
		PlayerCarIndex:   2,
		Speed:            287,
		Gear:             6,
		EngineRPM:        11200,
		Position:         3,
		GridPosition:     5,
		CurrentLap:       18,
		TotalLaps:        56,
		LastLapTimeMs:    84_300,
		DeltaToFrontMs:   2100,
		DeltaToLeaderMs:  9700,
		ActualCompound:   16, // soft
		TyresAgeLaps:     14,
		TyresWear:        [4]float32{42, 38, 31, 33},
		FuelInTank:       33.5,
		FuelRemainingLaps: 19.5,
		FuelMix:          1,
		ERSStoreEnergy:   2_400_000,
		ERSDeployMode:    1,
		Weather:          1,
		TrackTemp:        37,
		AirTemp:          24,
		RainPercentage:   5,
		TrackID:          7, // Silverstone
		SessionType:      10, // Race
		PitWindowIdeal:   22,
		PitWindowLatest:  30,
	}
}

func newTestBrain(t *testing.T) *brain.RaceBrain {
	t.Helper()
	state := mockState()
	b := brain.New(
		func() *models.RaceState { return state },
		brain.StaticContext{
			Soul:          "Calm under pressure. Direct.",
			User:          "Driver hates filler. Use position numbers.",
			DriverProfile: "Aggressive on entry, manages tires well in race.",
			TrackSetup:    "Silverstone — high-speed sweepers, rear-tire limited.",
			PastLearnings: "Last race RR cliff at lap 25 on softs.",
		},
	)
	return b
}

func newTestGate(t *testing.T, b *brain.RaceBrain, mock *MockProvider) (*CommsGate, chan models.DrivingInsight, chan models.DrivingInsight) {
	t.Helper()
	rawCh := make(chan models.DrivingInsight, 16)
	transCh := make(chan models.DrivingInsight, 16)
	verbosity := int32(5)
	g := NewCommsGate(
		mock,
		b,
		func() int32 { return atomic.LoadInt32(&verbosity) },
		rawCh,
		transCh,
	)
	g.SetBatchInterval(50 * time.Millisecond)
	return g, rawCh, transCh
}

// jsonGateResp builds a JSON string the LLM would emit.
func jsonGateResp(communicate bool, voice string) string {
	if communicate {
		return `{"communicate_to_driver":true,"voice_text":"` + voice + `","analyst_input_needed":false,"analyst_prompt":"NONE"}`
	}
	return `{"communicate_to_driver":false,"voice_text":"","analyst_input_needed":false,"analyst_prompt":"NONE"}`
}

func drainOne(t *testing.T, ch <-chan models.DrivingInsight, timeout time.Duration) (models.DrivingInsight, bool) {
	t.Helper()
	select {
	case ins := <-ch:
		return ins, true
	case <-time.After(timeout):
		return models.DrivingInsight{}, false
	}
}

// --- tests ---

func TestGateConfigPromptIncludesAllSnapshotLayers(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	g, _, _ := newTestGate(t, b, mock)

	b.Write(brain.Observation{
		Topic: brain.TopicTireDegradation, Agent: "analyst",
		Summary: "RR wearing 0.4%/lap", Confidence: 0.8,
	})
	b.RecordSpeech("DRS available", 3)
	b.RecordEvent(brain.RaceEvent{Code: "SCDP", Detail: "deployed"})
	b.RecordDriver("how are tires?", "query")

	system, _ := g.gateConfig(false)

	wants := []string{
		"Calm under pressure",        // soul
		"Driver hates filler",        // user prefs
		"Silverstone",                // track from BuildContext (track_id 7)
		"Soft",                       // tire compound (ActualCompound 16)
		"Active Observations",
		"tires.degradation",
		"RR wearing 0.4%/lap",
		"Recent Race Events",
		"SCDP",
		"Recent Radio",
		"DRS available",
		"Driver State",
		"how are tires?",
		"Driver Profile",
		"Aggressive on entry",
		"Track Setup",
		"high-speed sweepers",
		"Past Learnings",
		"RR cliff at lap 25",
		"VERBOSITY INSTRUCTION",
	}
	for _, w := range wants {
		if !strings.Contains(system, w) {
			t.Errorf("system prompt missing %q", w)
		}
	}
}

func TestUrgentInsightEmitsTranslatedRadio(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	mock.QueueText(jsonGateResp(true, "Box box, scenario two"))

	g, rawCh, transCh := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	rawCh <- models.DrivingInsight{Message: "tire cliff imminent", Type: "warning", Priority: 5}

	got, ok := drainOne(t, transCh, time.Second)
	if !ok {
		t.Fatal("expected emitted insight, got nothing")
	}
	if got.Message != "Box box, scenario two" {
		t.Errorf("want translated message, got %q", got.Message)
	}
	if got.Priority != 4 {
		t.Errorf("urgent should emit at priority 4, got %d", got.Priority)
	}
}

func TestUrgentFallsBackWhenProviderUnavailable(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	mock.SetAvailable(false)

	g, rawCh, transCh := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	rawCh <- models.DrivingInsight{Message: "engine overheating", Type: "warning", Priority: 5}

	got, ok := drainOne(t, transCh, time.Second)
	if !ok {
		t.Fatal("expected fallback emission")
	}
	if !strings.HasPrefix(got.Message, "Team:") {
		t.Errorf("unavailable provider should pass through with Team: prefix, got %q", got.Message)
	}
}

func TestUrgentFallsBackOnLLMError(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	mock.QueueError(errors.New("network down"))

	g, rawCh, transCh := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	rawCh <- models.DrivingInsight{Message: "rear wing damage", Type: "warning", Priority: 5}

	got, ok := drainOne(t, transCh, time.Second)
	if !ok {
		t.Fatal("expected fallback after LLM error")
	}
	if !strings.HasPrefix(got.Message, "Team:") {
		t.Errorf("LLM error should fall back to pass-through, got %q", got.Message)
	}
}

func TestNormalBatchEmitsWhenLLMApproves(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	mock.QueueText(jsonGateResp(true, "Pace target one twenty four flat"))

	g, rawCh, transCh := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	rawCh <- models.DrivingInsight{Message: "slow pace", Type: "info", Priority: 2}

	got, ok := drainOne(t, transCh, time.Second)
	if !ok {
		t.Fatal("expected batch emission")
	}
	if got.Message != "Pace target one twenty four flat" {
		t.Errorf("unexpected message: %q", got.Message)
	}
}

func TestNormalBatchHoldsWhenLLMDeclines(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	mock.QueueText(jsonGateResp(false, ""))

	g, rawCh, transCh := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	rawCh <- models.DrivingInsight{Message: "irrelevant noise", Type: "info", Priority: 2}

	if _, ok := drainOne(t, transCh, 250*time.Millisecond); ok {
		t.Fatal("LLM declined; nothing should be emitted")
	}
	if len(mock.Calls()) == 0 {
		t.Fatal("expected at least one LLM call")
	}
}

func TestNormalBatchPassesThroughWhenProviderUnavailable(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	mock.SetAvailable(false)

	g, rawCh, transCh := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	rawCh <- models.DrivingInsight{Message: "tire pressure low", Type: "warning", Priority: 2}
	rawCh <- models.DrivingInsight{Message: "fuel mix lean", Type: "info", Priority: 1}

	got, ok := drainOne(t, transCh, time.Second)
	if !ok {
		t.Fatal("expected fallback batch emission")
	}
	if !strings.HasPrefix(got.Message, "Team:") {
		t.Errorf("offline batch should use Team: prefix, got %q", got.Message)
	}
	if !strings.Contains(got.Message, "tire pressure low") || !strings.Contains(got.Message, "fuel mix lean") {
		t.Errorf("fallback should join all queued messages, got %q", got.Message)
	}
}

func TestStrategyInsightTakesTranslateBypassAfterOneCycle(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	// Two batch ticks: tick 1 ages the strategy item to 1, tick 2 routes it to translateAndEmit.
	mock.QueueText(jsonGateResp(true, "Box now, target hards"))

	g, rawCh, transCh := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	rawCh <- models.DrivingInsight{Message: "Pit recommendation lap 22", Type: "strategy", Priority: 3}

	got, ok := drainOne(t, transCh, 2*time.Second)
	if !ok {
		t.Fatal("expected strategy insight to emit via translation")
	}
	if got.Message != "Box now, target hards" {
		t.Errorf("strategy bypass should use translated text, got %q", got.Message)
	}
	if got.Priority != 3 {
		t.Errorf("strategy bypass should preserve priority, got %d", got.Priority)
	}
}

func TestDriverQueryReturnsLLMResponse(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	mock.QueueText(jsonGateResp(true, "Tires are good — eight laps left in them"))

	g, _, transCh := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	resp := g.HandleQuery(ctx, "how are my tires?")
	if resp.Message != "Tires are good — eight laps left in them" {
		t.Errorf("driver query response unexpected: %q", resp.Message)
	}
	if resp.Priority != 4 {
		t.Errorf("driver query reply should be priority 4, got %d", resp.Priority)
	}

	got, ok := drainOne(t, transCh, time.Second)
	if !ok {
		t.Fatal("driver query should also emit on translatedChan")
	}
	if got.Message != resp.Message {
		t.Errorf("emitted vs returned mismatch: %q vs %q", got.Message, resp.Message)
	}

	last, _ := mock.LastCall()
	if !strings.Contains(last.Prompt, "how are my tires?") {
		t.Errorf("LLM prompt should contain the driver question, got %q", last.Prompt)
	}
}

func TestDriverQueryHandlesNoTelemetry(t *testing.T) {
	b := brain.New(func() *models.RaceState { return nil }, brain.StaticContext{})
	mock := NewMockProvider()
	g, _, _ := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	resp := g.HandleQuery(ctx, "any data?")
	if !strings.Contains(strings.ToLower(resp.Message), "telemetry") {
		t.Errorf("expected no-telemetry message, got %q", resp.Message)
	}
	if len(mock.Calls()) != 0 {
		t.Errorf("LLM should not be called when there's no telemetry, got %d calls", len(mock.Calls()))
	}
}

func TestDriverQueryHandlesProviderUnavailable(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()
	mock.SetAvailable(false)

	g, _, _ := newTestGate(t, b, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	resp := g.HandleQuery(ctx, "fuel level?")
	if !strings.Contains(strings.ToLower(resp.Message), "ai connection") {
		t.Errorf("expected provider-unavailable message, got %q", resp.Message)
	}
}

func TestVerbosityInstructionAcrossLevels(t *testing.T) {
	b := newTestBrain(t)
	mock := NewMockProvider()

	cases := []struct {
		level int32
		want  string
	}{
		{1, "EXTREMELY brief"},
		{3, "very concise"},
		{5, "concise but include key context"},
		{7, "supporting details"},
		{10, "detailed explanation"},
	}

	for _, c := range cases {
		var lv int32 = c.level
		g := NewCommsGate(mock, b, func() int32 { return atomic.LoadInt32(&lv) }, nil, nil)
		got := g.verbosityInstruction()
		if !strings.Contains(got, c.want) {
			t.Errorf("level %d: want substring %q, got %q", c.level, c.want, got)
		}
	}
}
