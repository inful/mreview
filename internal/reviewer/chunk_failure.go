package reviewer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ChunkFailureError is the typed error returned by ReviewMR when a
// chunk's LLM call fails after the retry budget is exhausted and
// AllowPartial is false (the default). It signals "no summary and
// no inline findings were posted" — the operator must re-run the
// review manually rather than trusting a half-completed report.
//
// Fields carry the operator-facing context that the CLI surfaces
// in its non-zero-exit message: which batch, which files, how many
// attempts were made, and the underlying error.
type ChunkFailureError struct {
	Batch    int      // 1-indexed batch number that failed
	Files    []string // paths in the failed batch (capped by chunkFileList)
	Attempts int      // total LLM calls made for this chunk (1 + retries)
	Cause    error    // the underlying error from the final attempt
}

// Error renders the message operators see in logs and CLI output.
// Format: "chunk review failed: batch N (file1,file2) after N
// attempts: <cause>".
func (e *ChunkFailureError) Error() string {
	return fmt.Sprintf("chunk review failed: batch %d (%s) after %d attempts: %v",
		e.Batch, strings.Join(e.Files, ","), e.Attempts, e.Cause)
}

// Unwrap exposes the underlying cause to errors.Is / errors.As.
func (e *ChunkFailureError) Unwrap() error { return e.Cause }

// isTransientLLMError reports whether err is the kind of failure we
// want to retry: a per-call timeout (context.DeadlineExceeded).
//
// What we treat as transient and why:
//
//   - context.DeadlineExceeded: the LLM call timed out. Likely a
//     transient stall on the model side. Worth retrying with backoff.
//
// What we deliberately do NOT treat as transient:
//
//   - context.Canceled: the operator pressed Ctrl-C. Retrying
//     fights the shutdown; propagate immediately.
//   - 5xx / 429 from the LLM endpoint: handled by the openai-go
//     client's own MaxRetries=2 layer we set in cmd/mreview. The
//     chunk-level retry catches the timeout case that bubbles past
//     it.
//   - Parse failures: the same prompt is likely to fail the same
//     way; an operator re-run is more useful than a retry.
//
// errors.Is(err, context.DeadlineExceeded) handles both direct and
// wrapped cases (fmt.Errorf("llm: %w", err)).
func isTransientLLMError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// chunkBackoff returns the wait duration before the next retry
// attempt. Index is 0 for the first retry (after attempt 1 fails),
// 1 for the second retry (after attempt 2 fails), etc.
//
// Exponential with no jitter (kept deterministic for tests); cap at
// 5s. Tuned for the default ChunkRetries=1, where the worst-case
// wait is 500ms — acceptable for the local-LLM target where most
// calls succeed on attempt 1.
func chunkBackoff(retryIndex int) time.Duration {
	const initial = 500 * time.Millisecond
	const maxBackoff = 5 * time.Second
	d := initial << retryIndex // 500ms, 1s, 2s, 4s, ...
	if d > maxBackoff || d < 0 {
		return maxBackoff
	}
	return d
}
