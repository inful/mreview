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
// the caller requires a config file. Kept separate so callers
// can distinguish "no file specified" (use defaults) from
// "file specified but missing" (error).
var ErrNotFound = errors.New("config: file not specified")
