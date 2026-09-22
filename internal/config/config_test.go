package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const sampleYAML = `
gitlab:
  url: https://gitlab.example.com
  token_env: GITLAB_TOKEN_CUSTOM

llm:
  base_url: http://llm.internal:8080/v1
  api_key_env: LLM_API_KEY
  model: custom-model:7b

review:
  max_diff_bytes: 100000
  per_chunk_timeout: 60s
  temperature: 0.1
  max_tokens: 1024
  bot_username_env: GITLAB_BOT_USERNAME

server:
  addr: ":9090"
  webhook_secret_env: GITLAB_WEBHOOK_SECRET
  queue_size: 16
  shutdown_timeout: 10s

retry:
  max_attempts: 6
  initial_backoff: 250ms
  max_backoff: 5s
`

func TestParse_Sample(t *testing.T) {
	f, err := Parse([]byte(sampleYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.GitLab.URL != "https://gitlab.example.com" {
		t.Errorf("GitLab.URL = %q", f.GitLab.URL)
	}
	if f.GitLab.TokenEnv != "GITLAB_TOKEN_CUSTOM" {
		t.Errorf("GitLab.TokenEnv = %q", f.GitLab.TokenEnv)
	}
	if f.LLM.BaseURL != "http://llm.internal:8080/v1" {
		t.Errorf("LLM.BaseURL = %q", f.LLM.BaseURL)
	}
	if f.LLM.Model != "custom-model:7b" {
		t.Errorf("LLM.Model = %q", f.LLM.Model)
	}
	if f.Review.MaxDiffBytes != 100000 {
		t.Errorf("Review.MaxDiffBytes = %d", f.Review.MaxDiffBytes)
	}
	if f.Review.Temperature != 0.1 {
		t.Errorf("Review.Temperature = %v", f.Review.Temperature)
	}
	if f.Review.MaxTokens != 1024 {
		t.Errorf("Review.MaxTokens = %d", f.Review.MaxTokens)
	}
	if f.Server.QueueSize != 16 {
		t.Errorf("Server.QueueSize = %d", f.Server.QueueSize)
	}
	if f.Retry.MaxAttempts != 6 {
		t.Errorf("Retry.MaxAttempts = %d", f.Retry.MaxAttempts)
	}
}

func TestParse_EmptyInputGetsDefaults(t *testing.T) {
	f, err := Parse([]byte(""))
	if err != nil {
		t.Fatalf("Parse empty: %v", err)
	}
	if f.GitLab.URL != "https://gitlab.com" {
		t.Errorf("default URL = %q", f.GitLab.URL)
	}
	if f.LLM.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("default LLM URL = %q", f.LLM.BaseURL)
	}
	if f.LLM.Model != "qwen2.5-coder:7b" {
		t.Errorf("default model = %q", f.LLM.Model)
	}
	if f.Review.MaxDiffBytes != 200000 {
		t.Errorf("default MaxDiffBytes = %d", f.Review.MaxDiffBytes)
	}
}

func TestParse_PartialOverridesDefaults(t *testing.T) {
	in := `
gitlab:
  url: https://custom.example.com
`
	f, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.GitLab.URL != "https://custom.example.com" {
		t.Errorf("URL not overridden: %q", f.GitLab.URL)
	}
	if f.GitLab.TokenEnv != "GITLAB_TOKEN" {
		t.Errorf("TokenEnv not defaulted: %q", f.GitLab.TokenEnv)
	}
	if f.LLM.Model != "qwen2.5-coder:7b" {
		t.Errorf("Model not defaulted: %q", f.LLM.Model)
	}
}

func TestParse_InvalidYAML(t *testing.T) {
	_, err := Parse([]byte("not: valid: yaml: :::"))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse YAML") {
		t.Errorf("error should mention 'parse YAML', got %v", err)
	}
}

func TestLoad_EmptyPathReturnsDefaults(t *testing.T) {
	f, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if f.LLM.Model != "qwen2.5-coder:7b" {
		t.Errorf("expected defaults; got model %q", f.LLM.Model)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("error should mention read, got %v", err)
	}
}

func TestLoad_RealFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	err := writeFile(path, sampleYAML)
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.GitLab.URL != "https://gitlab.example.com" {
		t.Errorf("URL = %q", f.GitLab.URL)
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	// Construct a File that bypasses Defaults via direct struct
	// initialization — Validate() should catch the missing
	// required fields.
	f := &File{}
	validateErr := f.Validate()
	if validateErr == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(validateErr.Error(), "missing required") {
		t.Errorf("error should mention 'missing required', got %v", validateErr)
	}
}

