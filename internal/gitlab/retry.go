package gitlab

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// RetryConfig controls how doWithRetry retries on transient errors.
//
//   - MaxAttempts is the total attempt count, NOT the number of retries.
//     MaxAttempts == 1 disables retry entirely.
//   - InitialBackoff is the wait before the second attempt; subsequent
//     waits double up to MaxBackoff with ±20% jitter.
//   - MaxBackoff caps the per-attempt wait. If Retry-After asks for a
//     longer wait, Retry-After is honored up to RetryAfterCap.
//   - RetryAfterCap is the upper bound on a server-requested wait. A
//     misbehaving server that returns "Retry-After: 999999" gets capped.
//   - Logger is optional; nil disables the per-attempt log line.
//
// Zero-value RetryConfig is invalid (MaxAttempts == 0 means "no
// attempts"). Callers should always set MaxAttempts explicitly.
type RetryConfig struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	RetryAfterCap  time.Duration
	Logger         *slog.Logger
}

// retryAfterCapDefault is the default ceiling for a Retry-After header
// when RetryConfig.RetryAfterCap is zero.
const retryAfterCapDefault = 60 * time.Second

// doWithRetry runs fn, retrying on transient errors. The function
// receives the current attempt number (1-indexed) so it can decorate
// logs with "first try", "retry 2/3", etc.
//
// fn should return *Error for any classified failure; non-*Error
// failures (context canceled, programmer error) abort the retry loop
// immediately.
//
// op is a short label used in retry log lines ("FetchMR", "PostNote").
func doWithRetry(ctx context.Context, cfg RetryConfig, op string, fn func(ctx context.Context, attempt int) error) error {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.RetryAfterCap == 0 {
		cfg.RetryAfterCap = retryAfterCapDefault
	}

	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn(ctx, attempt)
		if err == nil {
			return nil
		}
		lastErr = err

		// Decide whether to retry. Only KindTransient is retryable.
		e := AsError(err)
		if e == nil || e.Kind != KindTransient {
			return err
		}
		if attempt == cfg.MaxAttempts {
			break
		}

		// Compute the wait. Server-supplied Retry-After takes
		// precedence; otherwise exponential backoff with jitter.
		wait := nextBackoff(cfg, attempt, e)
		if cfg.Logger != nil {
			cfg.Logger.Warn("retrying after transient error",
				"op", op,
				"attempt", attempt,
				"max_attempts", cfg.MaxAttempts,
				"wait", wait.String(),
				"status", e.StatusCode,
				"err", e.Error(),
			)
		}

		// Respect context cancellation while waiting.
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return lastErr
}

// nextBackoff returns the wait time before the next attempt. Honors
// Retry-After from the response if present and parseable; falls back
// to exponential backoff with ±20% jitter.
//
// Exposed as a package-private helper so tests can pin the result for
// deterministic timing.
func nextBackoff(cfg RetryConfig, attempt int, e *Error) time.Duration {
	if d := retryAfterDuration(e, time.Now()); d > 0 {
		if cfg.RetryAfterCap > 0 && d > cfg.RetryAfterCap {
			d = cfg.RetryAfterCap
		}
		return d
	}
	return exponentialBackoff(cfg.InitialBackoff, cfg.MaxBackoff, attempt)
}

// exponentialBackoff returns base * 2^(attempt-1) capped at maxBackoff,
// then applies ±20% jitter. attempt is 1-indexed (the wait BEFORE the
// Nth retry, so attempt=1 yields ~base).
func exponentialBackoff(base, maxBackoff time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	// base * 2^(attempt-1), but watch the overflow.
	mult := math.Pow(2, float64(attempt-1))
	d := time.Duration(float64(base) * mult)
	if d <= 0 || d > maxBackoff {
		d = maxBackoff
	}
	// Jitter ±20%.
	jitter := time.Duration(float64(d) * 0.2 * (2*rand.Float64() - 1))
	d += jitter
	if d < 0 {
		d = 0
	}
	return d
}

// retryAfterDuration parses the Retry-After value from the error's Body
// when the body carries a header value, or returns 0 when not present.
//
// The Body of a transient *Error typically contains the upstream error
// message; we don't get the header back through the wrapped client.
// In practice we read Retry-After directly from the *http.Response
// inside the client method (see client.go), so this helper is
// conservative: it only handles delta-seconds in the body for now.
//
// now is injected so tests can pin the wall clock for HTTP-date parsing.
func retryAfterDuration(e *Error, now time.Time) time.Duration {
	if e == nil || e.Body == "" {
		return 0
	}
	// Try delta-seconds first.
	if d, err := strconv.Atoi(e.Body); err == nil && d >= 0 {
		return time.Duration(d) * time.Second
	}
	// Try HTTP-date.
	if t, err := http.ParseTime(e.Body); err == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// parseRetryAfterHeader is the canonical parser for the HTTP header
// value. It accepts both delta-seconds ("120") and HTTP-date forms.
// Returns 0 for missing or unparseable values.
func parseRetryAfterHeader(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if d, err := strconv.Atoi(value); err == nil && d >= 0 {
		return time.Duration(d) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}
