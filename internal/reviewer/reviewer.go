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

	"github.com/bmatcuk/doublestar/v4"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

// Config is the constructor input for Reviewer. Zero-value is
// invalid; callers should set every field.
type Config struct {
	// GitLab client (already configured with base URL + token).
	GitLab *gitlab.Client

	// LLM provider.
	LLM llm.Provider

	// Model name sent on every Chat call. Required (the OpenAI
	// API requires a model even when the provider has a default).
	Model string

	// MaxDiffBytes is the per-chunk byte budget used by
	// llm.ChunkByFile. Files exceeding this return an error
	// (the operator must split the MR manually).
	MaxDiffBytes int

	// Categories, when non-empty, override the default set in
	// the system prompt. Use to scope the review (e.g. security
	// only).
	Categories []llm.Category

	// IncludeDescription controls whether the MR description is
	// prepended to the user prompt.
	IncludeDescription bool

	// Temperature / MaxTokens are forwarded to the LLM. Zero
	// means "use the provider default".
	Temperature float64
	MaxTokens   int

	// PerChunkTimeout is the per-LLM-call timeout. 0 means no
	// per-call override (rely on context).
	PerChunkTimeout time.Duration

	// Logger receives progress lines (one per chunk fetch /
	// chunk review / post). nil falls back to slog.Default().
	Logger *slog.Logger

	// DryRun, when true, logs every GitLab POST the reviewer
	// would make and skips the actual call. The LLM still runs.
	DryRun bool

	// BotUsername is the username the reviewer's GitLab token
	// posts as. Used by the dedupe layer to ignore comments
	// authored by other humans — only the bot's prior comments
	// form the fingerprint baseline.
	//
	// When empty, every existing comment counts as the bot's
	// (useful in tests; conservative in production because
	// humans may have posted their own review notes that won't
	// match any finding).
	BotUsername string

	// CommentMode controls what gets posted to GitLab. Default
	// (zero value) is CommentModeBoth — both the summary note
	// and inline findings. Operators can restrict to one or the
	// other for cost or noise reasons.
	CommentMode CommentMode

	// IgnorePaths is a list of doublestar glob patterns. Files
	// whose path matches ANY pattern are dropped from the diff before
	// chunking. Empty means review everything.
	//
	// Supports the full doublestar syntax (`**`, `*`, `?`,
	// character classes). Common patterns:
	//   - "**/*.pb.go"          — generated protobuf files
	//   - "vendor/**"            — Go vendor directory
	//   - "**/generated/**"      — anything under any generated/
	IgnorePaths []string

	// SystemPromptSuffix is operator-supplied text appended to
	// the system prompt AFTER the system-owned schema and rules.
	// Use for documentation standards, language-specific dependency
	// preferences, team conventions, etc. Empty = no team guidance.
	//
	// The system-owned prefix (JSON schema, output format,
	// severity semantics) is always emitted and cannot be replaced.
	// Operators cannot accidentally break parsing by editing
	// this string.
	SystemPromptSuffix string

	// UserPromptSuffix is operator-supplied text appended to
	// the user prompt AFTER the diff chunks. Use for per-MR
	// context (e.g. "this PR is a WIP, focus on architecture not
	// naming"). Empty = no extra context.
	UserPromptSuffix string
}

// CommentMode selects which kinds of comments mreview posts.
// The zero value (CommentModeBoth) preserves the original
// behavior; explicit names exist for flag binding.
type CommentMode int

const (
	// CommentModeBoth posts the summary note + one inline
	// discussion per finding. This is the default behavior.
	CommentModeBoth CommentMode = iota
	// CommentModeInlineOnly posts inline discussions but
	// skips the summary note. Useful when a separate system
	// owns the summary comment.
	CommentModeInlineOnly
	// CommentModeSummaryOnly posts the summary note but skips
	// inline findings. Useful when the bot is being used as a
	// "triage only" pre-filter and a human does the line-level
	// review.
	CommentModeSummaryOnly
)

// String returns the kebab-case name for flag binding.
func (c CommentMode) String() string {
	switch c {
	case CommentModeInlineOnly:
		return "inline-only"
	case CommentModeSummaryOnly:
		return "summary-only"
	default:
		return "both"
	}
}

