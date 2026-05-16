package audio

import (
	"sync"
	"testing"
	"time"
)

// fakeDucker records every SetDucked edge for assertions.
type fakeDucker struct {
	mu     sync.Mutex
	edges  []bool
	closed bool
}

func (f *fakeDucker) SetDucked(d bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edges = append(f.edges, d)
	return nil
}
func (f *fakeDucker) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}
func (f *fakeDucker) snapshot() ([]bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.edges...), f.closed
}

// waitUntil polls until cond is true or the deadline expires. Polling
// beats fixed sleeps for timer tests — passes earlier when the timer
// fires fast and fails fast when the logic is wrong.
func waitUntil(t *testing.T, cond func() bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", within)
}

func TestCoordinator_DucksAndUnducks(t *testing.T) {
	f := &fakeDucker{}
	c := NewCoordinator(f)
	c.SpeakingFor(40 * time.Millisecond)

	// Duck edge should be immediate.
	waitUntil(t, func() bool {
		e, _ := f.snapshot()
		return len(e) >= 1
	}, 100*time.Millisecond)
	e, _ := f.snapshot()
	if e[0] != true {
		t.Fatalf("first edge should be duck=true; got %v", e)
	}

	// Unduck edge after the duration elapses.
	waitUntil(t, func() bool {
		e, _ := f.snapshot()
		return len(e) >= 2
	}, 500*time.Millisecond)
	e, _ = f.snapshot()
	if !(e[0] == true && e[1] == false) {
		t.Fatalf("expected duck then unduck; got %v", e)
	}
}

func TestCoordinator_OverlappingSpeakingFor_ExtendsDeadline(t *testing.T) {
	f := &fakeDucker{}
	c := NewCoordinator(f)
	c.SpeakingFor(80 * time.Millisecond)
	// 30 ms in, a second call extends the unduck out 80 ms more.
	time.Sleep(30 * time.Millisecond)
	c.SpeakingFor(80 * time.Millisecond)

	// At 80 ms total elapsed, the original timer would have fired —
	// but the extension should have stopped it. Only the duck edge
	// should have shown up so far.
	time.Sleep(70 * time.Millisecond) // total elapsed ~100 ms
	e, _ := f.snapshot()
	if len(e) != 1 || e[0] != true {
		t.Fatalf("expected only duck edge at 100ms; got %v", e)
	}

	// Now wait for the extended unduck (deadline was ~30+80 = 110 ms).
	waitUntil(t, func() bool {
		e, _ := f.snapshot()
		return len(e) >= 2
	}, 300*time.Millisecond)
	e, _ = f.snapshot()
	if !(len(e) == 2 && e[0] == true && e[1] == false) {
		t.Fatalf("expected exactly duck then unduck; got %v", e)
	}
}

func TestCoordinator_SpeakingTick_StreamingPattern(t *testing.T) {
	f := &fakeDucker{}
	c := NewCoordinator(f)
	// Simulate Gemini Live: 6 chunks at 15 ms each, then silence.
	for i := 0; i < 6; i++ {
		c.SpeakingTick(60 * time.Millisecond)
		time.Sleep(15 * time.Millisecond)
	}
	// Only one duck edge during the stream.
	e, _ := f.snapshot()
	if len(e) != 1 || e[0] != true {
		t.Fatalf("expected single duck during stream; got %v", e)
	}
	// After tail elapses, unduck.
	waitUntil(t, func() bool {
		e, _ := f.snapshot()
		return len(e) >= 2
	}, 300*time.Millisecond)
	e, _ = f.snapshot()
	if !(len(e) == 2 && e[1] == false) {
		t.Fatalf("expected unduck after stream; got %v", e)
	}
}

func TestCoordinator_Stop_ForcesUnduckAndClose(t *testing.T) {
	f := &fakeDucker{}
	c := NewCoordinator(f)
	c.SpeakingFor(500 * time.Millisecond)
	waitUntil(t, func() bool {
		e, _ := f.snapshot()
		return len(e) >= 1
	}, 100*time.Millisecond)

	c.Stop()

	e, closed := f.snapshot()
	if !(len(e) == 2 && e[0] == true && e[1] == false) {
		t.Fatalf("Stop should issue unduck edge; got %v", e)
	}
	if !closed {
		t.Fatalf("Stop should call Close on the Ducker")
	}

	// Subsequent calls should be no-ops, not panics.
	c.SpeakingFor(100 * time.Millisecond)
	c.SpeakingTick(100 * time.Millisecond)
	c.Stop()
	time.Sleep(50 * time.Millisecond)
	e, _ = f.snapshot()
	if len(e) != 2 {
		t.Fatalf("no edges should fire after Stop; got %v", e)
	}
}

func TestCoordinator_NilDucker_IsSafe(t *testing.T) {
	c := NewCoordinator(nil)
	// All operations must be no-op and panic-free.
	c.SpeakingFor(10 * time.Millisecond)
	c.SpeakingTick(10 * time.Millisecond)
	c.Stop()
}

