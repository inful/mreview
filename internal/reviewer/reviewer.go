// Package reviewer orchestrates the end-to-end MR review pipeline:
//
//  1. Fetch the merge request from GitLab (metadata + diff_refs).
//  2. Fetch the per-file diff.
//  3. Chunk the diff per-file with internal/llm.ChunkByFile.
//  4. For each chunk (or batch): build the review prompt, call
//     the LLM, parse the JSON response.
//  5. If more than one chunk was reviewed, ask the LLM to
//     consolidate per-chunk summaries into one verdict.
//  6. Filter findings whose file paths don't appear in the diff
//     (LLM hallucination guard).
//  7. Post the summary note + one inline discussion per finding.
//     Line-out-of-range errors (KindConflict) drop the single
//     finding without failing the whole review.
//
// The Reviewer owns no goroutines — caller-provided context
// controls cancellation. The same Reviewer can run multiple
// reviews sequentially; for parallel review of one MR's chunks,
// that's a future optimization.
package reviewer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
	"github.com/inful/mreview/internal/strutil"
)

// Reviewer is the orchestrator. Construct one with NewReviewer,
// then call ReviewMR for each MR.
type Reviewer struct {
	cfg Config
	// action is the GitLab merge_request action (open /
	// reopen / update) that triggered the most recent
	// ReviewMR call. Used to gate the summary post so update
	// events don't pile up stale summaries. Set per-ReviewMR
	// call; zero means "unknown / standalone (review CLI)".
	action string
}

// NewReviewer validates cfg, applies defaults, and returns a Reviewer.
func NewReviewer(cfg Config) (*Reviewer, error) {
	cfg, err := validateConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Reviewer{cfg: cfg}, nil
}

