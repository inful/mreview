package provider

import (
	"context"
	"testing"
)

// TestBuild_AllProviders verifies every recognised provider
// name resolves to a non-nil llm.LLMProvider. We don't make
// any network calls — the harness library's constructor
// doesn't dial out, it just sets up the client.
//
// "Local" needs only BaseURL (no API key). All others take
// an API key and (optionally) a base URL.
func TestBuild_AllProviders(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "anthropic",
			cfg:  Config{Name: Anthropic, APIKey: "test-key"},
		},
		{
			name: "openai",
			cfg:  Config{Name: OpenAI, APIKey: "test-key"},
		},
		{
			name: "gemini",
			cfg:  Config{Name: Gemini, APIKey: "test-key"},
		},
		{
			name: "litellm",
			cfg:  Config{Name: LiteLLM, APIKey: "test-key", BaseURL: "http://localhost:4000/v1"},
		},
		{
			name: "openrouter",
			cfg:  Config{Name: OpenRouter, APIKey: "test-key"},
		},
		{
			name: "local",
			cfg:  Config{Name: Local, BaseURL: "http://localhost:11434/v1"},
		},
		{
			name:    "unknown",
			cfg:     Config{Name: Name("nope")},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(string(c.name), func(t *testing.T) {
			p, err := Build(context.Background(), c.cfg)
			if c.wantErr {
				if err == nil {
					t.Errorf("Build(%s) returned no error; expected one",
						c.cfg.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build(%s): %v", c.cfg.Name, err)
			}
			if p == nil {
				t.Errorf("Build(%s) returned nil provider with no error",
					c.cfg.Name)
			}
		})
	}
}

// TestName_Valid pins the recognised provider names. Adding a
// provider? Add a constant, update this test, update Build.
func TestName_Valid(t *testing.T) {
	cases := map[Name]bool{
		Anthropic:             true,
		OpenAI:                true,
		Gemini:                true,
		LiteLLM:               true,
		OpenRouter:            true,
		Local:                 true,
		Name(""):              false,
		Name("gpt"):           false,
		Name("ANTHROPIC"):     false, // case-sensitive
		Name("anthropic-pro"): false, // exact-match, not prefix
	}
	for n, want := range cases {
		t.Run(string(n), func(t *testing.T) {
			if got := n.Valid(); got != want {
				t.Errorf("(%q).Valid() = %v, want %v", string(n), got, want)
			}
		})
	}
}
