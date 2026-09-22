package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestThrottle_FirstDelivery_Accepted(t *testing.T) {
	th := newThrottle(30*time.Second, 100)
	if th.shouldSkip("group/project", 42, time.Unix(1000, 0)) {
		t.Error("first delivery for an MR should never be skipped")
	}
	if th.Size() != 1 {
		t.Errorf("Size = %d, want 1", th.Size())
	}
}

func TestThrottle_RapidDuplicate_Skipped(t *testing.T) {
	th := newThrottle(30*time.Second, 100)
	t0 := time.Unix(1000, 0)
	if th.shouldSkip("group/project", 42, t0) {
		t.Fatal("first delivery should be accepted")
	}
	// 5 seconds later: within the 30s window.
	if !th.shouldSkip("group/project", 42, t0.Add(5*time.Second)) {
		t.Error("delivery 5s after accepted should be skipped (window=30s)")
	}
}

func TestThrottle_AfterWindow_Accepted(t *testing.T) {
	th := newThrottle(30*time.Second, 100)
	t0 := time.Unix(1000, 0)
	th.shouldSkip("group/project", 42, t0)
	// 31 seconds later: window expired.
	if th.shouldSkip("group/project", 42, t0.Add(31*time.Second)) {
		t.Error("delivery 31s after accepted should be accepted (window=30s)")
	}
}

func TestThrottle_DifferentMRs_Independent(t *testing.T) {
	th := newThrottle(30*time.Second, 100)
	t0 := time.Unix(1000, 0)
	th.shouldSkip("group/A", 42, t0)
	if th.shouldSkip("group/B", 42, t0) {
		t.Error("different projects should not share throttle state")
	}
	if th.shouldSkip("group/A", 43, t0) {
		t.Error("different IIDs in same project should not share throttle state")
	}
}

func TestThrottle_ZeroWindow_Disabled(t *testing.T) {
	th := newThrottle(0, 100)
	t0 := time.Unix(1000, 0)
	for i := 0; i < 100; i++ {
		if th.shouldSkip("group/project", 42, t0) {
			t.Errorf("zero-window throttle should accept every delivery (call %d)", i)
		}
	}
}

func TestThrottle_EvictionAtMaxSize(t *testing.T) {
	th := newThrottle(time.Hour, 3) // tiny cap to force eviction
	base := time.Unix(1000, 0)
	th.shouldSkip("p", 1, base)
	th.shouldSkip("p", 2, base.Add(1*time.Second))
	th.shouldSkip("p", 3, base.Add(2*time.Second))
	if th.Size() != 3 {
		t.Errorf("Size = %d, want 3", th.Size())
	}
	// Fourth entry should evict the oldest (IID 1).
	th.shouldSkip("p", 4, base.Add(3*time.Second))
	if th.Size() != 3 {
		t.Errorf("Size after eviction = %d, want 3", th.Size())
	}
	if th.shouldSkip("p", 1, base.Add(4*time.Second)) {
		t.Error("IID 1 should have been evicted; re-add should be accepted")
	}
}

func TestThrottle_Sweep_RemovesOldEntries(t *testing.T) {
	th := newThrottle(10*time.Second, 100)
	base := time.Unix(1000, 0)
	th.shouldSkip("p", 1, base)
	th.shouldSkip("p", 2, base.Add(1*time.Second))
	th.shouldSkip("p", 3, base.Add(2*time.Second))
	// Sweep at base+15s — all three are older than 10s.
	removed := th.sweep(base.Add(15 * time.Second))
	if removed != 3 {
		t.Errorf("sweep removed %d, want 3", removed)
	}
	if th.Size() != 0 {
		t.Errorf("Size after sweep = %d, want 0", th.Size())
	}
}

func TestThrottle_Sweep_KeepsRecentEntries(t *testing.T) {
	th := newThrottle(10*time.Second, 100)
	base := time.Unix(1000, 0)
	th.shouldSkip("p", 1, base.Add(5*time.Second))  // 5s old at sweep
	th.shouldSkip("p", 2, base.Add(12*time.Second)) // 0s old at sweep
	// Sweep at base+12s — IID 1 is 7s old (within 10s window); IID 2 is 0s old.
	removed := th.sweep(base.Add(12 * time.Second))
	if removed != 0 {
		t.Errorf("sweep removed %d, want 0", removed)
	}
	if th.Size() != 2 {
		t.Errorf("Size after sweep = %d, want 2", th.Size())
	}
}

func TestThrottleSweeper_CancelsOnContext(t *testing.T) {
	th := newThrottle(time.Hour, 100)
	th.shouldSkip("p", 1, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		throttleSweeper(ctx, th, 50*time.Millisecond, nilLogger())
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweeper did not exit on context cancel")
	}
}

// nilLogger returns a discard logger so the sweeper goroutine
// can call Debug() without panicking on nil.
func nilLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