// ReviewMR runs the full pipeline for one MR.
//
// The optional action parameter is the GitLab merge_request
// action that triggered the review ("open", "reopen",
// "update"). When provided, update events skip posting the
// summary note (which would otherwise accumulate on every
// push). Empty string means "no action context" (the review
// CLI subcommand) — the summary posts normally.
//
// Errors are typed: *gitlab.Error carries Kind → exit code, *llm
// errors surface directly. Subcommand code wraps them in
// *ExitError.
func (r *Reviewer) ReviewMR(ctx context.Context, project string, iid int, action ...string) (*Result, error) {
	if len(action) > 0 {
		r.action = action[0]
	} else {
		r.action = ""
	}
	logger := r.cfg.Logger.With("run", "review", "project", project, "mr_iid", iid, "action", r.action)

	mr, err := r.cfg.GitLab.FetchMR(ctx, project, iid)
	if err != nil {
		return nil, fmt.Errorf("reviewer: fetch MR: %w", err)
	}
	logger.Info("fetched MR", "title", mr.Title, "state", mr.State)

	changes, err := r.cfg.GitLab.FetchChanges(ctx, project, iid)
	if err != nil {
		return nil, fmt.Errorf("reviewer: fetch changes: %w", err)
	}
	logger.Info("fetched changes", "files", len(changes))

	// Apply ignore-paths filter BEFORE chunking so the LLM
	// doesn't waste tokens on files we don't want reviewed.
	if len(r.cfg.IgnorePaths) > 0 {
		before := len(changes)
		changes = filterChangesByPath(changes, r.cfg.IgnorePaths)
		logger.Info("applied ignore-paths filter",
			"patterns", len(r.cfg.IgnorePaths),
			"before", before,
			"after", len(changes),
		)
	}

	chunks, err := llm.ChunkByFile(changes, r.cfg.MaxDiffBytes)
	if err != nil {
		return nil, fmt.Errorf("reviewer: chunk diff: %w", err)
	}
	logger.Info("chunked diff", "chunks", len(chunks))

	// Run the LLM per chunk (or one merged call if there's only
	// one chunk). Each call returns a ReviewResponse; we
	// accumulate findings + summaries for the final merge.
	//
	// For multi-batch MRs, each batch's prompt carries the
	// findings from previous batches as a "Findings from previous
	// batches" section. This gives the chunk LLM ground truth
	// about file scope/types already seen, so it (and the merge
	// step downstream) cannot hallucinate claims like "the MR
	// contains no Go code" when its own batch was, say, only
	// markdown. See llm.PromptOptions.PriorFindings for the
	// root-cause write-up.
	chunkResponses := make([]llm.ReviewResponse, 0, len(chunks))
	var priorFindings []llm.Finding
	allChunksSucceeded := true
	for idx, chunkBatch := range batchChunks(chunks, r.cfg.MaxBatchBytes) {
		batchNum := idx + 1
		logger.Info("reviewing chunk batch",
			"batch", batchNum,
			"chunks", len(chunkBatch),
			"prior_findings", len(priorFindings),
		)

		resp, attempts, err := r.reviewChunksWithRetries(ctx, mr, chunkBatch, priorFindings, batchNum, logger)
		if err != nil {
			if !r.cfg.AllowPartial {
				// Atomic failure (issue #31): no summary, no inline
				// findings. The CLI surfaces the ChunkFailureError
				// as a non-zero exit so the operator knows the
				// review didn't complete — half a review is worse
				// than no review.
				return nil, &ChunkFailureError{
					Batch:    batchNum,
					Files:    chunkFileListAll(chunkBatch),
					Attempts: attempts,
					Cause:    err,
				}
			}
			// Legacy behaviour (issue #14's visibility fix):
			// log a WARN, substitute an empty response so the
			// summary note still gets posted (it'll explain the
			// gap). The log carries the batch index + bounded file
			// list so operators can identify which chunk was
			// dropped without re-reading every prompt.
			logger.Warn("chunk review failed",
				"batch", batchNum,
				"files", chunkFileList(chunkBatch),
				"err", err.Error(),
			)
			chunkResponses = append(chunkResponses, llm.ReviewResponse{})
			allChunksSucceeded = false
			continue
		}
		chunkResponses = append(chunkResponses, resp)
		// Accumulate for the next batch's prompt. We deliberately
		// do NOT dedupe here — the chunk LLM only needs enough
		// context to know what files have been touched and what
		// kinds of issues have been raised. Dedup happens against
		// GitLab discussions after the merge step.
		priorFindings = append(priorFindings, resp.Findings...)
	}

	// Merge: if multiple chunk responses, ask the LLM to
	// consolidate the per-chunk summaries. If just one, use it
	// directly.
	final, err := r.consolidate(ctx, mr, chunkResponses, allChunksSucceeded)
	if err != nil {
		return nil, fmt.Errorf("reviewer: consolidate: %w", err)
	}

	// Sanity-check the LLM output: a non-trivial diff that
	// produces zero findings almost always means the model
	// regressed to "LGTM" instead of reviewing. Surface it as a
	// loud WARN so operators running at default verbosity notice.
	// (See issue #14 — the system prompt used to invite this
	// pattern; the prompt now requires at least one finding, but
	// local LLMs still sometimes produce empty outputs.)
	if len(final.Findings) == 0 {
		if totalLines := countDiffLines(changes); totalLines > 10 {
			logger.Warn("LLM returned no findings on a non-trivial diff",
				"files", len(changes),
				"diff_lines", totalLines,
			)
		}
	}

	// Filter findings whose file paths don't appear in the diff
	// (LLM hallucination guard) and drop the rest through the
	// GitLab poster.
	validFiles := pathIndex(changes)
	fileMeta := fileMetaIndex(changes)
	filteredFindings := filterFindings(final.Findings, validFiles, logger)

	// Dedupe against the bot's prior comments. Phase 6 keeps this
	// simple — fingerprint = normalized body hash. Re-running the
	// review on an unchanged MR is a no-op.
	discs, err := r.cfg.GitLab.ListDiscussions(ctx, project, iid)
	if err != nil {
		logger.Warn("dedupe fetch failed; proceeding without dedupe", "err", err.Error())
		discs = nil
	}
	fpSet := newFingerprintSet(discs, r.cfg.BotUsername)
	logger.Info("dedupe set built", "size", fpSet.Size())
	if fpSet.Size() > 0 {
		filteredFindings = dedupeAgainstSet(filteredFindings, fpSet, logger)
	}

	result := &Result{
		MR:         mr,
		Findings:   make([]PostedFinding, 0, len(filteredFindings)),
		DedupeSize: fpSet.Size(),
	}

	// Post the summary first so the inline comments can reference it.
	// Skip on "update" events: every push would otherwise pile
	// up a fresh summary note in the MR timeline. The inline
	// findings already carry the per-push signal.
	if r.cfg.CommentMode != CommentModeInlineOnly && shouldPostSummary(r.action) {
		if summaryBody := renderSummary(mr, final.Summary, filteredFindings); summaryBody != "" {
			summary, err := r.postSummary(ctx, project, iid, summaryBody)
			if err != nil {
				// A summary post failure is logged but not fatal — we
				// still want to attempt the inline comments.
				logger.Warn("post summary failed", "err", err.Error())
			} else {
				result.Summary = summary
			}
		}
	} else if r.action == "update" {
		logger.Debug("skipping summary post on update event",
			"reason", "summary thread would accumulate on every push",
		)
	}

	// Post inline findings.
	if r.cfg.CommentMode != CommentModeSummaryOnly {
		for _, f := range filteredFindings {
			// Look up the file's diff metadata so postFinding can
			// construct the correct position (new / modified /
			// deleted / renamed) — see postFinding's docstring.
			meta := fileMeta[f.File]
			pf := r.postFinding(ctx, project, iid, mr.DiffRefs, meta, f)
			result.Findings = append(result.Findings, pf)
		}
	}

	logger.Info("review complete",
		"findings_total", len(filteredFindings),
		"findings_posted", countPosted(result.Findings),
		"summary_posted", result.Summary != nil,
	)
	return result, nil
}