func TestCoordinator_ZeroDuration_NoOp(t *testing.T) {
	f := &fakeDucker{}
	c := NewCoordinator(f)
	c.SpeakingFor(0)
	c.SpeakingTick(0)
	c.SpeakingChunk(0, 50*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	e, _ := f.snapshot()
	if len(e) != 0 {
		t.Fatalf("zero-duration calls should produce no edges; got %v", e)
	}
}

// TestCoordinator_SpeakingChunk_FasterThanRealtime models the Gemini
// Live bug: the producer emits 10 chunks of 10 ms playback in 50 ms
// wall time, totalling 100 ms of buffered audio. The unduck should
// fire ~100 ms after the LAST chunk (i.e. when the buffer drains)
// plus the 20 ms tail — NOT 20 ms after the last chunk as the old
// SpeakingTick logic would have.
func TestCoordinator_SpeakingChunk_FasterThanRealtime(t *testing.T) {
	f := &fakeDucker{}
	c := NewCoordinator(f)
	const (
		chunkDur = 10 * time.Millisecond
		wallGap  = 5 * time.Millisecond // producer emits 2× realtime
		tail     = 20 * time.Millisecond
		chunks   = 10
	)
	for i := 0; i < chunks; i++ {
		c.SpeakingChunk(chunkDur, tail)
		time.Sleep(wallGap)
	}
	lastChunkAt := time.Now()

	// At this point: 10 chunks declared (100 ms of playback), 50 ms
	// wall elapsed. ~50 ms of buffered audio remains. Plus 20 ms tail
	// means we expect the unduck around lastChunkAt + 70 ms.
	// SpeakingTick(20 ms) would have unducked already.
	e, _ := f.snapshot()
	if len(e) != 1 || e[0] != true {
		t.Fatalf("expected single duck edge during stream; got %v", e)
	}

	waitUntil(t, func() bool {
		e, _ := f.snapshot()
		return len(e) >= 2
	}, 500*time.Millisecond)

	elapsed := time.Since(lastChunkAt)
	e, _ = f.snapshot()
	if !(len(e) == 2 && e[1] == false) {
		t.Fatalf("expected unduck; got %v", e)
	}
	// Expected unduck delay after last chunk: leftover buffer (~50 ms)
	// + tail (20 ms) = ~70 ms. Wide tolerance for timer jitter under
	// load: 30 ms floor (must NOT be the old 20 ms tail), 200 ms ceiling.
	if elapsed < 30*time.Millisecond {
		t.Fatalf("unduck fired too early: %v after last chunk (would have been the old bug)", elapsed)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("unduck fired too late: %v after last chunk", elapsed)
	}
}

// TestCoordinator_SpeakingChunk_TrailingSilence verifies the single-
// chunk case: one 50 ms chunk + 30 ms tail = unduck at ~80 ms.
func TestCoordinator_SpeakingChunk_TrailingSilence(t *testing.T) {
	f := &fakeDucker{}
	c := NewCoordinator(f)
	start := time.Now()
	c.SpeakingChunk(50*time.Millisecond, 30*time.Millisecond)

	waitUntil(t, func() bool {
		e, _ := f.snapshot()
		return len(e) >= 2
	}, 300*time.Millisecond)
	elapsed := time.Since(start)
	e, _ := f.snapshot()
	if !(len(e) == 2 && e[0] == true && e[1] == false) {
		t.Fatalf("expected duck then unduck; got %v", e)
	}
	if elapsed < 60*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("expected unduck ~80 ms after start; got %v", elapsed)
	}
}

// TestCoordinator_SpeakingChunk_CrossTurnReset verifies that a second
// turn (after the first turn fully drains and unducks) anchors its
// playbackEnd at "now", not at the stale value from turn one.
// Without the playbackEnd-vs-now check, turn two's deadline would be
// stale-playbackEnd + chunkDur + tail (immediate fire) — which would
// still work in practice because the gen counter rejects the old
// callback, but defensively we want playbackEnd to reset.
func TestCoordinator_SpeakingChunk_CrossTurnReset(t *testing.T) {
	f := &fakeDucker{}
	c := NewCoordinator(f)

	// Turn 1: one short chunk, wait for full unduck.
	c.SpeakingChunk(15*time.Millisecond, 15*time.Millisecond)
	waitUntil(t, func() bool {
		e, _ := f.snapshot()
		return len(e) >= 2
	}, 200*time.Millisecond)

	// Idle long enough that any stale state would have drifted past.
	time.Sleep(50 * time.Millisecond)

	// Turn 2: a fresh chunk should duck again immediately and unduck
	// only after its own 30 ms + 15 ms.
	start := time.Now()
	c.SpeakingChunk(30*time.Millisecond, 15*time.Millisecond)
	waitUntil(t, func() bool {
		e, _ := f.snapshot()
		return len(e) >= 4
	}, 300*time.Millisecond)
	elapsed := time.Since(start)
	e, _ := f.snapshot()
	if !(len(e) == 4 && e[2] == true && e[3] == false) {
		t.Fatalf("expected turn 2 duck/unduck pair; got %v", e)
	}
	if elapsed < 35*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("turn 2 unduck timing off: %v", elapsed)
	}
}
