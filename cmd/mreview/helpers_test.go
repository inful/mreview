package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/ci/artifact"
	"github.com/inful/mreview/internal/config"
)

// osWriteFile is a package-level indirection so tests can stub
// the filesystem call. (Currently unused indirection, but kept
// for future flexibility.)
var osWriteFile = os.WriteFile

// mustLoadConfig is a tiny helper that loads a config from path
// (or returns a default-populated empty file when path is "").
// Tests use it to set up config objects without repeating the
// Load boilerplate.
func mustLoadConfig(t *testing.T, path string) *config.File {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", path, err)
	}
	return cfg
}

// configFileForTest returns a config.File with all-zero values
// (no defaults applied). Tests that want to assert "zero → not
// propagated to env" use this instead of config.Load("").
func configFileForTest() config.File {
	return config.File{}
}

// ---------------------------------------------------------------------------
// Task 1.8: providerAPIKey
// ---------------------------------------------------------------------------

// TestProviderAPIKey covers the 7-case switch in
// providerAPIKey. Each provider has a documented env-var
// convention (Anthropic → ANTHROPIC_API_KEY, etc.); the
// function is the single source of truth for the lookup,
// so a refactor that drops a case (or misspells an env-var
// name) is caught here.
//
// The "local" case is interesting: local servers don't
// need API keys, so the function explicitly returns "" even
// if the operator happens to have ANTHROPIC_API_KEY (or any
// other) set. The test asserts that explicitly.
func TestProviderAPIKey(t *testing.T) {
	cases := []struct {
		provider string
		envVar   string // env-var to set before the call
		envVal   string // value to set
		want     string // expected return
	}{
		{"anthropic", "ANTHROPIC_API_KEY", "sk-ant-xxx", "sk-ant-xxx"},
		{"openai", "OPENAI_API_KEY", "sk-openai-xxx", "sk-openai-xxx"},
		{"gemini", "GOOGLE_API_KEY", "AIza-xxx", "AIza-xxx"},
		{"litellm", "LITELLM_API_KEY", "litellm-xxx", "litellm-xxx"},
		{"openrouter", "OPENROUTER_API_KEY", "or-xxx", "or-xxx"},
		{"local", "ANTHROPIC_API_KEY", "should-be-ignored", ""},
		{"unknown", "ANTHROPIC_API_KEY", "should-be-ignored", ""},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			t.Setenv(tc.envVar, tc.envVal)
			// Clear every other provider's env var so a
			// stray value from a previous test doesn't
			// leak into this one.
			for _, v := range []string{
				"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GOOGLE_API_KEY",
				"LITELLM_API_KEY", "OPENROUTER_API_KEY",
			} {
				if v != tc.envVar {
					t.Setenv(v, "")
				}
			}
			got := providerAPIKey(tc.provider)
			if got != tc.want {
				t.Errorf("providerAPIKey(%q) = %q, want %q", tc.provider, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Task 1.9: logWithError, errStr, statusLabel
// ---------------------------------------------------------------------------

// TestLogWithError covers the helper that produces the
// *ExitError returned from every error path in runReview
// and friends. It logs at error level with the err
// stringified, and returns a *ExitError{Code, Reason,
// Wrapped: err} so exitCodeFromError can map it to a
// process exit code.
//
// We assert both the log line AND the returned error so a
// regression that returns a correctly-typed error but
// drops the log line (or vice versa) is caught.
func TestLogWithError(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	wrapped := errors.New("underlying cause")

	got := logWithError(logger, ExitAuth, "auth failure", wrapped)

	// Return value shape.
	var exitErr *ExitError
	if !errors.As(got, &exitErr) {
		t.Fatalf("logWithError returned %T, want *ExitError", got)
	}
	if exitErr.Code != ExitAuth {
		t.Errorf("Code = %d, want %d", exitErr.Code, ExitAuth)
	}
	if exitErr.Reason != "auth failure" {
		t.Errorf("Reason = %q, want %q", exitErr.Reason, "auth failure")
	}
	if !errors.Is(got, wrapped) {
		t.Errorf("returned error should wrap the original (errors.Is failed)")
	}

	// Log line shape.
	logged := buf.String()
	if !strings.Contains(logged, `"level":"ERROR"`) {
		t.Errorf("expected ERROR level in log, got: %s", logged)
	}
	if !strings.Contains(logged, `"msg":"auth failure"`) {
		t.Errorf("expected msg=auth failure in log, got: %s", logged)
	}
	if !strings.Contains(logged, "underlying cause") {
		t.Errorf("expected underlying cause in log, got: %s", logged)
	}
}

// TestLogWithError_NilErr covers the case where logWithError
// is called with a nil err. The current implementation
// passes nil to logger.Error, which renders as <nil> in the
// JSON output; the function must not panic and must
// still return a properly-shaped *ExitError. The Wrapped
// field stays nil (intentional; errors.Is will not match
// anything specific).
func TestLogWithError_NilErr(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	got := logWithError(logger, ExitInternal, "internal", nil)
	var exitErr *ExitError
	if !errors.As(got, &exitErr) {
		t.Fatalf("logWithError returned %T, want *ExitError", got)
	}
	if exitErr.Wrapped != nil {
		t.Errorf("Wrapped = %v, want nil", exitErr.Wrapped)
	}
}

// TestErrStr covers the two-branch helper used by
// logWithError and the orchestrator's logger calls. It
// trims whitespace (a deliberate choice: log lines with
// trailing newlines from upstream libraries end up
// double-spaced otherwise).
func TestErrStr(t *testing.T) {
	t.Run("nil returns empty string", func(t *testing.T) {
		if got := errStr(nil); got != "" {
			t.Errorf("errStr(nil) = %q, want empty", got)
		}
	})
	t.Run("non-nil returns trimmed string", func(t *testing.T) {
		err := errors.New("hello world\n")
		if got := errStr(err); got != "hello world" {
			t.Errorf("errStr = %q, want %q (no trailing whitespace)", got, "hello world")
		}
	})
}

// TestStatusLabel covers the three-branch generic helper
// that turns a LoadResult into a one-line log label. The
// priority is: Value (present) > ParseError (malformed) >
// else (missing). The test asserts each branch and the
// boundary between them (a result with both Value AND
// ParseError, which shouldn't happen in production but is
// well-defined here).
func TestStatusLabel(t *testing.T) {
	type sampleT struct{ S string }

	t.Run("Value set -> present", func(t *testing.T) {
		r := artifact.LoadResult[sampleT]{Value: &sampleT{S: "x"}}
		if got := statusLabel(r); got != "present" {
			t.Errorf("statusLabel = %q, want %q", got, "present")
		}
	})
	t.Run("ParseError set -> malformed", func(t *testing.T) {
		r := artifact.LoadResult[sampleT]{ParseError: errors.New("bad json")}
		if got := statusLabel(r); got != "malformed" {
			t.Errorf("statusLabel = %q, want %q", got, "malformed")
		}
	})
	t.Run("Value nil + ParseError nil -> missing", func(t *testing.T) {
		r := artifact.LoadResult[sampleT]{}
		if got := statusLabel(r); got != "missing" {
			t.Errorf("statusLabel = %q, want %q", got, "missing")
		}
	})
	t.Run("Value wins when both set (defensive)", func(t *testing.T) {
		// Production never sets both; if a future refactor
		// changes the loader to set both on a malformed
		// parse that still yields a value, the operator
		// should still see 'present' (Value wins).
		r := artifact.LoadResult[sampleT]{
			Value:      &sampleT{S: "x"},
			ParseError: errors.New("ignored"),
		}
		if got := statusLabel(r); got != "present" {
			t.Errorf("statusLabel = %q, want %q (Value should win)", got, "present")
		}
	})
}
