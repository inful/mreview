// Package config loads mreview's YAML configuration file.
//
// The schema (File) mirrors every flag/env var documented in
// examples/config.yaml. Loading is pure: no GitLab or LLM calls
// happen at load time — the result is just a typed struct that
// cmd/mreview maps onto its CLI flags.
//
// Secret handling: the file references env-var NAMES (e.g.
// GITLAB_TOKEN), never secret values. This lets operators keep
// config files in version control while secrets live in their
// secret store. The loader does NOT read those env vars; that's
// the caller's responsibility (kong does it via env:"" tags on
// the flags themselves).
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the top-level config schema.
//
// One File maps onto every mreview subcommand. Fields the
// subcommand doesn't use are ignored; e.g. `mreview review` reads
// GitLab/LLM/Review but ignores Server.
type File struct {
	GitLab GitLabConfig `yaml:"gitlab"`
	LLM    LLMConfig    `yaml:"llm"`
	Review ReviewConfig `yaml:"review"`
	Server ServerConfig `yaml:"server"`
	Retry  RetryConfig  `yaml:"retry"`

	// LLMPresets is an optional map of model-name → preset. The
	// cmd layer uses ApplyPreset(model) to derive a packing
	// budget and per-chunk timeout when the operator hasn't
	// supplied explicit CLI/env values.
	LLMPresets map[string]LLMPreset `yaml:"llm_presets,omitempty"`

	// LLMPresetByModel is an optional map of LLM model identifier
	// → preset name. When `--model` matches an entry, the named
	// preset from LLMPresets is applied. Strict lookup only — no
	// prefix matching, no heuristics.
	LLMPresetByModel map[string]string `yaml:"llm_preset_by_model,omitempty"`
}

// GitLabConfig holds GitLab API connection settings.
type GitLabConfig struct {
	// URL is the API base (e.g. "https://gitlab.com"). Required.
	URL string `yaml:"url"`

	// TokenEnv is the NAME of the env var carrying the
	// Personal Access Token. The loader does NOT read this
	// variable; cmd/mreview passes it through to kong's env
	// resolution at flag-binding time. Required.
	TokenEnv string `yaml:"token_env"`
}

// LLMConfig holds OpenAI-compatible LLM connection settings.
type LLMConfig struct {
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	Model     string `yaml:"model"`
}

// ReviewConfig holds review-pipeline settings.
type ReviewConfig struct {
	MaxDiffBytes    int     `yaml:"max_diff_bytes"`
	PerChunkTimeout string  `yaml:"per_chunk_timeout"` // duration string ("120s"); parsed by caller
	Temperature     float64 `yaml:"temperature"`
	MaxTokens       int     `yaml:"max_tokens"`
	BotUsernameEnv  string  `yaml:"bot_username_env"`

	// ChunkRetries is the chunk-level retry budget for transient
	// LLM errors (per-call timeouts). When 0 the reviewer's
	// default (1) applies. Negative is rejected at the flag-
	// parsing layer by the CLI; here we just pass it through.
	ChunkRetries int `yaml:"chunk_retries,omitempty"`

	// AllowPartial restores the legacy "log + substitute empty"
	// behaviour when a chunk fails. Default false (atomic
	// failure — see issue #31).
	AllowPartial bool `yaml:"allow_partial,omitempty"`
}