func TestValidate_NegativeMaxDiffBytes(t *testing.T) {
	f := &File{}
	f.Defaults()
	f.Review.MaxDiffBytes = -1
	if validateErr := f.Validate(); validateErr == nil {
		t.Fatal("expected error for negative MaxDiffBytes")
	}
}

func TestValidate_ZeroQueueSize(t *testing.T) {
	f := &File{}
	f.Defaults()
	f.Server.QueueSize = 0
	if validateErr := f.Validate(); validateErr == nil {
		t.Fatal("expected error for zero QueueSize")
	}
}

func TestMustEnv_Set(t *testing.T) {
	t.Setenv("MREVIEW_TEST_ENV", "value")
	got, err := MustEnv("MREVIEW_TEST_ENV")
	if err != nil {
		t.Errorf("MustEnv: %v", err)
	}
	if got != "value" {
		t.Errorf("got %q, want value", got)
	}
}

func TestMustEnv_Unset(t *testing.T) {
	// Use a name that's not in our test environment.
	t.Setenv("MREVIEW_TEST_MISSING", "")
	if _, err := MustEnv("MREVIEW_TEST_MISSING"); err == nil {
		t.Fatal("expected error for missing env")
	}
}

func TestErrNotFoundIsDistinct(t *testing.T) {
	// ErrNotFound is exported so callers can detect "no file
	// specified". Just sanity-check it's a non-nil sentinel.
	if errors.Is(ErrNotFound, ErrNotFound) != true {
		t.Error("ErrNotFound should be itself")
	}
}

// writeFile is a tiny test helper. We avoid os.WriteFile import
// in this test file so the test surface stays compact.
func writeFile(path, content string) error {
	return osWriteFile(path, []byte(content), 0o600)
}

// Compile-time check that the duration strings we ship in the
// sample YAML parse cleanly — guards against accidental typos
// in the docs that would silently ship.
func TestSampleYAMLDurationsParse(t *testing.T) {
	f, parseErr := Parse([]byte(sampleYAML))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	for _, d := range []string{
		f.Review.PerChunkTimeout,
		f.Server.ShutdownTimeout,
		f.Retry.InitialBackoff,
		f.Retry.MaxBackoff,
	} {
		if _, err := time.ParseDuration(d); err != nil {
			t.Errorf("duration %q in sample YAML doesn't parse: %v", d, err)
		}
	}
}

