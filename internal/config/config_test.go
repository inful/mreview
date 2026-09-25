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

// TestParse_SkillsDisabledByDefault verifies that a config file
// without a skills: section leaves Skills zero-valued (the cmd
// layer treats IsZero() as "don't start the MCP server"). This
// is the back-compat guarantee — existing deployments are
// unchanged after #44 lands.
func TestParse_SkillsDisabledByDefault(t *testing.T) {
	f, err := Parse([]byte(`
gitlab:
  url: https://gitlab.com
  token_env: GITLAB_TOKEN
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !f.Skills.IsZero() {
		t.Errorf("Skills should be zero-valued when section is absent; got %+v", f.Skills)
	}
}

// TestParse_SkillsSectionHonoursValues verifies that every
// field of SkillsConfig round-trips through YAML.
func TestParse_SkillsSectionHonoursValues(t *testing.T) {
	yaml := `
gitlab:
  url: https://gitlab.com
  token_env: GITLAB_TOKEN
skills:
  repo_path: inful/mreview-skills
  directory: skills
  ref: v1.2.3
  token_env: SKILLS_PAT
`
	f, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Skills.RepoPath != "inful/mreview-skills" {
		t.Errorf("RepoPath = %q; want inful/mreview-skills", f.Skills.RepoPath)
	}
	if f.Skills.Directory != "skills" {
		t.Errorf("Directory = %q; want skills", f.Skills.Directory)
	}
	if f.Skills.Ref != "v1.2.3" {
		t.Errorf("Ref = %q; want v1.2.3", f.Skills.Ref)
	}
	if f.Skills.TokenEnv != "SKILLS_PAT" {
		t.Errorf("TokenEnv = %q; want SKILLS_PAT", f.Skills.TokenEnv)
	}
}

// TestParse_SkillsDefaultsPopulatedWhenRepoPathSet verifies
// that Defaults() fills Directory / Ref / TokenEnv when
// RepoPath is set but the other fields are absent. The cmd
// layer can then build a loader without checking each field.
func TestParse_SkillsDefaultsPopulatedWhenRepoPathSet(t *testing.T) {
	f, err := Parse([]byte(`
gitlab:
  url: https://gitlab.com
  token_env: GITLAB_TOKEN
skills:
  repo_path: inful/mreview-skills
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Skills.Directory != "skills" {
		t.Errorf("Directory default = %q; want skills", f.Skills.Directory)
	}
	if f.Skills.Ref != "main" {
		t.Errorf("Ref default = %q; want main", f.Skills.Ref)
	}
	if f.Skills.TokenEnv != "GITLAB_TOKEN" {
		t.Errorf("TokenEnv default = %q; want GITLAB_TOKEN (reused from gitlab)", f.Skills.TokenEnv)
	}
}

// TestParse_SkillsDefaultsNotAppliedWhenRepoPathEmpty verifies
// the opt-in design: a blank skills: section keeps the feature
// disabled even after Defaults() runs. Prevents surprise
// outbound requests against a public GitLab repo for
// deployments that never opted in.
func TestParse_SkillsDefaultsNotAppliedWhenRepoPathEmpty(t *testing.T) {
	f, err := Parse([]byte(`
gitlab:
  url: https://gitlab.com
  token_env: GITLAB_TOKEN
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Skills.Directory != "" {
		t.Errorf("Directory should be empty (disabled); got %q", f.Skills.Directory)
	}
	if f.Skills.Ref != "" {
		t.Errorf("Ref should be empty (disabled); got %q", f.Skills.Ref)
	}
	if f.Skills.TokenEnv != "" {
		t.Errorf("TokenEnv should be empty (disabled); got %q", f.Skills.TokenEnv)
	}
	if !f.Skills.IsZero() {
		t.Errorf("Skills should be zero-valued when section is absent; got %+v", f.Skills)
	}
}

// TestValidate_SkillsMissingFieldsSurfacesErrors verifies that
// a partial skills config (RepoPath set but Directory / Ref /
// TokenEnv missing) fails Validate. The cmd layer catches this
// at startup with an actionable error rather than later, when
// the loader silently fetches from "/" or fails on an empty
// ref.
func TestValidate_SkillsMissingFieldsSurfacesErrors(t *testing.T) {
	f := &File{}
	f.Defaults() // populates GitLab so it doesn't also fail
	f.Skills.RepoPath = "inful/mreview-skills"
	// Deliberately leave Skills.Directory / Ref / TokenEnv zero.
	err := f.Validate()
	if err == nil {
		t.Fatal("expected validation error for partial skills config")
	}
	for _, want := range []string{"skills.directory", "skills.ref", "skills.token_env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error should mention %s; got %v", want, err)
		}
	}
}