// ServerConfig holds `mreview serve` settings.
type ServerConfig struct {
	Addr             string `yaml:"addr"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
	QueueSize        int    `yaml:"queue_size"`
	ShutdownTimeout  string `yaml:"shutdown_timeout"`
}

// RetryConfig holds GitLab API retry policy. Applies to every
// subcommand that calls GitLab.
type RetryConfig struct {
	MaxAttempts    int    `yaml:"max_attempts"`
	InitialBackoff string `yaml:"initial_backoff"`
	MaxBackoff     string `yaml:"max_backoff"`
}

// Defaults populates unset fields with the same defaults the CLI
// uses. Callers can then detect "unset" vs "set-to-default" only
// when they need to distinguish them — for most cases, the
// populated struct is what they want.
func (f *File) Defaults() {
	if f.GitLab.URL == "" {
		f.GitLab.URL = "https://gitlab.com"
	}
	if f.GitLab.TokenEnv == "" {
		f.GitLab.TokenEnv = "GITLAB_TOKEN"
	}
	if f.LLM.BaseURL == "" {
		f.LLM.BaseURL = "http://localhost:11434/v1"
	}
	if f.LLM.Model == "" {
		f.LLM.Model = "qwen2.5-coder:7b"
	}
	if f.Review.MaxDiffBytes == 0 {
		f.Review.MaxDiffBytes = 200000
	}
	if f.Review.PerChunkTimeout == "" {
		f.Review.PerChunkTimeout = "120s"
	}
	if f.Review.Temperature == 0 {
		f.Review.Temperature = 0.2
	}
	if f.Review.MaxTokens == 0 {
		f.Review.MaxTokens = 2048
	}
	if f.Review.BotUsernameEnv == "" {
		f.Review.BotUsernameEnv = "GITLAB_BOT_USERNAME"
	}
	if f.Server.Addr == "" {
		f.Server.Addr = ":8080"
	}
	if f.Server.WebhookSecretEnv == "" {
		f.Server.WebhookSecretEnv = "GITLAB_WEBHOOK_SECRET"
	}
	if f.Server.QueueSize == 0 {
		f.Server.QueueSize = 32
	}
	if f.Server.ShutdownTimeout == "" {
		f.Server.ShutdownTimeout = "30s"
	}
	if f.Retry.MaxAttempts == 0 {
		f.Retry.MaxAttempts = 4
	}
	if f.Retry.InitialBackoff == "" {
		f.Retry.InitialBackoff = "500ms"
	}
	if f.Retry.MaxBackoff == "" {
		f.Retry.MaxBackoff = "30s"
	}
}

// Validate checks the populated File for required / well-formed
// values. Callers run this AFTER Defaults() so the required check
// fires only on missing *user-supplied* values.
func (f *File) Validate() error {
	var missing []string
	if f.GitLab.URL == "" {
		missing = append(missing, "gitlab.url")
	}
	if f.GitLab.TokenEnv == "" {
		missing = append(missing, "gitlab.token_env")
	}
	if f.LLM.BaseURL == "" {
		missing = append(missing, "llm.base_url")
	}
	if f.LLM.Model == "" {
		missing = append(missing, "llm.model")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required fields: %s", strings.Join(missing, ", "))
	}
	if f.Review.MaxDiffBytes < 0 {
		return fmt.Errorf("config: review.max_diff_bytes must be > 0, got %d", f.Review.MaxDiffBytes)
	}
	if f.Server.QueueSize < 1 {
		return fmt.Errorf("config: server.queue_size must be > 0, got %d", f.Server.QueueSize)
	}
	return nil
}

// Load reads path, parses YAML, and returns the populated File.
//
// When path is empty, an empty File is returned (not an error) —
// callers can rely on Defaults() + Validate() to handle the
// "no config file" case the same as the "config file with all
// defaults" case.
func Load(path string) (*File, error) {
	if path == "" {
		f := &File{}
		f.Defaults()
		return f, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return Parse(data)
}

// Parse converts YAML bytes into a populated File. Exposed so
// tests can build configs without filesystem I/O.
func Parse(data []byte) (*File, error) {
	f := &File{}
	if err := yaml.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("config: parse YAML: %w", err)
	}
	f.Defaults()
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return f, nil
}

// MustEnv returns os.Getenv(name) or wraps the missing-variable
// case with a friendly hint. Empty string is treated as "set".
// (Not all env-loaded vars are secrets; some are config knobs
// where empty is a valid value.)
func MustEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("config: environment variable %s is required", name)
	}
	return v, nil
}

// ErrNotFound is returned when the path argument is empty AND
// the caller requires a config file. Kept separate so callers can
// distinguish "no file specified" (use defaults) from
// "file specified but missing" (error).
var ErrNotFound = errors.New("config: file not specified")

// LLMPreset declares the runtime characteristics of one LLM model
// that the reviewer needs to size its work. The operator supplies
// the model context window (in tokens); everything else
// (max_batch_bytes) is derived from it.
type LLMPreset struct {
	// ContextWindow is the model's max context length in tokens.
	// Required.
	ContextWindow int `yaml:"context_window"`

	// PerChunkTimeout is the per-LLM-call timeout. Duration
	// string ("15m", "5m") — string for yaml.v3 round-trip
	// compatibility; parsed on use. Defaults to "15m" when
	// empty.
	PerChunkTimeout string `yaml:"per_chunk_timeout"`

	// ReasoningEffort is the model's reasoning budget
	// ("low"/"medium"/"high"). Empty means "use the server's
	// default". Only honoured by reasoning-capable models
	// (OpenAI o-series, Azure AI Foundry, Groq, Together, etc.);
	// providers that don't support the field ignore it.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`

	// MaxBatchBytes is an optional override for the byte budget
	// used by BatchChunksWithLimit when packing multiple chunks
	// into a single LLM call. When > 0, this value wins over the
	// derivation from ContextWindow + MaxTokens. Set this when:
	//   - The model's effective context is smaller than its
	//     advertised window (common for heavily quantized local
	//     models), so the derived budget over-promises.
	//   - Empirical wall-clock shows the model can't process a
	//     packed batch within PerChunkTimeout, even though the
	//     prompt fits the budget.
	// Empty (zero) means "use the derived value" — the historical
	// behaviour.
	MaxBatchBytes int `yaml:"max_batch_bytes,omitempty"`
}