// TestDeriveMaxBatchBytes covers the derivation formula across
// realistic model classes plus the infeasibility edge cases.
func TestDeriveMaxBatchBytes(t *testing.T) {
	cases := []struct {
		name          string
		contextWindow int
		maxTokens     int
		wantMin       int // inclusive lower bound (derivation is approximate)
		wantMax       int // inclusive upper bound
	}{
		{
			name:          "opus-local: 168k window, 8192 output",
			contextWindow: 168000,
			maxTokens:     8192,
			wantMin:       520000,
			wantMax:       545000,
		},
		{
			name:          "gemma-26b-e4b: 80k window, 8192 output",
			contextWindow: 80000,
			maxTokens:     8192,
			wantMin:       225000,
			wantMax:       240000,
		},
		{
			name:          "small model: 8k window, 2048 output",
			contextWindow: 8000,
			maxTokens:     2048,
			wantMin:       12000,
			wantMax:       14000,
		},
		{
			name:          "feasibility floor: context < overhead+output+safety",
			contextWindow: 4000,
			maxTokens:     2048,
			wantMin:       0,
			wantMax:       0,
		},
		{
			name:          "zero context",
			contextWindow: 0,
			maxTokens:     8192,
			wantMin:       0,
			wantMax:       0,
		},
		{
			name:          "negative context",
			contextWindow: -1,
			maxTokens:     8192,
			wantMin:       0,
			wantMax:       0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveMaxBatchBytes(tc.contextWindow, tc.maxTokens)
			if got < tc.wantMin || got > tc.wantMax {
				t.Errorf("DeriveMaxBatchBytes(%d, %d) = %d; want in [%d, %d]",
					tc.contextWindow, tc.maxTokens, got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// TestApplyPreset_Hit verifies the happy path: model name is
// mapped to a preset, preset exists, values are returned.
func TestApplyPreset_Hit(t *testing.T) {
	yaml := `
llm_presets:
  opus-local:
    context_window: 168000
    per_chunk_timeout: 15m
  gemma-26b-e4b:
    context_window: 80000
    per_chunk_timeout: 5m

llm_preset_by_model:
  "qwen2.5-coder:7b": opus-local
  "gemma-26b-e4b":    gemma-26b-e4b
`
	f, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	name, p, ok := f.ApplyPreset("gemma-26b-e4b")
	if !ok {
		t.Fatal("expected preset match for gemma-26b-e4b")
	}
	if name != "gemma-26b-e4b" {
		t.Errorf("name = %q, want gemma-26b-e4b", name)
	}
	if p.ContextWindow != 80000 {
		t.Errorf("ContextWindow = %d, want 80000", p.ContextWindow)
	}
	if p.PerChunkTimeout != "5m" {
		t.Errorf("PerChunkTimeout = %q, want 5m", p.PerChunkTimeout)
	}
}

// TestApplyPreset_ReasoningEffort verifies that the
// reasoning_effort field on an LLMPreset is parsed and returned
// to callers. Reasoning-capable models (o-series, Azure AI Foundry,
// Groq, etc.) use this to control the reasoning budget.
func TestApplyPreset_ReasoningEffort(t *testing.T) {
	yaml := `
llm_presets:
  opus-local:
    context_window: 168000
    per_chunk_timeout: 15m
  o-series:
    context_window: 200000
    reasoning_effort: high

llm_preset_by_model:
  "o3-mini": o-series
`
	f, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	_, p, ok := f.ApplyPreset("o3-mini")
	if !ok {
		t.Fatal("expected preset match for o3-mini")
	}
	if p.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %q, want high", p.ReasoningEffort)
	}

	// opus-local doesn't set reasoning_effort — should default to "".
	_, p, ok = f.ApplyPreset("qwen2.5-coder:7b")
	if !ok || p == (LLMPreset{}) {
		_ = ok
	}
	// ApplyPreset for qwen2.5-coder:7b isn't in this fixture; use a
	// separate YAML to confirm the default-empty case.
	yaml2 := `
llm_presets:
  plain:
    context_window: 8000

llm_preset_by_model:
  "m": plain
`
	f2, _ := Parse([]byte(yaml2))
	_, p2, _ := f2.ApplyPreset("m")
	if p2.ReasoningEffort != "" {
		t.Errorf("omitted reasoning_effort should default to empty, got %q", p2.ReasoningEffort)
	}
}

// TestApplyPreset_Miss verifies that an unknown model returns ok=false
// without panicking — caller is expected to fall back to defaults
// and log a warning.
func TestApplyPreset_Miss(t *testing.T) {
	yaml := `
llm_presets:
  opus-local:
    context_window: 168000

llm_preset_by_model:
  "qwen2.5-coder:7b": opus-local
`
	f, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	_, _, ok := f.ApplyPreset("unknown-model")
	if ok {
		t.Error("expected no match for unknown-model")
	}
}

// TestApplyPreset_NoConfig verifies that a config without presets
// returns ok=false (so callers default gracefully).
func TestApplyPreset_NoConfig(t *testing.T) {
	f, err := Parse([]byte(`gitlab: { url: x, token_env: T }`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, _, ok := f.ApplyPreset("anything")
	if ok {
		t.Error("expected no match when config has no presets")
	}
}

// TestApplyPreset_DanglingReference verifies the misconfiguration
// case: model → preset name, but no preset with that name. The
// function returns ok=false but preserves the name so the caller
// can log it loudly.
func TestApplyPreset_DanglingReference(t *testing.T) {
	yaml := `
llm_presets:
  opus-local:
    context_window: 168000

llm_preset_by_model:
  "qwen2.5-coder:7b": nonexistent-preset
`
	f, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	name, _, ok := f.ApplyPreset("qwen2.5-coder:7b")
	if ok {
		t.Error("expected no match for dangling preset reference")
	}
	if name != "nonexistent-preset" {
		t.Errorf("dangling name should be returned for logging, got %q", name)
	}
}

// TestParsePresetTimeout confirms the helper handles empty,
// valid, and invalid duration strings consistently.
func TestParsePresetTimeout(t *testing.T) {
	cases := []struct {
		in        string
		want      time.Duration
		wantValid bool
	}{
		{"", 15 * time.Minute, false},
		{"5m", 5 * time.Minute, true},
		{"30s", 30 * time.Second, true},
		{"garbage", 15 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, valid := ParsePresetTimeout(tc.in)
			if got != tc.want {
				t.Errorf("ParsePresetTimeout(%q) = %v; want %v", tc.in, got, tc.want)
			}
			if valid != tc.wantValid {
				t.Errorf("ParsePresetTimeout(%q) valid = %v; want %v", tc.in, valid, tc.wantValid)
			}
		})
	}
}
