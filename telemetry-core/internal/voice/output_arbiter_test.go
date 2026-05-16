package voice

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutputArbiter_AcquireIdleSucceedsImmediately(t *testing.T) {
	a := NewOutputArbiter()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	release, err := a.Acquire(ctx, "live", false, nil)
	if err != nil {
		t.Fatalf("acquire idle: %v", err)
	}
	if a.Owner() != "live" {
		t.Errorf("owner = %q, want live", a.Owner())
	}
	release()
	if a.Owner() != "" {
		t.Errorf("after release, owner = %q, want empty", a.Owner())
	}
}

func TestOutputArbiter_SecondAcquireBlocksUntilRelease(t *testing.T) {
	a := NewOutputArbiter()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	release1, err := a.Acquire(ctx, "live", false, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	got := make(chan error, 1)
	go func() {
		_, err := a.Acquire(ctx, "voice_client", false, nil)
		got <- err
	}()

	// Should still be blocked.
	select {
	case e := <-got:
		t.Fatalf("second acquire returned early: err=%v", e)
	case <-time.After(60 * time.Millisecond):
	}

	release1()

	select {
	case e := <-got:
		if e != nil {
			t.Fatalf("second acquire after release: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("second acquire never completed after release")
	}
}

func TestOutputArbiter_PreemptCancelsPreviousHolder(t *testing.T) {
	a := NewOutputArbiter()

	holderCtx, holderCancel := context.WithCancel(context.Background())
	defer holderCancel()

	release1, err := a.Acquire(holderCtx, "live", false, holderCancel)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Confirm the holder's ctx is still live before preemption.
	select {
	case <-holderCtx.Done():
		t.Fatal("holder ctx cancelled before preemption")
	default:
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	release2, err := a.Acquire(ctx2, "urgent", true, nil)
	if err != nil {
		t.Fatalf("preempt acquire: %v", err)
	}
	defer release2()

	// Holder's ctx must now be cancelled.
	select {
	case <-holderCtx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("preempted holder ctx was not cancelled")
	}

	if a.Owner() != "urgent" {
		t.Errorf("owner = %q, want urgent", a.Owner())
	}

	// The original release closure must be a no-op now that ownership moved.
	release1()
	if a.Owner() != "urgent" {
		t.Errorf("late release from preempted owner clobbered slot: owner=%q", a.Owner())
	}
}

func TestOutputArbiter_PreemptIgnoredWhenHolderNotPreemptable(t *testing.T) {
	a := NewOutputArbiter()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Holder registers nil cancel — meaning "I am not preemptable".
	release1, err := a.Acquire(ctx, "ack", false, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release1()

	// Preempt request should wait, not steal.
	got := make(chan error, 1)
	go func() {
		_, err := a.Acquire(ctx, "urgent", true, nil)
		got <- err
	}()
	select {
	case e := <-got:
		t.Fatalf("preempt acquired non-preemptable holder: err=%v owner=%q", e, a.Owner())
	case <-time.After(100 * time.Millisecond):
	}
	if a.Owner() != "ack" {
		t.Errorf("owner = %q, want ack still", a.Owner())
	}
}

func TestOutputArbiter_AcquireRespectsContextCancellation(t *testing.T) {
	a := NewOutputArbiter()
	releaseHold, err := a.Acquire(context.Background(), "live", false, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer releaseHold()

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()

	if _, err := a.Acquire(ctx, "voice_client", false, nil); err == nil {
		t.Fatal("expected context.DeadlineExceeded waiting for slot")
	}
}

func TestOutputArbiter_DoubleReleaseIsSafe(t *testing.T) {
	a := NewOutputArbiter()
	release, err := a.Acquire(context.Background(), "live", false, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release() // must not panic, must not blow up the next acquire
	r2, err := a.Acquire(context.Background(), "next", false, nil)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	r2()
}

func TestOutputArbiter_ConcurrentAcquireSerializes(t *testing.T) {
	a := NewOutputArbiter()
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := a.Acquire(context.Background(), "worker", false, nil)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			now := concurrent.Add(1)
			if mc := maxConcurrent.Load(); now > mc {
				maxConcurrent.CompareAndSwap(mc, now)
			}
			time.Sleep(20 * time.Millisecond)
			concurrent.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("max concurrent owners = %d, want 1", got)
	}
}