// reviewChunks runs one LLM call against one batch of chunks and
// returns the parsed ReviewResponse. Batches let the reviewer push
// multiple small files into a single prompt when they all fit.
//
// The priorFindings argument carries findings from earlier batches
// in the same MR (batches 1..N-1 in the caller's loop). It is
// threaded into the user prompt as a "Findings from previous
// batches" section so the chunk LLM has ground truth about the
// file scope already covered. Pass nil on the first batch (single-
// batch MRs always pass nil).
func (r *Reviewer) reviewChunks(ctx context.Context, mr *gitlab.MergeRequest, chunks []llm.Chunk, priorFindings []llm.Finding) (llm.ReviewResponse, error) {
	meta := llm.ReviewMetadata{
		IID:          mr.IID,
		Title:        mr.Title,
		Description:  mr.Description,
		Author:       mr.Author.Username,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
	}
	system, user, err := llm.BuildReviewPrompt(meta, chunks, llm.PromptOptions{
		Categories:           r.cfg.Categories,
		IncludeMRDescription: r.cfg.IncludeDescription,
		SystemPromptSuffix:   r.cfg.SystemPromptSuffix,
		UserPromptSuffix:     r.cfg.UserPromptSuffix,
		PriorFindings:        priorFindings,
	})
	if err != nil {
		return llm.ReviewResponse{}, err
	}

	// Diagnostic: log the LLM prompt and raw response when
	// --verbose is set. Helps confirm whether the LLM is
	// actually receiving the diff content (or whether the
	// chunker is producing empty/garbage chunks), and whether
	// the response is `findings: []` (model behaviour) or a
	// parse failure.
	r.cfg.Logger.Debug("llm prompt",
		"model", r.cfg.Model,
		"chunks", len(chunks),
		"system_len", len(system),
		"user_len", len(user),
		"system", system,
		"user", user,
	)

	resp, err := r.cfg.LLM.Chat(ctx, llm.ChatRequest{
		System:          system,
		User:            user,
		Model:           r.cfg.Model,
		Temperature:     r.cfg.Temperature,
		MaxTokens:       r.cfg.MaxTokens,
		ResponseFormat:  llm.ResponseFormatJSONObject,
		ReasoningEffort: r.cfg.ReasoningEffort,
		Timeout:         r.cfg.PerChunkTimeout,
	})
	if err != nil {
		return llm.ReviewResponse{}, fmt.Errorf("llm: %w", err)
	}

	r.cfg.Logger.Debug("llm raw response",
		"model", r.cfg.Model,
		"raw", resp.Content,
	)

	parsed, err := llm.ParseReviewResponse(resp.Content)
	if err != nil {
		return llm.ReviewResponse{}, fmt.Errorf("parse: %w (raw: %s)", err, strutil.Truncate(resp.Content, 1000))
	}
	return parsed, nil
}

