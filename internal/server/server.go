// Package server hosts the GitLab webhook receiver.
//
// Architecture:
//
//   - The HTTP server accepts POST /webhook from GitLab.
//   - Each request is verified via the X-Gitlab-Token shared
//     secret (HMAC compare).
//   - Accepted events are pushed to a bounded job channel; a
//     pool of N workers picks them up and runs the reviewer
//     against the MR.
//
// The server does NOT block on the LLM call: workers run with
// the same context as `mreview review` but the HTTP handler
// always returns 202 immediately (or 503 when the queue is
// full). GitLab will retry webhook deliveries on 5xx, so we
// never silently drop work.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Config wires the server. Every field is required.
type Config struct {
	// Addr is the HTTP listen address (e.g. ":8080").
	Addr string

	// WebhookSecret is the X-Gitlab-Token shared secret.
	WebhookSecret string

	// Handler is invoked for each accepted webhook event after
	// HMAC verification. The HTTP handler returns immediately;
	// long-running work happens here.
	Handler JobHandler

	// Logger receives access logs and per-event logs. nil falls
	// back to slog.Default().
	Logger *slog.Logger

	// RequiredLabel, when non-empty, gates webhook acceptance:
	// only MRs carrying a label whose title equals this value
	// are enqueued for review. Empty means "review every MR."
	//
	// The check uses GitLab's webhook payload — no extra API call
	// is needed (the labels array is in object_attributes.labels).
	RequiredLabel string

	// ThrottleWindow is the per-(project, IID) cooldown that
	// suppresses rapid-fire duplicate webhook deliveries. When a
	// delivery for the same MR arrives within this window after
	// the last accepted one, the handler returns 202 without
	// enqueuing a review.
	//
	// The review that's already in-flight (or just completed)
	// covers the latest commit anyway — GitLab only re-fires on
	// user-driven pushes (force-push, branch update), and rapid
	// pushes are the common case for which this exists.
	//
	// Zero disables throttling (every delivery is enqueued).
	// Recommended: 30s for most teams; longer for slower LLMs.
	ThrottleWindow time.Duration

	// ReadHeaderTimeout bounds time spent reading request
	// headers; protects against slow-loris attacks. 5s is a
	// sane default.
	ReadHeaderTimeout time.Duration

	// ShutdownTimeout is how long Run waits for in-flight
	// jobs to finish during graceful shutdown.
	ShutdownTimeout time.Duration

	// QueueSize is the depth of the worker pool's job channel.
	// 0 means use the default (32). Applied before start; not
	// adjustable at runtime.
	QueueSize int

	// Workers is the size of the worker pool — the steady-state
	// concurrency cap on simultaneous reviews. 0 means use the
	// default (4). Distinct from QueueSize, which is the burst
	// buffer (how many webhook deliveries can wait when the pool
	// is fully busy).
	//
	// Memory-constrained LLM servers (single Ollama on a Pi,
	// CPU-only llama.cpp on shared hardware) benefit from dropping
	// this; operators sharing an LLM with other tenants tighten it
	// to leave headroom; operators with a fat LLM (vLLM with
	// batching, multi-GPU inference) raise it above 4 to use the
	// box fully.
	//
	// Not yet exposed via the CLI / YAML — see issue #35 for the
	// operator-facing wiring. The plumbing here is so that
	// follow-up doesn't need to re-plumb.
	Workers int
}

// JobHandler is invoked for each verified webhook event.
//
// Implementations should respect ctx — when the server shuts down,
// ctx is canceled and the handler is expected to return promptly.
type JobHandler func(ctx context.Context, job Job)

// Server is the long-running webhook receiver.
type Server struct {
	cfg      Config
	http     *http.Server
	pool     *Pool
	throttle *throttle
	logger   *slog.Logger
}

// New builds a Server. Returns an error if any required field is
// missing.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("server: Config.Addr is required")
	}
	if cfg.WebhookSecret == "" {
		return nil, errors.New("server: Config.WebhookSecret is required")
	}
	if cfg.Handler == nil {
		return nil, errors.New("server: Config.Handler is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 5 * time.Second
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}

	pool := newPool(cfg.Logger)
	if cfg.QueueSize > 0 {
		pool.SetCapacity(cfg.QueueSize)
	}

	throttle := newThrottle(cfg.ThrottleWindow, 10000)

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", makeWebhookHandler(cfg, pool, cfg.Logger, throttle))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Lightweight liveness probe. Reports worker queue depth
		// + throttle size so operators can spot saturation
		// before GitLab retries pile up.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"queue_depth":%d,"queue_capacity":%d,"throttle_size":%d}`,
			pool.Depth(), pool.Capacity(), throttle.Size())
	})

	s := &Server{
		cfg:      cfg,
		pool:     pool,
		throttle: throttle,
		logger:   cfg.Logger,
	}
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}
	return s, nil
}

// Run starts the HTTP server and blocks until ctx is canceled,
// at which point it gracefully shuts down, draining in-flight
// jobs up to the configured timeout.
//
// Returns nil on a clean shutdown, or the underlying ListenAndServe
// error if the server fails to start. Once Run returns, the
// caller should treat the server as unusable.
func (s *Server) Run(ctx context.Context) error {
	// Listen separately so we can return a clean error on
	// bind failure (e.g. address already in use) instead of
	// blocking on the channel forever.
	ln, err := newListener(ctx, s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.cfg.Addr, err)
	}

	// Start the worker pool. ctx.Done() triggers drain.
	poolCtx, poolCancel := context.WithCancel(ctx)
	defer poolCancel()
	s.pool.start(poolCtx, s.cfg.Handler, s.cfg.Workers)

	// Throttle sweep: drop entries older than the window so the
	// map self-cleans on idle instances. The window is short
	// (operator-tunable, ~30s) so sweeping every window/2 is
	// plenty. Disabled when window == 0 (sweep is a no-op).
	if s.cfg.ThrottleWindow > 0 {
		sweepInterval := s.cfg.ThrottleWindow / 2
		if sweepInterval < time.Second {
			sweepInterval = time.Second
		}
		go throttleSweeper(ctx, s.throttle, sweepInterval, s.logger)
	}

	serverErr := make(chan error, 1)
	go func() {
		s.logger.Info("webhook server listening", "addr", s.cfg.Addr)
		err := s.http.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutdown signal received; draining")
	case err := <-serverErr:
		if err != nil {
			poolCancel()
			s.pool.wait(s.cfg.ShutdownTimeout)
			return err
		}
		// Server stopped on its own; cancel pool and return.
		poolCancel()
		s.pool.wait(s.cfg.ShutdownTimeout)
		return nil
	}

	// Graceful shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		s.logger.Warn("http shutdown error", "err", err)
	}
	poolCancel()
	s.pool.wait(s.cfg.ShutdownTimeout)
	if err := <-serverErr; err != nil {
		return err
	}
	return nil
}

// throttleSweeper periodically evicts entries older than the
// throttle window so the map stays bounded. Returns silently
// when ctx is canceled.
func throttleSweeper(ctx context.Context, t *throttle, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if removed := t.sweep(now); removed > 0 {
				logger.Debug("throttle sweep", "removed", removed, "remaining", t.Size())
			}
		}
	}
}
