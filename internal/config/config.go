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
	GitLab   GitLabConfig   `yaml:"gitlab"`
	Provider ProviderConfig `yaml:"provider"`
	Review   ReviewConfig   `yaml:"review"`
	Retry    RetryConfig    `yaml:"retry"`

	// Skills configures the optional skills MCP server (issue
	// #44). When RepoPath is empty the skills layer is
	// disabled — no MCP server, no mcp__skills__* tools,
	// existing deployments are unchanged.
	Skills SkillsConfig `yaml:"skills,omitempty"`

	// LLMPresets is an optional map of model-name → preset. The
	// cmd layer uses ApplyPreset(model) to derive a packing
	// budget and per-chunk timeout when the operator hasn't
	// supplied explicit CLI/env values.
	//
	// Deprecated by the architecture reset (#42): the harness
	// library now owns the provider matrix and prompt-cache
	// discipline. PR #3 drops this in favour of harness's own
	// preset layer. Kept for now to avoid breaking operator
	// configs during the transition.
	LLMPresets map[string]LLMPreset `yaml:"llm_presets,omitempty"`

	// LLMPresetByModel is an optional map of LLM model
	// identifier → preset name. Deprecated alongside
	// LLMPresets; see #42.
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

// ProviderConfig holds provider connection settings for the
// harness library. After the architecture reset (#42), the
// harness library owns the provider matrix; mreview only
// forwards a base URL (for litellm / local proxies) and the
// model name. Per-provider env-var conventions are read
// directly by harness's provider constructors.
type ProviderConfig struct {
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
}

// ReviewConfig holds review-pipeline settings that survive
// the architecture reset (#42). The LLM-specific fields
// (MaxDiffBytes, PerChunkTimeout, Temperature, MaxTokens,
// ChunkRetries, AllowPartial) are gone — the harness library
// owns the LLM loop and its tuning lives in harness's own
// configuration.
//
// Only BotUsernameEnv remains; the orchestrator reads it
// for the dedupe fingerprint filter (see internal/reviewer).
type ReviewConfig struct {
	BotUsernameEnv string `yaml:"bot_username_env,omitempty"`
}

// SkillsConfig configures the optional skills MCP server (issue
// #44). The agent uses skills as on-demand team-authored review
// guidance — see internal/skills/ for the loader and
// internal/skills/mcp/ for the MCP transport.
//
// All fields are optional. When RepoPath is empty, the skills
// layer is disabled and no mcp__skills__* tools are exposed.
// Defaults() fills sensible values for an air-gapped /
// public-instance deployment that just points at the canonical
// upstream.
type SkillsConfig struct {
	// RepoPath is the GitLab project path (e.g.
	// "inful/mreview-skills") that hosts the .md skills.
	// Required to enable the skills MCP server. Empty
	// disables the feature.
	RepoPath string `yaml:"repo_path,omitempty"`

	// Directory is the directory within RepoPath that
	// contains the .md files (e.g. "skills"). Defaults to
	// "skills".
	Directory string `yaml:"directory,omitempty"`

	// Ref is the branch / tag / SHA the loader reads from.
	// Empty means "use the project's default branch". Pinned
	// SHAs are a deliberate future addition — see #44.
	Ref string `yaml:"ref,omitempty"`

	// TokenEnv is the NAME of the env var carrying the PAT
	// used to read the skills repo. Empty defaults to
	// GitLab.TokenEnv at the cmd layer so existing
	// deployments reuse the same token.
	TokenEnv string `yaml:"token_env,omitempty"`
}

// IsZero reports whether s carries no configuration at all.
// Used by the cmd layer to decide whether to skip starting the
// skills MCP server entirely.
func (s SkillsConfig) IsZero() bool {
	return s.RepoPath == "" && s.Directory == "" && s.Ref == "" && s.TokenEnv == ""
}

// ServerConfig used to hold `mreview serve` settings. The
// architecture reset (#42) drops the serve mode; the type
// is kept as a stub so old YAML config files referencing
// `server:` don't fail the strict YAML parse. The fields are
// unused. Remove in a future release.
//
// Deprecated.
type ServerConfig struct {
	Addr             string `yaml:"addr"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
	QueueSize        int    `yaml:"queue_size"`
	ShutdownTimeout  string `yaml:"shutdown_timeout"`
	Workers          int    `yaml:"workers,omitempty"`
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
	if f.Provider.BaseURL == "" {
		f.Provider.BaseURL = "http://localhost:11434/v1"
	}
	if f.Provider.Model == "" {
		f.Provider.Model = "qwen2.5-coder:7b"
	}
	if f.Review.BotUsernameEnv == "" {
		f.Review.BotUsernameEnv = "GITLAB_BOT_USERNAME"
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
	// Skills defaults only fire when the operator configured
	// at least RepoPath. A blank skills: section in YAML
	// keeps the feature disabled — no surprise outbound
	// requests against a public GitLab repo.
	if f.Skills.RepoPath != "" {
		if f.Skills.Directory == "" {
			f.Skills.Directory = "skills"
		}
		if f.Skills.Ref == "" {
			f.Skills.Ref = "main"
		}
		if f.Skills.TokenEnv == "" {
			// Default to reusing the GitLab token. The
			// cmd layer resolves the env-var name to a
			// value at wire time; this field only carries
			// the NAME of the env var.
			f.Skills.TokenEnv = f.GitLab.TokenEnv
		}
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
	// Skills validation only fires when skills are enabled
	// (RepoPath set). Empty Skills means "disabled" — no
	// validation, no error.
	if f.Skills.RepoPath != "" {
		if f.Skills.Directory == "" {
			missing = append(missing, "skills.directory")
		}
		if f.Skills.Ref == "" {
			missing = append(missing, "skills.ref")
		}
		if f.Skills.TokenEnv == "" {
			missing = append(missing, "skills.token_env")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required fields: %s", strings.Join(missing, ", "))
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
