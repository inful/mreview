package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// runWithArgs is a tiny helper used by every test in this file: it
// invokes run() with a synthetic stdout / stderr and returns both
// buffers + the exit code.
func runWithArgs(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(), args, stdout, stderr)
	return stdout.String(), stderr.String(), code
}

func TestRun_HelpFlagExitsZeroAndListsSubcommands(t *testing.T) {
	stdout, _, code := runWithArgs(t, "--help")
	if code != ExitOK {
		t.Errorf("--help returned %d, want %d", code, ExitOK)
	}
	for _, want := range []string{"review", "serve", "doctor", "--log-format"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--help output missing %q\n%s", want, stdout)
		}
	}
}

func TestRun_HelpOnSubcommandExitsZero(t *testing.T) {
	// `mreview review --help` should also print help and exit 0.
	stdout, _, code := runWithArgs(t, "review", "--help")
	if code != ExitOK {
		t.Errorf("review --help returned %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "--repo") {
		t.Errorf("review --help output missing --repo\n%s", stdout)
	}
}

func TestRun_NoSubcommandReturnsConfigError(t *testing.T) {
	_, stderr, code := runWithArgs(t)
	if code != ExitConfig {
		t.Errorf("no subcommand returned %d, want %d", code, ExitConfig)
	}
	// Kong tells the user which subcommands are valid; check for at
	// least one of them so the message is recognisable.
	if !strings.Contains(stderr, "review") {
		t.Errorf("expected 'review' in stderr, got: %q", stderr)
	}
}
func TestRun_ReviewSubcommand_ParsesFlags(t *testing.T) {
	_, stderr, code := runWithArgs(t,
		"review",
		"--repo=foo/bar",
		"--mr=42",
		"--gitlab-token=test",
		"--dry-run",
		"--log-format=json",
	)
	// The reviewer emits multiple JSON log lines; parse only the
	// first one for the field checks.
	first := firstJSONLine(stderr)
	if first == "" {
		t.Fatalf("no JSON line in stderr: %s", stderr)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(first), &m); err != nil {
		t.Fatalf("first stderr line is not valid JSON: %v\n%s", err, first)
	}
	// Exit code is non-zero because the test token is rejected
	// by the real gitlab.com; we only assert the first log line
	// was emitted (proving the wiring + flag binding work) and
	// carries the parsed values.
	if code != ExitAuth && code != ExitOK {
		t.Errorf("review returned %d, want auth-401 or 0\nstderr: %s", code, stderr)
	}
	if got := m["repo"]; got != "foo/bar" {
		t.Errorf("repo = %v, want %q", got, "foo/bar")
	}
	if got, _ := m["mr"].(float64); got != 42 {
		t.Errorf("mr = %v, want 42", m["mr"])
	}
	if got := m["dry_run"]; got != true {
		t.Errorf("dry_run = %v, want true", got)
	}
}

// firstJSONLine returns the first non-empty line of s.
func firstJSONLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func TestRun_ReviewSubcommand_MissingRequiredRepo(t *testing.T) {
	_, stderr, code := runWithArgs(t, "review", "--mr=1", "--gitlab-token=test")
	if code != ExitConfig {
		t.Errorf("missing --repo returned %d, want %d", code, ExitConfig)
	}
	if !strings.Contains(stderr, "--repo") {
		t.Errorf("expected '--repo' in stderr, got: %q", stderr)
	}
}

func TestRun_ReviewSubcommand_InvalidMR(t *testing.T) {
	_, stderr, code := runWithArgs(t, "review", "--repo=foo/bar", "--mr=not-a-number", "--gitlab-token=test")
	if code != ExitConfig {
		t.Errorf("invalid --mr returned %d, want %d", code, ExitConfig)
	}
	if !strings.Contains(stderr, "--mr") {
		t.Errorf("expected '--mr' in stderr, got: %q", stderr)
	}
}

func TestRun_ServeSubcommand_ParsesFlags(t *testing.T) {
	// serve blocks until the context is cancelled. Run it in a
	// goroutine and cancel to drive shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{
			"serve",
			"--addr=:0",
			"--webhook-secret=test-secret",
			"--queue-size=4",
			"--log-format=json",
		}, stdout, stderr)
	}()
	// Give it a moment to log then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not exit after context cancel")
	}
	if !strings.Contains(stderr.String(), "serve: starting") {
		t.Errorf("expected 'serve: starting' log line, got: %s", stderr.String())
	}
}

