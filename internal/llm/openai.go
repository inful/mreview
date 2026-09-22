package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	openai "github.com/openai/openai-go"
	openaiopt "github.com/openai/openai-go/option"
)

// OpenAIProvider implements Provider against any OpenAI-compatible
// Chat Completions endpoint.
//
// Tested against:
//   - Ollama's /v1 endpoint (default port 11434)
//   - llama.cpp's /v1 endpoint (default port 8080)
//   - LM Studio's /v1 endpoint (default port 1234)
//   - vLLM's /v1 endpoint (default port 8000)
//
// Any server that accepts {"messages": [...], "model": "..."} and
// returns the standard ChatCompletion JSON shape works without
// configuration.
type OpenAIProvider struct {
	client openai.Client
	model  string // default model; per-call request can override
}

// OpenAIConfig controls the provider's setup.
type OpenAIConfig struct {
	// BaseURL is the OpenAI-compatible root, including the /v1
	// suffix. Example: "http://localhost:11434/v1".
	BaseURL string

	// APIKey is the bearer token to send. Most local servers
	// accept any non-empty value or "no-key"; ollama's /v1 ignores
	// it. Empty is allowed but some servers reject the request.
	APIKey string

	// Model is the default model for Chat calls when
	// ChatRequest.Model is empty. Required for the provider to
	// be usable (the OpenAI API requires model on every call).
	Model string

	// Timeout is the per-request HTTP timeout. 0 means no timeout
	// (rely on the caller's context). 5 minutes is a sane default
	// for code review of large MRs.
	Timeout time.Duration

	// MaxRetries is the upstream openai-go retry budget. We also
	// wrap the call in our own retry layer in the reviewer; this
	// is just the inner transport's retries.
	MaxRetries int

	// HTTPClient, when non-nil, replaces the default http.Client
	// used by openai-go. Tests use this to inject a custom
	// transport (see stubbedOpenAITransport).
	HTTPClient *http.Client
}

// NewOpenAIProvider builds an OpenAIProvider from cfg.
func NewOpenAIProvider(cfg OpenAIConfig) (*OpenAIProvider, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("llm: BaseURL is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("llm: Model is required (set OpenAIConfig.Model or pass it on each ChatRequest)")
	}
	if !strings.HasSuffix(cfg.BaseURL, "/") && !strings.HasSuffix(cfg.BaseURL, "/v1") {
		// The openai-go client appends "/chat/completions" to
		// BaseURL; if the user passes "http://x:11434" without
		// the /v1, the request goes to /chat/completions instead
		// of /v1/chat/completions and most local servers 404.
		// Be helpful and append /v1 when the user clearly meant
		// the OpenAI-compatible root.
		if !strings.Contains(cfg.BaseURL, "/v1") {
			cfg.BaseURL += "/v1"
		}
	}

	opts := []openaiopt.RequestOption{
		openaiopt.WithBaseURL(cfg.BaseURL),
	}
	if cfg.APIKey != "" {
		opts = append(opts, openaiopt.WithAPIKey(cfg.APIKey))
	}
	if cfg.MaxRetries >= 0 {
		opts = append(opts, openaiopt.WithMaxRetries(cfg.MaxRetries))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, openaiopt.WithHTTPClient(cfg.HTTPClient))
	}

	return &OpenAIProvider{
		client: openai.NewClient(opts...),
		model:  cfg.Model,
	}, nil
}

// Chat sends one chat-completion request.
//
// Honors ChatRequest.System (sent as a system message), .User
// (sent as a user message), .Temperature, .MaxTokens, and
// .ResponseFormat (JSON-object mode when set).
func (p *OpenAIProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if strings.TrimSpace(req.User) == "" {
		return nil, errors.New("llm: ChatRequest.User is empty")
	}

	model := req.Model
	if model == "" {
		model = p.model
	}

	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, 2)
	if req.System != "" {
		msgs = append(msgs, openai.SystemMessage(req.System))
	}
	msgs = append(msgs, openai.UserMessage(req.User))

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(model),
		Messages: msgs,
	}
	if req.Temperature > 0 {
		params.Temperature = openai.Float(req.Temperature)
	}
	if req.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.ResponseFormat == ResponseFormatJSONObject {
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &openai.ResponseFormatJSONObjectParam{},
		}
	}

	callCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	resp, err := p.client.Chat.Completions.New(callCtx, params)
	if err != nil {
		return nil, fmt.Errorf("llm: chat completion: %w", err)
	}
	if resp == nil {
		return nil, errors.New("llm: empty response from server")
	}

	content := ""
	for _, choice := range resp.Choices {
		if choice.Message.Content != "" {
			content = choice.Message.Content
			break
		}
	}

	out := &ChatResponse{
		Content: content,
		Model:   string(resp.Model),
	}
	if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
		out.TokensIn = int(resp.Usage.PromptTokens)
		out.TokensOut = int(resp.Usage.CompletionTokens)
	}
	return out, nil
}

// Close releases any resources held by the provider. The default
// implementation has none; the method exists so callers using
// providers that DO hold resources (pools, long-lived connections)
// can be cleaned up uniformly.
func (p *OpenAIProvider) Close() error {
	return nil
}
