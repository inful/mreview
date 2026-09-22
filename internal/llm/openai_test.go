package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// llmStub is a tiny OpenAI-compatible chat-completions stub.
// Records every request and lets tests script the response stream
// (success, JSON schema mode, error, etc.).
type llmStub struct {
	*httptest.Server
	requests  []stubRequest
	responses []stubResponse
}

type stubRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

type stubResponse struct {
	status int
	body   string
}

func newLLMStub(t *testing.T) *llmStub {
	t.Helper()
	stub := &llmStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stub.requests = append(stub.requests, stubRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   string(body),
		})

		if len(stub.responses) == 0 {
			http.Error(w, "stub: no response queued", http.StatusInternalServerError)
			return
		}
		next := stub.responses[0]
		stub.responses = stub.responses[1:]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(next.status)
		_, _ = io.WriteString(w, next.body)
	}))
	t.Cleanup(stub.Close)
	return stub
}

func (s *llmStub) enqueue(status int, body string) {
	s.responses = append(s.responses, stubResponse{status: status, body: body})
}

const happyChatResponse = `{
  "id": "chatcmpl-1",
  "model": "qwen2.5-coder:7b",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "{\"findings\":[],\"summary\":\"LGTM\"}"},
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 100,
    "completion_tokens": 50,
    "total_tokens": 150
  }
}`

func newProvider(t *testing.T, baseURL string) *OpenAIProvider {
	t.Helper()
	p, err := NewOpenAIProvider(OpenAIConfig{
		BaseURL:    baseURL,
		APIKey:     "test-key",
		Model:      "qwen2.5-coder:7b",
		MaxRetries: 0, // we own retries at the caller layer
	})
	if err != nil {
		t.Fatalf("NewOpenAIProvider: %v", err)
	}
	return p
}

func TestOpenAIProvider_ConfigValidation(t *testing.T) {
	if _, err := NewOpenAIProvider(OpenAIConfig{Model: "x"}); err == nil {
		t.Error("expected error for empty BaseURL")
	}
	if _, err := NewOpenAIProvider(OpenAIConfig{BaseURL: "http://x"}); err == nil {
		t.Error("expected error for empty Model")
	}
}

func TestChat_HappyPath(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, happyChatResponse)
	p := newProvider(t, stub.URL)

	got, err := p.Chat(context.Background(), ChatRequest{
		System: "You are a code reviewer.",
		User:   "Review the diff at internal/x.go:42.",
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(got.Content, "LGTM") {
		t.Errorf("Content = %q", got.Content)
	}
	if got.TokensIn != 100 || got.TokensOut != 50 {
		t.Errorf("usage = in=%d out=%d, want 100/50", got.TokensIn, got.TokensOut)
	}
	if got.Model != "qwen2.5-coder:7b" {
		t.Errorf("Model = %q", got.Model)
	}
}

func TestChat_RequestShape(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, happyChatResponse)
	p := newProvider(t, stub.URL)

	_, err := p.Chat(context.Background(), ChatRequest{
		System:         "sys",
		User:           "user",
		Model:          "gpt-x",
		Temperature:    0.3,
		MaxTokens:      256,
		ResponseFormat: ResponseFormatJSONObject,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	req := stub.requests[0]
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if !strings.HasSuffix(req.Path, "/chat/completions") {
		t.Errorf("path = %q", req.Path)
	}
	// Authorization header must carry the API key (Authorization:
	// Bearer <key>) for the standard OpenAI-compatible header.
	if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(req.Body), &sent); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, req.Body)
	}
	if sent["model"] != "gpt-x" {
		t.Errorf("model = %v", sent["model"])
	}
	if sent["temperature"] != 0.3 {
		t.Errorf("temperature = %v", sent["temperature"])
	}
	if int(sent["max_completion_tokens"].(float64)) != 256 {
		t.Errorf("max_completion_tokens = %v", sent["max_completion_tokens"])
	}
	// ResponseFormat should appear as {"type":"json_object"}.
	rf, ok := sent["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want {type: json_object}", sent["response_format"])
	}
	// Messages should be [system, user].
	msgs, ok := sent["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v", sent["messages"])
	}
	for i, want := range []string{"system", "user"} {
		m := msgs[i].(map[string]any)
		if m["role"] != want || m["content"] == "" {
			t.Errorf("messages[%d] = %+v", i, m)
		}
	}
}