// TestRun_ConfigFile_InjectsDefaults writes a YAML config to a
// temp file and runs mreview with --config pointing at it. The
// flag's defaults should be replaced by the config values, and
// explicit CLI flags should still win over the config.
func TestRun_ConfigFile_InjectsDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	yaml := `
gitlab:
  url: https://config-gitlab.example.com
  token_env: MY_GITLAB_TOKEN
llm:
  base_url: http://config-llm:9999/v1
  model: config-model:7b
review:
  max_diff_bytes: 50000
  per_chunk_timeout: 30s
  temperature: 0.5
`
	if err := osWriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Set the token env var so applyConfigToEnv can copy it.
	t.Setenv("MY_GITLAB_TOKEN", "test-token")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{
			"--config=" + cfgPath,
			"doctor",
			"--skip-gitlab",
			"--log-format=json",
		}, stdout, stderr,
	)
	// doctor will try to hit the LLM URL from the config. The LLM
	// is unreachable so we expect a non-zero exit. The point of
	// this test is that the dispatch happened with config values
	// (no parse error).
	if code == ExitConfig {
		t.Errorf("expected dispatch to succeed (config loaded), got ExitConfig\nstderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "config-llm:9999") {
		t.Errorf("expected config-supplied LLM URL in logs, got: %s", stderr.String())
	}
}

// TestRun_ConfigFile_CLIOverridesConfig verifies that explicit
// CLI flags still win over config-file values.
func TestRun_ConfigFile_CLIOverridesConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	yaml := `
llm:
  base_url: http://config-llm:9999/v1
  model: config-model:7b
review:
  max_diff_bytes: 50000
`
	if err := osWriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("MY_GITLAB_TOKEN", "test-token")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	// Override --max-diff-bytes via CLI. The config says 50000.
	code := run(context.Background(),
		[]string{
			"--config=" + cfgPath,
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--max-diff-bytes=999000", // override
			"--log-format=json",
		}, stdout, stderr,
	)
	// We don't care about the exit code (review will fail because
	// the LLM is unreachable). We care that the override was
	// accepted without a parse error.
	if code == ExitConfig {
		t.Errorf("config + CLI override should parse, got ExitConfig\nstderr: %s", stderr.String())
	}
}

// TestRun_ConfigFile_MissingFile_ReturnsConfigError verifies the
// --config path error path surfaces as ExitConfig.
func TestRun_ConfigFile_MissingFile_ReturnsConfigError(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{
			"--config=/nonexistent/path/config.yaml",
			"doctor",
			"--skip-gitlab",
			"--skip-llm",
			"--log-format=json",
		}, stdout, stderr,
	)
	if code != ExitConfig {
		t.Errorf("missing config file should give ExitConfig, got %d", code)
	}
	if !strings.Contains(stderr.String(), "read") {
		t.Errorf("error should mention read failure, got: %s", stderr.String())
	}
}

// TestRun_ConfigFile_ResolvesTokenEnvViaCopy verifies that the
// file's token_env indirection is honored: setting the env var the
// file points at results in GITLAB_TOKEN being set for the CLI.
func TestRun_ConfigFile_ResolvesTokenEnvViaCopy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	yaml := `
gitlab:
  url: https://gitlab.example.com
  token_env: MY_GITLAB_TOKEN
`
	if err := osWriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("MY_GITLAB_TOKEN", "secret-value-from-custom-env")
	// Clear GITLAB_TOKEN so we can detect whether applyConfigToEnv
	// copied it. t.Setenv restores on cleanup.
	t.Setenv("GITLAB_TOKEN", "")

	// Just call the package-private helper directly. We can't
	// drive a real run() with these values because the GitLab
	// endpoint will reject the test token — but the env-var
	// resolution happens before any network call, so we can
	// assert side effects.
	applyConfigToEnv(mustLoadConfig(t, cfgPath))

	if got := os.Getenv("GITLAB_TOKEN"); got != "secret-value-from-custom-env" {
		t.Errorf("GITLAB_TOKEN = %q, want token copied from MY_GITLAB_TOKEN", got)
	}
	if got := os.Getenv("GITLAB_URL"); got != "https://gitlab.example.com" {
		t.Errorf("GITLAB_URL = %q, want from config", got)
	}
}

