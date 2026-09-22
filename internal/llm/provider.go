// Package llm talks to local LLM endpoints (Ollama, llama.cpp, vLLM,
// LM Studio — anything that speaks the OpenAI Chat Completions API).
//
// Architecture:
//
//   - Provider is the interface the reviewer depends on. The default
//     implementation is OpenAIProvider (covers every OpenAI-compatible
//     endpoint via openai-go + a custom base URL).
//
//   - ChatRequest / ChatResponse are the platform-neutral types.
//
//   - parse.go handles the resilient JSON parser that turns the LLM's
//     text response into a ReviewResponse. Local models occasionally
//     emit prose around the JSON, so the parser is layered:
//
//     1. raw JSON (preferred path)
//     2. extract ```json ... ``` fenced block
//     3. loose bracket extraction as a last resort
//
//   - chunk.go (lands in phase 4) splits a diff into per-file
//     chunks that fit the LLM's context budget.
//
// The package is consumed by internal/reviewer (phase 5), which
// orchestrates the full fetch → chunk → review → post pipeline.
package llm

import (
	"context"
	"time"
)

// Provider is the interface the reviewer depends on.
//
// One implementation today (OpenAIProvider); additional backends
// (e.g. raw llama.cpp without an OpenAI shim) can plug in later
// without touching the reviewer.
type Provider interface {
	// Chat sends one chat-completion request and returns the model's
	// text reply. Implementations decide whether to honor
	// ResponseFormat (most local LLMs accept JSON-object mode; a
	// few ignore it).
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// ChatRequest is the platform-neutral input to Provider.Chat.
type ChatRequest struct {
	System         string         // system prompt (or empty)
	User           string         // user prompt (or empty)
	Model          string         // model name (e.g. "qwen2.5-coder:7b")
	Temperature    float64        // 0.0–2.0; 0 means use server default
	MaxTokens      int            // upper bound; 0 means use server default
	ResponseFormat ResponseFormat // JSON object mode, or text mode
	Timeout        time.Duration  // per-request; 0 means no override
}

// ResponseFormat asks the model to constrain its output.
//
// JSONObject is the only one most local LLMs honor. Local models
// that don't recognize it will just produce JSON anyway, so the
// resilient parser still kicks in.
type ResponseFormat int

const (
	// ResponseFormatText is the default; the model is free to emit
	// any text (we still try to parse JSON from it).
	ResponseFormatText ResponseFormat = iota
	// ResponseFormatJSONObject asks the model to emit a JSON
	// object (response_format={"type":"json_object"}).
	ResponseFormatJSONObject
)

// ChatResponse is the platform-neutral output from Provider.Chat.
type ChatResponse struct {
	// Content is the model's raw text reply. May be JSON, may be
	// prose with JSON inside, may be prose with no JSON at all —
	// the resilient parser handles all three.
	Content string

	// TokensIn / TokensOut are the usage stats when the server
	// reports them. Zero values mean "not reported".
	TokensIn  int
	TokensOut int

	// Model is the model name as the server reported it (may
	// differ from the request when the server aliases).
	Model string
}

// Severity is the finding-importance enum. Anything not in this set
// is treated as SeverityInfo by the reviewer.
type Severity string

const (
	// SeverityInfo marks a non-blocking comment (nit, suggestion).
	SeverityInfo Severity = "info"
	// SeverityWarning marks a real issue that should be addressed
	// before merge.
	SeverityWarning Severity = "warning"
	// SeverityError marks a blocker (broken behaviour, security
	// hole).
	SeverityError Severity = "error"
)

// Category is a coarse label the LLM assigns to each finding. The
// reviewer doesn't enforce a vocabulary — any string is accepted —
// but the prompt template steers the model toward this set.
type Category string

const (
	// CategorySecurity covers authentication, authorization,
	// injection, secret leakage.
	CategorySecurity Category = "security"
	// CategoryCorrectness covers logic bugs, race conditions,
	// off-by-one, type misuse.
	CategoryCorrectness Category = "correctness"
	// CategoryStyle covers naming, formatting, idioms. Lowest
	// priority.
	CategoryStyle Category = "style"
	// CategoryPerf covers allocations, blocking calls, hot-path
	// complexity.
	CategoryPerf Category = "perf"
	// CategoryTest covers missing or weak test coverage.
	CategoryTest Category = "test"
	// CategoryDocs covers comments, READMEs, godoc.
	CategoryDocs Category = "docs"
)

// Finding is one actionable item the LLM surfaces from the diff.
type Finding struct {
	File       string   `json:"file"`                 // path at HEAD
	Line       int      `json:"line"`                 // 1-indexed; 0 if N/A
	Severity   Severity `json:"severity,omitempty"`   // info / warning / error
	Category   Category `json:"category,omitempty"`   // free-form but steered
	Body       string   `json:"body"`                 // markdown; required
	Suggestion string   `json:"suggestion,omitempty"` // code block; optional
}

// ReviewResponse is what the LLM is asked to produce.
//
// The schema is deliberately flat: one Findings array and one
// Summary string. The reviewer never inspects anything else, so
// adding new fields won't break existing callers.
type ReviewResponse struct {
	Findings []Finding `json:"findings"`
	Summary  string    `json:"summary"`
}