func TestChat_DefaultModel(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, happyChatResponse)
	p := newProvider(t, stub.URL)

	// ChatRequest.Model empty → use provider default.
	_, err := p.Chat(context.Background(), ChatRequest{User: "hi"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(stub.requests[0].Body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if sent["model"] != "qwen2.5-coder:7b" {
		t.Errorf("model = %v, want provider default", sent["model"])
	}
}

func TestChat_NoSystemMessage(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, happyChatResponse)
	p := newProvider(t, stub.URL)

	_, err := p.Chat(context.Background(), ChatRequest{User: "hi"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(stub.requests[0].Body), &sent)
	msgs := sent["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("expected 1 message (no system), got %d", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("first message role = %v, want user", msgs[0])
	}
}

func TestChat_EmptyUser(t *testing.T) {
	p := newProvider(t, "http://unused")
	_, err := p.Chat(context.Background(), ChatRequest{})
	if err == nil {
		t.Error("expected error for empty user message")
	}
}

func TestChat_ServerError(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusBadGateway, `{"error":{"message":"upstream down"}}`)
	p := newProvider(t, stub.URL)

	_, err := p.Chat(context.Background(), ChatRequest{User: "hi"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "chat completion") {
		t.Errorf("error should mention chat completion, got %v", err)
	}
}

func TestChat_EmptyChoices(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, `{"id":"x","model":"m","choices":[],"usage":{}}`)
	p := newProvider(t, stub.URL)

	got, err := p.Chat(context.Background(), ChatRequest{User: "hi"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got.Content != "" {
		t.Errorf("Content = %q, want empty", got.Content)
	}
	if got.TokensIn != 0 || got.TokensOut != 0 {
		t.Errorf("usage should be zero, got in=%d out=%d", got.TokensIn, got.TokensOut)
	}
}

func TestChat_TimeoutFromRequest(t *testing.T) {
	// Stub that delays its response; the per-request timeout fires.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, happyChatResponse)
	}))
	t.Cleanup(stub.Close)

	p := newProvider(t, stub.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := p.Chat(ctx, ChatRequest{User: "hi", Timeout: 0})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestChat_TimeoutFromRequestOverridesContext(t *testing.T) {
	// Per-request Timeout shorter than context should win.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, happyChatResponse)
	}))
	t.Cleanup(stub.Close)

	p := newProvider(t, stub.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := p.Chat(ctx, ChatRequest{User: "hi", Timeout: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// TestChat_DefaultUsageIsZero documents that an absent Usage block
// yields zero tokens rather than an error — important for local
// servers that don't report usage.
func TestChat_DefaultUsageIsZero(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
	p := newProvider(t, stub.URL)
	got, err := p.Chat(context.Background(), ChatRequest{User: "hi"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got.TokensIn != 0 || got.TokensOut != 0 {
		t.Errorf("usage should be zero, got in=%d out=%d", got.TokensIn, got.TokensOut)
	}
}

// TestAppendsV1WhenMissing_NoTrailingSlash: the provider appends
// /v1 to baseURLs that don't already have it. Verifies that the
// resulting call hits /v1/chat/completions on the stub.
func TestAppendsV1WhenMissing_NoTrailingSlash(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, happyChatResponse)
	// stub.URL is like "http://127.0.0.1:NNNN". Strip any /v1
	// suffix (there shouldn't be one) and pass it without the
	// suffix.
	base := strings.TrimSuffix(stub.URL, "/v1")
	p, err := NewOpenAIProvider(OpenAIConfig{
		BaseURL: base,
		APIKey:  "k",
		Model:   "m",
	})
	if err != nil {
		t.Fatalf("NewOpenAIProvider: %v", err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{User: "hi"}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	// The path must end in /v1/chat/completions.
	if !strings.HasSuffix(stub.requests[0].Path, "/v1/chat/completions") {
		t.Errorf("path = %q, want suffix /v1/chat/completions", stub.requests[0].Path)
	}
}

// TestAppendsV1WhenMissing_AlreadyHasV1: baseURL with /v1 should NOT
// get a second /v1 appended.
func TestAppendsV1WhenMissing_AlreadyHasV1(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, happyChatResponse)
	// stub.URL is already the bare origin. Append /v1 ourselves.
	base := stub.URL + "/v1"
	p, err := NewOpenAIProvider(OpenAIConfig{
		BaseURL: base,
		APIKey:  "k",
		Model:   "m",
	})
	if err != nil {
		t.Fatalf("NewOpenAIProvider: %v", err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{User: "hi"}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.HasSuffix(stub.requests[0].Path, "/v1/chat/completions") {
		t.Errorf("path = %q, want suffix /v1/chat/completions", stub.requests[0].Path)
	}
	if strings.Contains(stub.requests[0].Path, "/v1/v1/") {
		t.Errorf("path doubled /v1: %q", stub.requests[0].Path)
	}
}

// silence unused-import lint for fmt (used in error messages from
// the provider).
var _ = fmt.Sprintf
