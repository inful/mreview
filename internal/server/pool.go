package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Job is one unit of webhook work — review a specific MR.
//
// The handler is expected to be idempotent: GitLab may retry the
// same delivery, and our dedupe layer (phase 6) handles
// already-posted findings.
type Job struct {
	// Project is the GitLab project path (group/project).
	Project string

	// IID is the merge request IID.
	IID int

	// Action is the GitLab object_attributes.action value
	// ("open", "reopen", "update", ...). Used for logging and
	// future routing decisions.
	Action string

	// EventType is the X-Gitlab-Event header value (always
	// "Merge Request Hook" today; reserved for future event
	// types like "Note Hook").
	EventType string
}

// Pool is a bounded worker pool that runs JobHandler against
// submitted Jobs. Backpressure is signaled to the HTTP handler
// by returning ErrQueueFull from Submit, which the handler
// converts to HTTP 503 so GitLab retries.
type Pool struct {
	logger  *slog.Logger
	jobs    chan Job
	stop    chan struct{}
	done    sync.WaitGroup
	stopped sync.Once
}

// newPool builds a Pool with a generous default capacity (32).
// Callers may override via SetCapacity before Start. The default
// matches GitLab's per-IP webhook delivery rate, which is much
// higher than what a single local LLM can consume.
func newPool(logger *slog.Logger) *Pool {
	return &Pool{
		logger: logger,
		jobs:   make(chan Job, 32),
		stop:   make(chan struct{}),
	}
}

// SetCapacity resizes the job channel. Only effective before
// start() is called; otherwise the existing channel is reused.
func (p *Pool) SetCapacity(n int) {
	if n <= 0 {
		return
	}
	// Drain-and-replace is unsafe once started; we accept that
	// limitation since the typical pattern is configure-then-start.
	p.jobs = make(chan Job, n)
}

// Capacity returns the current channel capacity.
func (p *Pool) Capacity() int { return cap(p.jobs) }

// Depth returns the number of jobs currently queued.
func (p *Pool) Depth() int { return len(p.jobs) }

// ErrQueueFull is returned by Submit when the channel is at
// capacity. The HTTP handler maps this to 503 so GitLab retries
// the delivery later (per its exponential-backoff policy).
var ErrQueueFull = errQueueFull{}

type errQueueFull struct{}

func (errQueueFull) Error() string { return "server: job queue is full" }

// Submit enqueues a Job. Returns ErrQueueFull when the channel is
// at capacity (caller should respond 503 to GitLab).
func (p *Pool) Submit(job Job) error {
	select {
	case p.jobs <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

// start launches the workers. The count comes from the caller
// (typically Config.Workers, defaulted to 4 by server.New). The
// pool's worker count is independent of the queue depth — see
// Config.Workers documentation for why.
func (p *Pool) start(ctx context.Context, handler JobHandler, workers int) {
	for i := 0; i < workers; i++ {
		p.done.Add(1)
		go p.runWorker(ctx, handler)
	}
}

// runWorker pulls jobs from the channel until it's closed or ctx
// is canceled.
func (p *Pool) runWorker(ctx context.Context, handler JobHandler) {
	defer p.done.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			p.runOne(ctx, job, handler)
		}
	}
}

// runOne invokes handler with a per-job context derived from
// poolCtx. Logs start/finish with timing so operators can see
// which reviews are slow.
func (p *Pool) runOne(poolCtx context.Context, job Job, handler JobHandler) {
	ctx, cancel := context.WithCancel(poolCtx)
	defer cancel()
	logger := p.logger.With(
		"project", job.Project,
		"iid", job.IID,
		"action", job.Action,
		"event", job.EventType,
	)
	logger.Info("worker picked up job")
	defer func() {
		if r := recover(); r != nil {
			logger.Error("worker panic", "recovered", fmt.Sprintf("%v", r))
		}
	}()
	handler(ctx, job)
	logger.Info("worker finished job")
}

// wait blocks until all workers exit or the timeout elapses.
func (p *Pool) wait(timeout time.Duration) {
	p.stopped.Do(func() {
		close(p.stop)
		close(p.jobs)
	})
	done := make(chan struct{})
	go func() {
		p.done.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		p.logger.Warn("pool shutdown timeout exceeded; abandoning in-flight jobs")
	}
}