// Derivation constants for MaxBatchBytes. Tuned for source-code
// review: most LLMs use BPE tokenizers where ~4 bytes ≈ 1 token
// for code. The 15% safety margin absorbs tokenizer variance (the
// real ratio can be 3-5 bytes/token) and per-request overhead the
// packer can't predict (JSON encoding, message framing, etc.).
//
// Operators who find these too conservative or too generous can
// override via the --max-batch-bytes CLI flag — that's an explicit
// escape hatch and the derivation is logged when it fires so the
// operator can see what the system computed.
const (
	PromptOverheadTokens = 1500 // system prompt + MR header + JSON envelope
	BytesPerToken        = 4    // BPE assumption for source code
	SafetyMarginFraction = 0.15 // 15% buffer for tokenizer variance
)

// DeriveMaxBatchBytes computes the byte budget for packing
// multiple chunks into one LLM call, given the model's max context
// window and the caller's max output budget.
//
// Returns 0 when the configuration is infeasible (context window
// too small for the requested output) — callers should treat 0 as
// "disable packing."
func DeriveMaxBatchBytes(contextWindow, maxTokens int) int {
	if contextWindow <= 0 {
		return 0
	}
	safety := int(float64(contextWindow) * SafetyMarginFraction)
	usable := contextWindow - PromptOverheadTokens - maxTokens - safety
	if usable <= 0 {
		return 0
	}
	return usable * BytesPerToken
}

// ApplyPreset looks up the preset for modelName and returns its
// name and parsed values. Returns ("", LLMPreset{}, false) when
// no preset matches — callers should fall back to defaults and
// log a warning so the operator knows packing is disabled.
//
// Nil-safe: a nil receiver returns ok=false (no preset). This
// matters because cmd/mreview passes a nil *File when no config
// file was supplied.
//
// Lookup is strict: the modelName must appear as a key in
// LLMPresetByModel, and the named preset must exist in LLMPresets.
// No prefix matching; an unmatched model gets no preset.
func (f *File) ApplyPreset(modelName string) (string, LLMPreset, bool) {
	if f == nil {
		return "", LLMPreset{}, false
	}
	if len(f.LLMPresetByModel) == 0 || len(f.LLMPresets) == 0 {
		return "", LLMPreset{}, false
	}
	name, ok := f.LLMPresetByModel[modelName]
	if !ok {
		return "", LLMPreset{}, false
	}
	p, ok := f.LLMPresets[name]
	if !ok {
		// Misconfigured: model → preset name, but no preset
		// with that name. Don't silently disable; return the
		// name so the caller can log it loudly.
		return name, LLMPreset{}, false
	}
	return name, p, true
}

// ParsePresetTimeout returns the preset's per-chunk timeout as a
// time.Duration. Falls back to 15m when the field is empty or
// unparseable (the historical default from CLI flags). Invalid
// strings are returned as (15m, false) so the caller can log.
func ParsePresetTimeout(s string) (time.Duration, bool) {
	if s == "" {
		return 15 * time.Minute, false
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 15 * time.Minute, false
	}
	return d, true
}