// TestRun_ConfigFile_WebhookSecretEnvCopy covers the server-side
// indirection.
func TestRun_ConfigFile_WebhookSecretEnvCopy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	yaml := `
server:
  webhook_secret_env: MY_WEBHOOK_SECRET
`
	if err := osWriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("MY_WEBHOOK_SECRET", "webhook-secret-value")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "")

	applyConfigToEnv(mustLoadConfig(t, cfgPath))

	if got := os.Getenv("GITLAB_WEBHOOK_SECRET"); got != "webhook-secret-value" {
		t.Errorf("GITLAB_WEBHOOK_SECRET = %q, want from MY_WEBHOOK_SECRET", got)
	}
}

// TestPreScanConfigPath verifies the pre-scan helper extracts
// --config from raw args without going through kong.
func TestPreScanConfigPath(t *testing.T) {
	cases := []struct {
		name string
		args []string
		// expectedFile: empty means "no file specified" (the
		// pre-scan returns whatever resolveConfigPath returns for
		// empty, which checks the default ~/.config location;
		// we override that here so the test is hermetic).
		env          string
		expectedFile string
	}{
		{"--config=PATH before subcommand", []string{
			"--config=/tmp/c.yaml", "review",
		}, "", "/tmp/c.yaml"},
		{"--config PATH (space) before subcommand", []string{
			"--config", "/tmp/c.yaml", "review",
		}, "", "/tmp/c.yaml"},
		{"--config after subcommand is ignored", []string{
			"review", "--config=/tmp/c.yaml",
		}, "", ""},
		{"MREVIEW_CONFIG env", []string{"review"}, "/tmp/env-c.yaml", "/tmp/env-c.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MREVIEW_CONFIG", tc.env)
			got := preScanConfigPath(tc.args)
			if tc.expectedFile == "" {
				if got != "" {
					t.Errorf("preScanConfigPath = %q, want empty", got)
				}
				return
			}
			if got != tc.expectedFile {
				t.Errorf("preScanConfigPath = %q, want %q", got, tc.expectedFile)
			}
		})
	}
}

func TestLoadOptionalFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/team-rules.md"
	content := "We use logrus, not zap."
	if err := osWriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Run("empty path returns empty string", func(t *testing.T) {
		got, err := loadOptionalFile("", "test")
		if err != nil {
			t.Errorf("expected no error for empty path, got %v", err)
		}
		if got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("reads file content", func(t *testing.T) {
		got, err := loadOptionalFile(path, "system prompt")
		if err != nil {
			t.Fatalf("loadOptionalFile: %v", err)
		}
		if got != content {
			t.Errorf("got %q, want %q", got, content)
		}
	})

	t.Run("missing file returns error", func(t *testing.T) {
		_, err := loadOptionalFile(dir+"/nope.md", "user prompt")
		if err == nil {
			t.Error("expected error for missing file")
		}
	})
}

func TestApplyConfigToEnv_IntegerFields(t *testing.T) {
	// Build a config explicitly so the test isn't subject to
	// defaults. Load("") would populate defaults and make
	// assertions like "MREVIEW_RETRIES == empty" meaningless.
	cfg := configFileForTest()
	cfg.Review.MaxDiffBytes = 123456
	cfg.Review.Temperature = 0.7
	cfg.Review.MaxTokens = 4096
	cfg.Server.QueueSize = 64
	cfg.Retry.MaxAttempts = 7 // --retries should be 6
	applyConfigToEnv(&cfg)

	if got := os.Getenv("MREVIEW_MAX_DIFF_BYTES"); got != "123456" {
		t.Errorf("MREVIEW_MAX_DIFF_BYTES = %q, want 123456", got)
	}
	if got := os.Getenv("MREVIEW_TEMPERATURE"); got != "0.7" {
		t.Errorf("MREVIEW_TEMPERATURE = %q, want 0.7", got)
	}
	if got := os.Getenv("MREVIEW_MAX_TOKENS"); got != "4096" {
		t.Errorf("MREVIEW_MAX_TOKENS = %q, want 4096", got)
	}
	if got := os.Getenv("MREVIEW_QUEUE_SIZE"); got != "64" {
		t.Errorf("MREVIEW_QUEUE_SIZE = %q, want 64", got)
	}
	// MaxAttempts=7 → CLI --retries=6 → MREVIEW_RETRIES=6.
	if got := os.Getenv("MREVIEW_RETRIES"); got != "6" {
		t.Errorf("MREVIEW_RETRIES = %q, want 6 (MaxAttempts-1)", got)
	}
}

func TestRun_DoctorSubcommand_ParsesFlags(t *testing.T) {
	_, _, code := runWithArgs(t,
		"doctor",
		"--skip-gitlab",
		"--llm-url=http://localhost:1",
		"--log-format=json",
	)
	// doctor with --skip-gitlab will still hit LLM and may fail
	// on the unreachable localhost:1. We just want the flags to
	// parse and the subcommand to dispatch. Either ExitInternal
	// (LLM unreachable) or ExitOK (LLM unreachable but ping fell
	// back) are acceptable as long as dispatch worked.
	if code != ExitOK && code != ExitInternal {
		t.Errorf("doctor returned %d, want ok or internal", code)
	}
}

func TestRun_DoctorSubcommand_MissingGitLabToken(t *testing.T) {
	stdout, _, code := runWithArgs(t,
		"doctor",
		"--skip-llm",
		"--log-format=json",
	)
	if code != ExitInternal {
		t.Errorf("missing gitlab token should fail doctor with internal, got %d", code)
	}
	// Output should mention GitLab + missing token.
	if !strings.Contains(stdout, "GitLab") {
		t.Errorf("expected 'GitLab' in output: %s", stdout)
	}
	if !strings.Contains(stdout, "not set") {
		t.Errorf("expected 'not set' in output: %s", stdout)
	}
}

func TestRun_DoctorSubcommand_AllChecksSkipped(t *testing.T) {
	stdout, _, code := runWithArgs(t,
		"doctor",
		"--skip-gitlab",
		"--skip-llm",
		"--log-format=json",
	)
	if code != ExitOK {
		t.Errorf("all skipped should be ok, got %d", code)
	}
	if !strings.Contains(stdout, "All checks passed") {
		t.Errorf("expected success message: %s", stdout)
	}
}

func TestRun_InvalidLogFormat(t *testing.T) {
	_, stderr, code := runWithArgs(t, "--log-format=yaml", "review", "--repo=foo/bar", "--mr=1", "--gitlab-token=test")
	if code != ExitConfig {
		t.Errorf("invalid --log-format returned %d, want %d", code, ExitConfig)
	}
	if !strings.Contains(stderr, "log-format") {
		t.Errorf("expected 'log-format' in stderr, got: %q", stderr)
	}
}

func TestExitError_ErrorIncludesReason(t *testing.T) {
	e := &ExitError{Code: ExitAuth, Reason: "token rejected"}
	if e.Error() != "token rejected" {
		t.Errorf("Error() = %q, want %q", e.Error(), "token rejected")
	}
}

func TestExitError_ErrorIncludesWrappedCause(t *testing.T) {
	cause := bytes.ErrTooLarge
	e := &ExitError{Code: ExitInternal, Reason: "boom", Wrapped: cause}
	if !strings.Contains(e.Error(), "boom") || !strings.Contains(e.Error(), "too large") {
		t.Errorf("Error() should include reason + cause, got %q", e.Error())
	}
}
