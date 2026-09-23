package reviewer

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

	// MaxBatchBytes is the byte budget for packing multiple
	// chunks into one LLM call. When > 0, the reviewer greedily
	// groups consecutive chunks whose total size fits within this
	// budget, reducing call count for operators with large-
	// context models. When 0 (the default), each chunk gets its
	// own LLM call (the historical behaviour).
	//
	// Operators with high-context models should set this to
	// roughly (context_window_tokens - max_tokens -
	// prompt_overhead) * bytes_per_token. A preset layer
	// computing this from a declared context window is filed as
	// a follow-up.
	MaxBatchBytes int

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

	// ReasoningEffort controls the reasoning budget for
	// o-series-style models. Empty means "use the server's
	// default". Forwarded to llm.ChatRequest.ReasoningEffort;
	// providers that don't support the field ignore it.
	ReasoningEffort llm.ReasoningEffort

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

	// ChunkRetries is the chunk-level retry budget. When a chunk
	// call returns a transient error (currently: per-call
	// timeout, see isTransientLLMError), the reviewer retries
	// the same chunk up to ChunkRetries more times before
	// giving up. The total attempt count per chunk is
	// 1 + ChunkRetries. Default 1 (set by validateConfig when
	// zero). Negative is rejected by NewReviewer.
	//
	// The chunk-level retry catches the timeout case that
	// bubbles past the openai-go client's own MaxRetries=2;
	// see isTransientLLMError for the full classification.
	ChunkRetries int

	// AllowPartial is the escape hatch for the atomic-failure
	// default. When false (the default), ANY chunk that fails
	// after retries aborts the whole review: ReviewMR returns
	// *ChunkFailureError before posting any summary note or
	// inline discussion. Operators running on 50-file MRs who
	// would rather have a half-complete review than a hard
	// failure can set this true and to the legacy behaviour:
	// log a WARN, substitute an empty ReviewResponse, and
	// continue with the chunks that did succeed.
	//
	// See issue #31 for the rationale.
	AllowPartial bool
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

// validateConfig centralises the NewReviewer invariants so the
// constructor stays small. It also populates defaults so callers
// can rely on zero-value behaviour without separate setup:
//
//   - Logger: slog.Default() when nil
//   - ChunkRetries: 1 when zero (atomic-failure flow needs a retry
//     budget; the historical "no retries" was effectively one attempt)
//
// AllowPartial defaults to false (atomic-failure is the safe
// default; operators opt out explicitly).
//
// Returns the populated Config so the caller can store the
// defaulted values rather than the zero values they passed in.
// Passed by value because we want to mutate the local copy and
// return it; the caller's struct stays untouched.
func validateConfig(cfg Config) (Config, error) {
	if cfg.GitLab == nil {
		return cfg, errors.New("reviewer: Config.GitLab is required")
	}
	if cfg.LLM == nil {
		return cfg, errors.New("reviewer: Config.LLM is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return cfg, errors.New("reviewer: Config.Model is required")
	}
	if cfg.MaxDiffBytes <= 0 {
		return cfg, fmt.Errorf("reviewer: Config.MaxDiffBytes must be > 0, got %d", cfg.MaxDiffBytes)
	}
	if cfg.ChunkRetries < 0 {
		return cfg, fmt.Errorf("reviewer: Config.ChunkRetries must be >= 0, got %d", cfg.ChunkRetries)
	}
	if cfg.ChunkRetries == 0 {
		cfg.ChunkRetries = 1
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return cfg, nil
}
