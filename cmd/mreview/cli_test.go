package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/inful/mreview/internal/config"
)

// newTestLogger returns a slog.Logger writing to buf. Tests use
// this to assert the resolveLLMSettings log lines.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestResolveLLMSettings_PresetHit verifies that when a model
// matches a preset and no CLI values are supplied, the helper
// derives MaxBatchBytes from the preset's context window and
// applies the preset's per-chunk timeout.
func TestResolveLLMSettings_PresetHit(t *testing.T) {
	cfg, err := config.Parse([]byte(`
llm_presets:
  opus-local:
    context_window: 168000
    per_chunk_timeout: 15m

llm_preset_by_model:
  "qwen2.5-coder:7b": opus-local
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var logBuf bytes.Buffer
	maxBytes, timeout, _ := resolveLLMSettings(cfg, "qwen2.5-coder:7b", 0, 0, "", 8192, newTestLogger(&logBuf))

	if maxBytes < 520000 || maxBytes > 545000 {
		t.Errorf("maxBytes = %d; want in [520000, 545000]", maxBytes)
	}
	if timeout != 15*time.Minute {
		t.Errorf("timeout = %v; want 15m", timeout)
	}
	if !strings.Contains(logBuf.String(), "max_batch_bytes derived from preset") {
		t.Errorf("expected derivation log, got:\n%s", logBuf.String())
	}
}

// TestResolveLLMSettings_CLIOverridesPreset confirms CLI values
// take precedence over preset values, and the override is logged.
func TestResolveLLMSettings_CLIOverridesPreset(t *testing.T) {
	cfg, err := config.Parse([]byte(`
llm_presets:
  opus-local:
    context_window: 168000
    per_chunk_timeout: 15m

llm_preset_by_model:
  "qwen2.5-coder:7b": opus-local
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var logBuf bytes.Buffer
	maxBytes, timeout, _ := resolveLLMSettings(cfg, "qwen2.5-coder:7b", 300000, 10*time.Minute, "", 8192, newTestLogger(&logBuf))

	if maxBytes != 300000 {
		t.Errorf("CLI value should win: maxBytes = %d, want 300000", maxBytes)
	}
	if timeout != 10*time.Minute {
		t.Errorf("CLI value should win: timeout = %v, want 10m", timeout)
	}
	if !strings.Contains(logBuf.String(), "CLI flag wins over preset") {
		t.Errorf("expected override log, got:\n%s", logBuf.String())
	}
}

// TestResolveLLMSettings_NoPreset confirms that when no preset
// matches, the CLI values pass through unchanged and no
// derivation runs.
func TestResolveLLMSettings_NoPreset(t *testing.T) {
	cfg, err := config.Parse([]byte(`
llm_presets:
  opus-local:
    context_window: 168000

llm_preset_by_model:
  "qwen2.5-coder:7b": opus-local
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var logBuf bytes.Buffer
	maxBytes, timeout, _ := resolveLLMSettings(cfg, "unknown-model", 0, 0, "", 8192, newTestLogger(&logBuf))

	if maxBytes != 0 {
		t.Errorf("maxBytes = %d; want 0 (no preset, no CLI)", maxBytes)
	}
	if timeout != 0 {
		t.Errorf("timeout = %v; want 0 (no preset, no CLI)", timeout)
	}
	if strings.Contains(logBuf.String(), "LLM preset applied") {
		t.Errorf("should not log preset application when no match:\n%s", logBuf.String())
	}
}

// TestResolveLLMSettings_NilConfig confirms the helper handles a
// nil *config.File (which happens when no config file was given
// on the CLI).
func TestResolveLLMSettings_NilConfig(t *testing.T) {
	var logBuf bytes.Buffer
	maxBytes, timeout, _ := resolveLLMSettings(nil, "any-model", 0, 0, "", 8192, newTestLogger(&logBuf))

	if maxBytes != 0 || timeout != 0 {
		t.Errorf("nil config: maxBytes=%d timeout=%v; want 0/0", maxBytes, timeout)
	}
}

// TestResolveLLMSettings_ReasoningEffortFromPreset verifies that
// when a preset declares a reasoning_effort and the CLI didn't
// override it, the helper fills the value in and logs it.
func TestResolveLLMSettings_ReasoningEffortFromPreset(t *testing.T) {
	cfg, err := config.Parse([]byte(`
llm_presets:
  o-series:
    context_window: 200000
    per_chunk_timeout: 15m
    reasoning_effort: high

llm_preset_by_model:
  "o3-mini": o-series
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var logBuf bytes.Buffer
	_, _, effort := resolveLLMSettings(cfg, "o3-mini", 0, 0, "", 8192, newTestLogger(&logBuf))

	if effort != "high" {
		t.Errorf("effort = %q; want high (from preset)", effort)
	}
	if !strings.Contains(logBuf.String(), "reasoning_effort from preset") {
		t.Errorf("expected preset-applied log, got:\n%s", logBuf.String())
	}
}

// TestResolveLLMSettings_MaxBatchBytesFromPreset verifies that a
// preset's max_batch_bytes override is applied when the CLI is
// unset, and that it wins over the derivation from context_window.
// This is the path operators use to tune batch size for models
// whose effective context is smaller than their advertised window
// or that can't process packed batches within PerChunkTimeout.
func TestResolveLLMSettings_MaxBatchBytesFromPreset(t *testing.T) {
	cfg, err := config.Parse([]byte(`
llm_presets:
  coding-agent:
    context_window: 168000
    per_chunk_timeout: 5m
    max_batch_bytes: 5000

llm_preset_by_model:
  "coding-agent": coding-agent
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var logBuf bytes.Buffer
	maxBytes, _, _ := resolveLLMSettings(cfg, "coding-agent", 0, 0, "", 8192, newTestLogger(&logBuf))

	if maxBytes != 5000 {
		t.Errorf("maxBytes = %d; want 5000 (preset wins over derivation)", maxBytes)
	}
	if !strings.Contains(logBuf.String(), "max_batch_bytes from preset") {
		t.Errorf("expected preset-applied log, got:\n%s", logBuf.String())
	}
}

// TestResolveLLMSettings_MaxBatchBytesCLIOverridesPreset confirms
// the CLI value wins over the preset's max_batch_bytes override.
func TestResolveLLMSettings_MaxBatchBytesCLIOverridesPreset(t *testing.T) {
	cfg, err := config.Parse([]byte(`
llm_presets:
  coding-agent:
    context_window: 168000
    per_chunk_timeout: 5m
    max_batch_bytes: 5000

llm_preset_by_model:
  "coding-agent": coding-agent
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var logBuf bytes.Buffer
	maxBytes, _, _ := resolveLLMSettings(cfg, "coding-agent", 3000, 0, "", 8192, newTestLogger(&logBuf))

	if maxBytes != 3000 {
		t.Errorf("maxBytes = %d; want 3000 (CLI wins over preset)", maxBytes)
	}
	if !strings.Contains(logBuf.String(), "CLI flag wins over preset") {
		t.Errorf("expected CLI-wins log, got:\n%s", logBuf.String())
	}
}

// TestResolveLLMSettings_ReasoningEffortCLIOverridesPreset confirms
// the CLI value wins over the preset's reasoning_effort.
func TestResolveLLMSettings_ReasoningEffortCLIOverridesPreset(t *testing.T) {
	cfg, err := config.Parse([]byte(`
llm_presets:
  o-series:
    context_window: 200000
    per_chunk_timeout: 15m
    reasoning_effort: high

llm_preset_by_model:
  "o3-mini": o-series
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var logBuf bytes.Buffer
	_, _, effort := resolveLLMSettings(cfg, "o3-mini", 0, 0, "low", 8192, newTestLogger(&logBuf))

	if effort != "low" {
		t.Errorf("effort = %q; want low (CLI wins)", effort)
	}
}

// TestResolveLLMSettings_DanglingReference confirms that a model
// mapped to a missing preset name produces a WARN log.
func TestResolveLLMSettings_DanglingReference(t *testing.T) {
	cfg, err := config.Parse([]byte(`
llm_presets:
  opus-local:
    context_window: 168000

llm_preset_by_model:
  "qwen2.5-coder:7b": nonexistent-preset
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var logBuf bytes.Buffer
	maxBytes, _, _ := resolveLLMSettings(cfg, "qwen2.5-coder:7b", 0, 0, "", 8192, newTestLogger(&logBuf))

	if maxBytes != 0 {
		t.Errorf("dangling reference: maxBytes = %d; want 0", maxBytes)
	}
	if !strings.Contains(logBuf.String(), "LLM preset references unknown name") {
		t.Errorf("expected dangling-reference WARN, got:\n%s", logBuf.String())
	}
}
