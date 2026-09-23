// Package provider builds a harness llm.Provider from a name +
// config. The matrix is fixed: six providers, one entry point.
//
// Each provider constructor lives in
// github.com/sausheong/harness/providers/<name>. The harness
// library owns the wire format and streaming; mreview only
// chooses which constructor to call.
//
// Reference: issue #42, architecture reset, migration step 3.
package provider

import (
	"context"
	"fmt"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/harness/providers/anthropic"
	"github.com/sausheong/harness/providers/gemini"
	"github.com/sausheong/harness/providers/litellm"
	"github.com/sausheong/harness/providers/local"
	"github.com/sausheong/harness/providers/openai"
	"github.com/sausheong/harness/providers/openrouter"
)

// Name identifies one of the six supported providers. The
// string values match the CLI's --provider flag enum.
type Name string

const (
	// Anthropic — Anthropic's Claude models (api.anthropic.com
	// by default; configurable base URL).
	Anthropic Name = "anthropic"

	// OpenAI — OpenAI's Chat Completions API (api.openai.com).
	OpenAI Name = "openai"

	// Gemini — Google's Gemini via ADC.
	Gemini Name = "gemini"

	// LiteLLM — a self-hosted LiteLLM proxy (OpenAI-compatible).
	LiteLLM Name = "litellm"

	// OpenRouter — the OpenRouter aggregator (OpenAI-compatible).
	OpenRouter Name = "openrouter"

	// Local — local model server (Ollama, LM Studio, llama.cpp,
	// vLLM); no API key required.
	Local Name = "local"
)

// Valid reports whether n is one of the recognised providers.
// Used by the CLI's enum binding to reject typos early.
func (n Name) Valid() bool {
	switch n {
	case Anthropic, OpenAI, Gemini, LiteLLM, OpenRouter, Local:
		return true
	default:
		return false
	}
}

// Config is the input to Build. Fields are passed straight
// through to the harness provider constructor.
type Config struct {
	Name    Name
	APIKey  string // empty for local-only servers
	BaseURL string // provider-specific; some ignore it
}

// Build returns the harness llm.Provider for cfg. The Gemini
// constructor takes a context (for ADC); all others ignore it.
//
// The returned provider is the harness library's own
// llm.LLMProvider interface. mreview's orchestrator passes it
// to runtime.BuildRuntime.
//
// Per the architecture reset (#42), mreview no longer maintains
// the provider matrix — harness owns it. mreview's only
// responsibility here is the dispatch.
func Build(ctx context.Context, cfg Config) (llm.LLMProvider, error) {
	switch cfg.Name {
	case Anthropic:
		return anthropic.NewAnthropicProvider(cfg.APIKey, cfg.BaseURL), nil
	case OpenAI:
		return openai.NewOpenAIProvider(cfg.APIKey, cfg.BaseURL), nil
	case Gemini:
		return gemini.NewGeminiProvider(ctx, cfg.APIKey)
	case LiteLLM:
		return litellm.NewLiteLLMProvider(cfg.APIKey, cfg.BaseURL), nil
	case OpenRouter:
		return openrouter.NewOpenRouterProvider(cfg.APIKey, cfg.BaseURL), nil
	case Local:
		return local.NewLocalProvider(cfg.BaseURL), nil
	default:
		return nil, fmt.Errorf("provider: unknown name %q", cfg.Name)
	}
}