// consolidate combines per-chunk ReviewResponses into a single
// verdict. Single-chunk case returns the chunk response directly;
// multi-chunk case asks the LLM to merge the summaries.
//
// allChunksSucceeded gates the "fallback concatenation" path used
// when the merge LLM call or its parse fails. Under the atomic
// default (AllowPartial=false) allChunksSucceeded is always true
// here — any chunk failure has already aborted ReviewMR. Under
// AllowPartial=true, chunks may have silently failed (substituted
// empty); in that case the fallback would silently publish a
// half-broken review and the merge step is gated to refuse instead.
func (r *Reviewer) consolidate(ctx context.Context, mr *gitlab.MergeRequest, chunks []llm.ReviewResponse, allChunksSucceeded bool) (llm.ReviewResponse, error) {
	if len(chunks) == 0 {
		return llm.ReviewResponse{}, errors.New("reviewer: no chunk responses to consolidate")
	}
	if len(chunks) == 1 {
		return chunks[0], nil
	}

	// Build a short merge prompt: per-chunk summaries + the
	// combined findings. Ask the model to emit a single ReviewResponse.
	var summaries strings.Builder
	var allFindings []llm.Finding
	for i, c := range chunks {
		fmt.Fprintf(&summaries, "Chunk %d summary: %s\n", i+1, c.Summary)
		allFindings = append(allFindings, c.Findings...)
	}

	system, user := buildMergePrompt(mr, chunks)

	r.cfg.Logger.Debug("llm merge prompt",
		"model", r.cfg.Model,
		"system_len", len(system),
		"user_len", len(user),
		"system", system,
		"user", user,
	)

	resp, err := r.cfg.LLM.Chat(ctx, llm.ChatRequest{
		System:          system,
		User:            user,
		Model:           r.cfg.Model,
		Temperature:     r.cfg.Temperature,
		MaxTokens:       r.cfg.MaxTokens,
		ResponseFormat:  llm.ResponseFormatJSONObject,
		ReasoningEffort: r.cfg.ReasoningEffort,
		Timeout:         r.cfg.PerChunkTimeout,
	})
	if err != nil {
		// Issue #31: when any chunk already failed, refuse to
		// fall back to raw concatenation. Two failure modes
		// stacked together shouldn't silently publish.
		if !allChunksSucceeded {
			return llm.ReviewResponse{}, fmt.Errorf("reviewer: merge call failed after chunk failure: %w", err)
		}
		// Fallback: concatenate per-chunk summaries so the user
		// gets findings even if the merge call fails.
		r.cfg.Logger.Warn("merge call failed; using raw concatenation",
			"err", err.Error(),
		)
		return mergedFallback(allFindings, summaries.String()), nil
	}

	r.cfg.Logger.Debug("llm merge raw response",
		"model", r.cfg.Model,
		"raw", resp.Content,
	)

	parsed, err := llm.ParseReviewResponse(resp.Content)
	if err != nil {
		if !allChunksSucceeded {
			return llm.ReviewResponse{}, fmt.Errorf("reviewer: merge parse failed after chunk failure: %w", err)
		}
		return mergedFallback(allFindings, summaries.String()), nil
	}
	// If the merge dropped findings, restore them. Conservative:
	// the union of inputs wins.
	if len(parsed.Findings) < len(allFindings) {
		parsed.Findings = dedupeFindings(append(parsed.Findings, allFindings...))
	}
	return parsed, nil
}

// mergedFallback assembles a ReviewResponse from accumulated
// inputs when the merge LLM call fails or the merged response drops
// entries. Used by consolidate as the unified fallback path.
func mergedFallback(findings []llm.Finding, summaries string) llm.ReviewResponse {
	return llm.ReviewResponse{
		Findings: findings,
		Summary:  strings.TrimSpace(summaries),
	}
}

// reviewChunksWithRetries is the per-batch wrapper that adds the
// chunk-level retry budget (issue #17) on top of reviewChunks. It
// calls reviewChunks up to 1+ChunkRetries times and:
//
//   - returns immediately on success;
//   - retries with backoff on transient errors
//     (isTransientLLMError);
//   - returns immediately on non-transient errors (no retry —
//     parse failures and context.Canceled propagate);
//   - returns the LAST error and the total attempt count when
//     retries are exhausted.
//
// attempt is 1-indexed in logs so operators see "attempt 1 of 2",
// "attempt 2 of 2", matching what the issue text asks for.
func (r *Reviewer) reviewChunksWithRetries(
	ctx context.Context,
	mr *gitlab.MergeRequest,
	chunks []llm.Chunk,
	priorFindings []llm.Finding,
	batchNum int,
	logger *slog.Logger,
) (llm.ReviewResponse, int, error) {
	maxAttempts := 1 + r.cfg.ChunkRetries
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := r.reviewChunks(ctx, mr, chunks, priorFindings)
		if err == nil {
			if attempt > 1 {
				logger.Info("chunk recovered after retry",
					"batch", batchNum,
					"attempts", attempt,
				)
			}
			return resp, attempt, nil
		}
		lastErr = err
		// Non-transient: propagate without retry. Parse failures
		// // and context.Canceled fall in this bucket.
		if !isTransientLLMError(err) {
			return llm.ReviewResponse{}, attempt, err
		}
		// Out of budget: stop.
		if attempt == maxAttempts {
			logger.Warn("chunk review failed; retries exhausted",
				"batch", batchNum,
				"attempts", attempt,
				"err", err.Error(),
			)
			break
		}
		// Log at Debug per issue #17 — only the final exhaustion
		// is Warn.
		logger.Debug("chunk retry on transient error",
			"batch", batchNum,
			"attempt", attempt,
			"next_attempt", attempt+1,
			"err", err.Error(),
		)
		// Respect ctx cancellation: bail out of the sleep loop
		// when the operator presses Ctrl-C.
		select {
		case <-ctx.Done():
			return llm.ReviewResponse{}, attempt, ctx.Err()
		case <-time.After(chunkBackoff(attempt - 1)):
		}
	}
	return llm.ReviewResponse{}, maxAttempts, lastErr
}

// chunkFileListAll returns the file paths in a batch without the
// "(N more)" cap — used by ChunkFailureError so the operator sees
// every file that was in the failed batch, not a truncated list.
func chunkFileListAll(chunks []llm.Chunk) []string {
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.File)
	}
	return out
}