// ParseCommentMode parses a kebab-case name back into a
// CommentMode. Used by the CLI layer to decode --comment-mode.
func ParseCommentMode(s string) (CommentMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "both":
		return CommentModeBoth, nil
	case "inline-only", "inline":
		return CommentModeInlineOnly, nil
	case "summary-only", "summary":
		return CommentModeSummaryOnly, nil
	default:
		return CommentModeBoth, fmt.Errorf("reviewer: unknown comment-mode %q (want both|inline-only|summary-only)", s)
	}
}

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

// NewReviewer validates cfg and returns a Reviewer.
func NewReviewer(cfg Config) (*Reviewer, error) {
	if cfg.GitLab == nil {
		return nil, errors.New("reviewer: Config.GitLab is required")
	}
	if cfg.LLM == nil {
		return nil, errors.New("reviewer: Config.LLM is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("reviewer: Config.Model is required")
	}
	if cfg.MaxDiffBytes <= 0 {
		return nil, fmt.Errorf("reviewer: Config.MaxDiffBytes must be > 0, got %d", cfg.MaxDiffBytes)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Reviewer{cfg: cfg}, nil
}

// Result is what ReviewMR returns to the caller.
//
//   - MR: the GitLab merge-request projection.
//   - Findings: one per llm.Finding the reviewer tried to post.
//     Discussion is nil when the post was skipped (KindConflict,
//     file-not-in-diff, dedupe hit, etc.); Reason carries why.
//   - Summary: the summary note (or nil when --dry-run).
//   - DedupeSize: number of prior bot-authored comments that were
//     used as the dedupe baseline (0 when the MR is fresh).
type Result struct {
	MR         *gitlab.MergeRequest
	Findings   []PostedFinding
	Summary    *gitlab.Note
	DedupeSize int
}

// PostedFinding is the per-finding outcome: what we wanted to post,
// what we did post (or why we skipped it).
type PostedFinding struct {
	Finding    llm.Finding
	Discussion *gitlab.Discussion // nil if skipped
	Skipped    bool
	Reason     string // populated when Skipped
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
	chunkResponses := make([]llm.ReviewResponse, 0, len(chunks))
	for idx, chunkBatch := range batchChunks(chunks) {
		logger.Info("reviewing chunk batch", "batch", idx+1, "chunks", len(chunkBatch))

		resp, err := r.reviewChunks(ctx, mr, chunkBatch)
		if err != nil {
			// A single failed chunk doesn't fail the whole review —
			// we log and substitute an empty response so the summary
			// note still gets posted (it'll explain the gap). The
			// log carries the batch index + bounded file list so
			// operators can identify which chunk was dropped without
			// re-reading every prompt.
			logger.Warn("chunk review failed",
				"batch", idx+1,
				"files", chunkFileList(chunkBatch),
				"err", err.Error(),
			)
			chunkResponses = append(chunkResponses, llm.ReviewResponse{})
			continue
		}
		chunkResponses = append(chunkResponses, resp)
	}

	// Merge: if multiple chunk responses, ask the LLM to
	// consolidate the per-chunk summaries. If just one, use it
	// directly.
	final, err := r.consolidate(ctx, mr, chunkResponses)
	if err != nil {
		return nil, fmt.Errorf("reviewer: consolidate: %w", err)
	}

	// Filter findings whose file paths don't appear in the diff
	// (LLM hallucination guard) and drop the rest through the
	// GitLab poster.
	validFiles := pathIndex(changes)
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
			pf := r.postFinding(ctx, project, iid, mr.DiffRefs, f)
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
func (r *Reviewer) reviewChunks(ctx context.Context, mr *gitlab.MergeRequest, chunks []llm.Chunk) (llm.ReviewResponse, error) {
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
	})
	if err != nil {
		return llm.ReviewResponse{}, err
	}

	resp, err := r.cfg.LLM.Chat(ctx, llm.ChatRequest{
		System:         system,
		User:           user,
		Model:          r.cfg.Model,
		Temperature:    r.cfg.Temperature,
		MaxTokens:      r.cfg.MaxTokens,
		ResponseFormat: llm.ResponseFormatJSONObject,
		Timeout:        r.cfg.PerChunkTimeout,
	})
	if err != nil {
		return llm.ReviewResponse{}, fmt.Errorf("llm: %w", err)
	}

	parsed, err := llm.ParseReviewResponse(resp.Content)
	if err != nil {
		return llm.ReviewResponse{}, fmt.Errorf("parse: %w (raw: %s)", err, truncateForLog(resp.Content))
	}
	return parsed, nil
}

// consolidate combines per-chunk ReviewResponses into a single
// verdict. Single-chunk case returns the chunk response directly;
// multi-chunk case asks the LLM to merge the summaries.
func (r *Reviewer) consolidate(ctx context.Context, mr *gitlab.MergeRequest, chunks []llm.ReviewResponse) (llm.ReviewResponse, error) {
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

	system := "You are merging per-chunk code review outputs into one verdict. " +
		"Output ONLY a JSON object matching the ReviewResponse schema " +
		"(findings array + summary string). " +
		"Preserve every finding from the inputs — do not drop any. " +
		"Write a single, consolidated summary paragraph."

	user := fmt.Sprintf(
		"MR: !%d %q\n\nPer-chunk summaries:\n%s\n\nCombined findings (count=%d):\n%s\n\nEmit the merged JSON object.",
		mr.IID, mr.Title, summaries.String(), len(allFindings), formatFindings(allFindings),
	)

	resp, err := r.cfg.LLM.Chat(ctx, llm.ChatRequest{
		System:         system,
		User:           user,
		Model:          r.cfg.Model,
		Temperature:    r.cfg.Temperature,
		MaxTokens:      r.cfg.MaxTokens,
		ResponseFormat: llm.ResponseFormatJSONObject,
		Timeout:        r.cfg.PerChunkTimeout,
	})
	if err != nil {
		// Fallback: assemble manually so the user gets the
		// findings even if the merge call fails.
		r.cfg.Logger.Warn("merge call failed; using raw concatenation",
			"err", err.Error(),
		)
		return llm.ReviewResponse{
			Findings: allFindings,
			Summary:  strings.TrimSpace(summaries.String()),
		}, nil
	}

	parsed, err := llm.ParseReviewResponse(resp.Content)
	if err != nil {
		return llm.ReviewResponse{
			Findings: allFindings,
			Summary:  strings.TrimSpace(summaries.String()),
		}, nil
	}
	// If the merge dropped findings, restore them. Conservative:
	// the union of inputs wins.
	if len(parsed.Findings) < len(allFindings) {
		parsed.Findings = dedupeFindings(append(parsed.Findings, allFindings...))
	}
	return parsed, nil
}

// batchChunks groups consecutive chunks into batches. For now we
// put every chunk in its own batch (one call per chunk) — this
// keeps the implementation simple and the prompts focused. Future
// optimization: pack small chunks together when the sum fits.
func batchChunks(chunks []llm.Chunk) [][]llm.Chunk {
	if len(chunks) == 0 {
		return nil
	}
	out := make([][]llm.Chunk, len(chunks))
	for i, c := range chunks {
		out[i] = []llm.Chunk{c}
	}
	return out
}

// chunkFileList renders the file paths in a batch as a comma-
// separated string suitable for log output. Capped at the first
// `maxChunkLogFiles` entries with an "(N more)" tail to keep log
// lines bounded when a batch contains many chunks. Today
// batchChunks emits one chunk per batch, but the batching API
// supports packing small chunks together — this helper stays
// correct if that optimization lands later.
func chunkFileList(chunks []llm.Chunk) string {
	const maxChunkLogFiles = 5
	if len(chunks) == 0 {
		return ""
	}
	files := make([]string, 0, len(chunks))
	for i, c := range chunks {
		if i >= maxChunkLogFiles {
			files = append(files, fmt.Sprintf("(%d more)", len(chunks)-maxChunkLogFiles))
			break
		}
		files = append(files, c.File)
	}
	return strings.Join(files, ",")
}

// pathIndex returns the set of file paths present in the diff.
// Used to filter out LLM-hallucinated paths.
func pathIndex(changes []gitlab.ChangeFile) map[string]bool {
	out := make(map[string]bool, len(changes))
	for _, c := range changes {
		out[c.Path()] = true
	}
	return out
}

// filterFindings drops findings that:
//   - cite a file path not in the diff (hallucination)
//   - have an empty body
//   - have an out-of-range line (negative)
//
// The remaining findings pass through to the poster.
func filterFindings(in []llm.Finding, validFiles map[string]bool, logger *slog.Logger) []llm.Finding {
	out := make([]llm.Finding, 0, len(in))
	for _, f := range in {
		if strings.TrimSpace(f.Body) == "" {
			logger.Debug("drop finding: empty body", "file", f.File)
			continue
		}
		if !validFiles[f.File] {
			logger.Warn("drop finding: file not in diff (LLM hallucination?)",
				"file", f.File, "line", f.Line,
			)
			continue
		}
		if f.Line < 0 {
			logger.Debug("drop finding: negative line", "file", f.File, "line", f.Line)
			continue
		}
		out = append(out, f)
	}
	return out
}

// formatFindings renders a findings list as a compact block for
// the merge prompt. One finding per line.
func formatFindings(fs []llm.Finding) string {
	var b strings.Builder
	for i, f := range fs {
		fmt.Fprintf(&b, "%d. %s:%d [%s/%s] %s\n",
			i+1, f.File, f.Line, f.Severity, f.Category, f.Body)
	}
	return b.String()
}

// dedupeFindings collapses findings that share (file, line, body).
// Used by consolidate when the merge dropped entries.
func dedupeFindings(in []llm.Finding) []llm.Finding {
	seen := make(map[string]bool, len(in))
	out := make([]llm.Finding, 0, len(in))
	for _, f := range in {
		key := strings.TrimSpace(f.File) + "|" + fmt.Sprint(f.Line) + "|" + strings.TrimSpace(f.Body)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

// dedupeAgainstSet drops findings whose body fingerprint already
// exists in the bot's prior comments. Returns the surviving
// findings.
func dedupeAgainstSet(in []llm.Finding, fpSet *fingerprintSet, logger *slog.Logger) []llm.Finding {
	out := make([]llm.Finding, 0, len(in))
	for _, f := range in {
		if fpSet.Contains(f) {
			logger.Debug("dedupe: skip finding (already posted)",
				"file", f.File, "line", f.Line,
			)
			continue
		}
		out = append(out, f)
	}
	return out
}

// countPosted returns the number of PostedFinding entries with a
// non-nil Discussion.
func countPosted(pfs []PostedFinding) int {
	n := 0
	for _, pf := range pfs {
		if pf.Discussion != nil {
			n++
		}
	}
	return n
}

// truncateForLog is a small helper for logging the raw LLM output
// when parsing fails. Bounded so we don't blow up logs.
func truncateForLog(s string) string {
	const logMax = 1000
	if len(s) <= logMax {
		return s
	}
	return s[:logMax] + "...(truncated)"
}

// shouldPostSummary decides whether to post the summary note for
// this review action.
//
//   - "open" / "reopen" → post (the MR is freshly active; the
//     human reviewer benefits from a fresh verdict).
//   - "update" → skip (every push would pile up a fresh
//     summary in the timeline; inline findings carry the per-
//     push signal).
//   - empty / unknown → post (the standalone `mreview review`
//     CLI is operator-driven; they expect a complete report).
func shouldPostSummary(action string) bool {
	switch action {
	case "update":
		return false
	default:
		// "open", "reopen", "", anything else
		return true
	}
}

// filterChangesByPath drops change files whose path matches any
// of the supplied glob patterns. Returns a new slice; input is
// unchanged.
//
// Pattern semantics:
//   - `*.pb.go` — exact match against any path segment (Go's
//     path.Match)
//   - `vendor/**` — matches anything under vendor/ (we translate
//     `**` into "any characters")
//   - `**/generated/**` — matches anywhere in the tree
//
// We don't pull in a full doublestar library to keep the binary
// small — the common patterns (vendor/, *.pb.go, generated/) all
// work with the simple translation below.
func filterChangesByPath(in []gitlab.ChangeFile, patterns []string) []gitlab.ChangeFile {
	if len(patterns) == 0 {
		return in
	}
	out := make([]gitlab.ChangeFile, 0, len(in))
	for _, c := range in {
		if matchesAny(c.Path(), patterns) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// matchesAny reports whether path matches any doublestar glob
// pattern. Patterns support the full doublestar syntax
// (`**/*.pb.go`, `vendor/**`, `**/generated/**`, character
// classes, etc.).
//
// A malformed pattern (e.g. unmatched `[`) is silently skipped
// — operators can fix the pattern, but a typo shouldn't silently
// drop files from the review.
func matchesAny(path string, patterns []string) bool {
	for _, pattern := range patterns {
		ok, err := doublestar.PathMatch(pattern, path)
		if err != nil {
			continue
		}
		if ok {
			return true
		}
	}
	return false
}
