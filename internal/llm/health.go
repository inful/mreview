package llm

import (
	"context"
	"fmt"
)

// ListModels returns the IDs of every model the configured LLM
// server reports via /v1/models. Useful for `mreview doctor` and
// for verifying that the configured default model is actually
// available on the server (vs. a typo'd or uninstalled model).
//
// Each local LLM server has slightly different model metadata:
//
//   - Ollama: returns a long list including every pulled model.
//   - llama.cpp: returns a single model matching the loaded one.
//   - LM Studio: returns a single model matching the active load.
//   - vLLM: returns every registered model.
//
// Errors are returned raw from the underlying openai-go client;
// callers can errors.As on openai.APIError for status details.
func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	page, err := p.client.Models.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("llm: list models: %w", err)
	}
	out := make([]string, 0, len(page.Data))
	for _, m := range page.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Ping sends a tiny chat request to verify connectivity. Used by
// `mreview doctor` as a fallback when /v1/models isn't supported
// (e.g. an OpenAI-compatible endpoint that only implements
// /chat/completions).
//
// The model defaults to the provider's configured default; callers
// can override via req.Model.
func (p *OpenAIProvider) Ping(ctx context.Context) error {
	_, err := p.Chat(ctx, ChatRequest{
		User:      "ping",
		Model:     p.model,
		MaxTokens: 1,
	})
	return err
}
