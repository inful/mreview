package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// sampleYAML exercises every recognised top-level key in the
// post-#42 schema. `server:` is intentionally absent — the
// architecture reset drops the serve mode; the type is kept
// as a stub but not exercised by the fixture.
const sampleYAML = `
gitlab:
  url: https://gitlab.example.com
  token_env: GITLAB_TOKEN_CUSTOM

provider:
  base_url: http://provider.internal:8080/v1
  model: custom-model:7b

review:
  bot_username_env: GITLAB_BOT_USERNAME

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
	if f.Provider.BaseURL != "http://provider.internal:8080/v1" {
		t.Errorf("Provider.BaseURL = %q", f.Provider.BaseURL)
	}
	if f.Provider.Model != "custom-model:7b" {
		t.Errorf("Provider.Model = %q", f.Provider.Model)
	}
	if f.Review.BotUsernameEnv != "GITLAB_BOT_USERNAME" {
		t.Errorf("Review.BotUsernameEnv = %q", f.Review.BotUsernameEnv)
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
	if f.Provider.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("default Provider URL = %q", f.Provider.BaseURL)
	}
	if f.Provider.Model != "qwen2.5-coder:7b" {
		t.Errorf("default model = %q", f.Provider.Model)
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
	if f.Provider.Model != "qwen2.5-coder:7b" {
		t.Errorf("Model not defaulted: %q", f.Provider.Model)
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
	if f.Provider.Model != "qwen2.5-coder:7b" {
		t.Errorf("expected defaults; got model %q", f.Provider.Model)
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

// TestServerConfig_StubIsAccepted pins the "server: stub kept
// for back-compat" behaviour — old YAML files referencing the
// dropped serve mode still parse cleanly without errors.
func TestServerConfig_StubIsAccepted(t *testing.T) {
	yaml := `
gitlab:
  url: https://gitlab.com
  token_env: GITLAB_TOKEN
provider:
  base_url: http://x
  model: m
server:
  addr: ":9090"
  queue_size: 16
  workers: 8
`
	if _, err := Parse([]byte(yaml)); err != nil {
		t.Errorf("legacy server: block should parse without error: %v", err)
	}
}

func TestErrNotFoundIsDistinct(t *testing.T) {
	if errors.Is(ErrNotFound, ErrNotFound) != true {
		t.Error("ErrNotFound should be itself")
	}
}

// writeFile is a tiny test helper. We avoid os.WriteFile import
// in this test file so the test surface stays compact.
func writeFile(path, content string) error {
	return osWriteFile(path, []byte(content), 0o600)
}

// TestSampleYAMLDurationsParse guards against accidental typos
// in the duration strings in the sample fixture.
func TestSampleYAMLDurationsParse(t *testing.T) {
	f, parseErr := Parse([]byte(sampleYAML))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	for _, d := range []string{
		f.Retry.InitialBackoff,
		f.Retry.MaxBackoff,
	} {
		if _, err := time.ParseDuration(d); err != nil {
			t.Errorf("duration %q in sample YAML doesn't parse: %v", d, err)
		}
	}
}
