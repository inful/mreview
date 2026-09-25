package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_Review_ArtifactsDir_MissingDirectory_Lenient
// covers the headline behaviour from #43: a missing
// artifacts directory is degraded information, not a hard
// error. The review proceeds with an empty artifact set;
// the prompt renders the all-NOT-AVAILABLE block.
//
// This is the path CI operators hit on first run, before
// they've set up the central CI template: review still
// works, just without the build/test/lint/vulns signals.
//
// --tokensave-enabled=false keeps the test fast (no 30s
// MCP connect timeout).
func TestRun_Review_ArtifactsDir_MissingDirectory_Lenient(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--workdir=" + t.TempDir(),
			"--artifacts-dir=/nonexistent/dir",
			"--tokensave-enabled=false",
			"--log-format=json",
		},
		stdout, stderr,
	)
	// Exit-code-agnostic — we just want to confirm the guard
	// fires through to the review path despite the missing
	// artifacts dir.
	if !strings.Contains(stderr.String(), "starting review") {
		t.Errorf("missing artifacts dir should NOT abort the review; got:\n%s",
			stderr.String())
	}
}

// TestRun_Review_ArtifactsDir_AllPresent confirms the
// happy path: a directory containing all four artifacts
// produces an "artifacts loaded" log line and the review
// proceeds.
func TestRun_Review_ArtifactsDir_AllPresent(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"build.log":         "main.go:1: error: undefined: x\n",
		"test_results.json": `{"Action":"pass","Test":"T","Package":"x"}` + "\n",
		"lint.json":         `[{"FromLinter":"govet","Severity":"error","Text":"x","Pos":{"Filename":"a.go","Line":1}}]`,
		"vulns.json":        `{"findings":[{"ID":"GO-1","Summary":"s","Details":"d","Package":"x","Version":"1","Severity":"HIGH"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--workdir=" + t.TempDir(),
			"--artifacts-dir=" + dir,
			"--tokensave-enabled=false",
			"--log-format=json",
		},
		stdout, stderr,
	)
	if !strings.Contains(stderr.String(), "artifacts loaded") {
		t.Errorf("expected 'artifacts loaded' log line, got: %s", stderr.String())
	}
	// Per-artifact status: each one should be "present".
	for _, want := range []string{
		`"build":"present"`,
		`"tests":"present"`,
		`"lint":"present"`,
		`"vulns":"present"`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("expected %q in log, got: %s", want, stderr.String())
		}
	}
}

// TestRun_Review_ArtifactsDir_PartialCoverage covers the
// realistic CI case: some artifacts present, some missing.
// The review proceeds; per-artifact status reflects each
// file's availability.
func TestRun_Review_ArtifactsDir_PartialCoverage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "build.log"),
		[]byte("main.go:1: error: undefined: x\n"), 0o600); err != nil {
		t.Fatalf("write build.log: %v", err)
	}
	// test_results.json / lint.json / vulns.json — not written

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--workdir=" + t.TempDir(),
			"--artifacts-dir=" + dir,
			"--tokensave-enabled=false",
			"--log-format=json",
		},
		stdout, stderr,
	)
	if !strings.Contains(stderr.String(), `"build":"present"`) {
		t.Errorf("expected build present, got: %s", stderr.String())
	}
	for _, want := range []string{
		`"tests":"missing"`,
		`"lint":"missing"`,
		`"vulns":"missing"`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("expected %q in log, got: %s", want, stderr.String())
		}
	}
}

// TestRun_Review_ArtifactsDir_Malformed covers the
// malformed-JSON path: the artifact is present but
// unparseable. The review proceeds; the status flag is
// "malformed" and the prompt surfaces the raw content.
func TestRun_Review_ArtifactsDir_Malformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lint.json"),
		[]byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write lint.json: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--workdir=" + t.TempDir(),
			"--artifacts-dir=" + dir,
			"--tokensave-enabled=false",
			"--log-format=json",
		},
		stdout, stderr,
	)
	if !strings.Contains(stderr.String(), `"lint":"malformed"`) {
		t.Errorf("expected lint malformed, got: %s", stderr.String())
	}
}

// TestRun_Review_HelpFlagShowsArtifactsFlag pins the
// discoverability contract.
func TestRun_Review_HelpFlagShowsArtifactsFlag(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{"review", "--help"},
		stdout, stderr,
	)
	if code != ExitOK {
		t.Errorf("--help returned %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "--artifacts-dir") {
		t.Errorf("--help output missing --artifacts-dir\n%s", stdout.String())
	}
}
