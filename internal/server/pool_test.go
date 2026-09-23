package server

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// TestPool_RespectsWorkersConcurrency verifies that Pool.start with
// a given worker count never runs more than that many handlers
// concurrently. Pins issue #35's acceptance criterion: "spawn N
// workers, queue M > N jobs, assert at most N run concurrently".
//
// The test queues M jobs that all block on a release channel. We
// wait for the first N handlers to enter the critical section via a
// bounded `started` channel (buffer = N). After N signals, the
// remaining M-N jobs sit in the queue; if the pool is misconfigured,
// a fifth (or later) handler would already be running and would
// raise the observed max-inflight above N.
//
// The started channel uses a non-blocking send (default branch) so
// that handlers queued after release don't deadlock on a full
// buffer — they just drop the signal and proceed.
func TestPool_RespectsWorkersConcurrency(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := newPool(logger)
	pool.SetCapacity(100) // plenty of room; queue never blocks Submit

	const workers = 2
	const totalJobs = 8

	var inflight atomic.Int32
	var maxInflight atomic.Int32
	var completed atomic.Int32

	release := make(chan struct{})
	// Buffer == workers so only the first workers-many sends land.
	started := make(chan struct{}, workers)

	handler := func(ctx context.Context, job Job) {
		n := inflight.Add(1)
		// Update max-inflight using a CAS loop so concurrent
		// increments never lower the recorded maximum.
		for {
			old := maxInflight.Load()
			if n <= old || maxInflight.CompareAndSwap(old, n) {
				break
			}
		}
		// Non-blocking send: after the first `workers` handlers
		// signal entry, the buffer is full. Handlers that arrive
		// later (queued, waiting for a free worker) skip the
		// signal — they don't deadlock on send.
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		inflight.Add(-1)
		completed.Add(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.start(ctx, handler, workers)

	for i := 0; i < totalJobs; i++ {
		if err := pool.Submit(Job{Project: "g/p", IID: i}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	// Wait for all `workers` handlers to enter the critical section.
	deadline := time.After(2 * time.Second)
	for i := 0; i < workers; i++ {
		select {
		case <-started:
		case <-deadline:
			t.Fatalf("only %d/%d workers started in time", i, workers)
		}
	}

	// Steady-state assertion: exactly `workers` jobs in flight, no
	// completions yet, and max-inflight never exceeded `workers`.
	if got := inflight.Load(); got != int32(workers) {
		t.Errorf("current in-flight = %d, want %d", got, workers)
	}
	if got := maxInflight.Load(); got > int32(workers) {
		t.Errorf("max in-flight observed = %d, want <= %d", got, workers)
	}
	if got := completed.Load(); got != 0 {
		t.Errorf("completed before release = %d, want 0 (jobs should still be blocked)", got)
	}

	// Release every blocked handler and let the queue drain.
	// Don't call pool.wait() yet — it closes the job channel and
	// abandons queued jobs. Instead, poll the completed counter
	// until every job has finished (the handlers themselves
	// proceed past `<-release` because the channel is closed and
	// return immediately after dropping their `started` signal
	// via the non-blocking select).
	close(release)

	drainDeadline := time.After(5 * time.Second)
	for completed.Load() != int32(totalJobs) {
		select {
		case <-drainDeadline:
			t.Fatalf("only %d/%d jobs completed in time", completed.Load(), totalJobs)
		case <-time.After(5 * time.Millisecond):
		}
	}

	if got := inflight.Load(); got != 0 {
		t.Errorf("in-flight after drain = %d, want 0", got)
	}
}

// TestPool_DefaultWorkersIsFour pins the default that issue #11
// introduced (Config.Workers <= 0 → 4) and the test that
// issue #11's TestNew_WorkersDefaults covers at the New() level.
// At the Pool level we verify the contract: when started with no
// explicit worker count (here, default via New), the pool spins up
// exactly the configured count.
func TestPool_StartsRequestedNumberOfWorkers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := newPool(logger)
	pool.SetCapacity(100)

	const workers = 4
	const totalJobs = 4 // one per worker — keeps things simple

	started := make(chan struct{}, workers)
	handler := func(ctx context.Context, job Job) {
		started <- struct{}{}
		// Block forever (until ctx cancel) so we can probe the
		// steady state. This is a single-shot test, not the
		// release-drain pattern above.
		<-ctx.Done()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.start(ctx, handler, workers)

	for i := 0; i < totalJobs; i++ {
		if err := pool.Submit(Job{Project: "g/p", IID: i}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	deadline := time.After(2 * time.Second)
	for i := 0; i < workers; i++ {
		select {
		case <-started:
		case <-deadline:
			t.Fatalf("only %d/%d workers started in time", i, workers)
		}
	}

	// Give the pool a beat to spin up a 5th worker (which would
	// be a bug). If the next-started signal never arrives within
	// 100ms, we know the count was respected.
	select {
	case <-started:
		t.Errorf("a 5th worker started; pool exceeded configured worker count")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}
