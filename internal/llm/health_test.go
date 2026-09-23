package llm

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
)

const modelsFixture = `{
  "object": "list",
  "data": [
    {"id": "qwen2.5-coder:7b", "object": "model", "created": 0, "owned_by": "user"},
    {"id": "llama3:8b", "object": "model", "created": 0, "owned_by": "user"},
    {"id": "mistral:7b", "object": "model", "created": 0, "owned_by": "user"}
  ]
}`

const emptyModelsFixture = `{"object":"list","data":[]}`

func TestListModels_Success(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, modelsFixture)
	p := newProvider(t, stub.URL)

	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 3 {
		t.Errorf("expected 3 models, got %d (%v)", len(models), models)
	}
	for _, want := range []string{"qwen2.5-coder:7b", "llama3:8b", "mistral:7b"} {
		if !slices.Contains(models, want) {
			t.Errorf("models missing %q", want)
		}
	}
}

func TestListModels_Empty(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, emptyModelsFixture)
	p := newProvider(t, stub.URL)

	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}
}

func TestListModels_ServerError(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusInternalServerError, `{"error":"upstream"}`)
	p := newProvider(t, stub.URL)

	_, err := p.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "list models") {
		t.Errorf("error should mention 'list models', got %v", err)
	}
}

func TestPing_Success(t *testing.T) {
	stub := newLLMStub(t)
	stub.enqueue(http.StatusOK, happyChatResponse)
	p := newProvider(t, stub.URL)

	if err := p.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}
