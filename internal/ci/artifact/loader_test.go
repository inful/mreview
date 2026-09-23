package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeArtifact writes content to name in dir, returning the
// full path. Helper for the loader tests.
func writeArtifact(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// unreadablePath returns the path to a file with mode 0
// (no read perms). Tests that try to read it should get a
// permission error.
func unreadablePath(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "unreadable")
	if err := os.WriteFile(p, []byte("data"), 0o000); err != nil {
		t.Fatalf("write unreadable: %v", err)
	}
	return p
}

// TestLoadAll_MissingDirectory_Propagates covers the
// catastrophic case: the directory itself doesn't exist.
// LoadAll returns an error so the caller exits with
// ExitConfig before any review work.
func TestLoadAll_MissingDirectory_Propagates(t *testing.T) {
	_, err := LoadAll("/nonexistent/dir", Source{
		BuildPath: "build.log",
	})
	if err == nil {
		t.Fatal("missing directory should return error")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should mention 'does not exist', got %v", err)
	}
}

// TestLoadAll_AllMissing_NotError covers the headline
// behaviour: when every artifact is missing, LoadAll still
// succeeds (every LoadResult.NotAvailable = true). The
// caller treats this as "degraded review".
func TestLoadAll_AllMissing_NotError(t *testing.T) {
	dir := t.TempDir()
	set, err := LoadAll(dir, Source{
		BuildPath: "build.log",
		TestsPath: "test_results.json",
		LintPath:  "lint.json",
		VulnsPath: "vulns.json",
	})
	if err != nil {
		t.Errorf("all-missing should NOT return error, got %v", err)
	}
	if !set.Build.NotAvailable {
		t.Error("build should be NotAvailable")
	}
	if !set.Tests.NotAvailable {
		t.Error("tests should be NotAvailable")
	}
	if !set.Lint.NotAvailable {
		t.Error("lint should be NotAvailable")
	}
	if !set.Vulns.NotAvailable {
		t.Error("vulns should be NotAvailable")
	}
}

// TestLoadBuild_5CaseMatrix covers the 5-case acceptance
// matrix from #43: happy, missing, empty, malformed,
// unreadable.
func TestLoadBuild_5CaseMatrix(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "build.log",
			"line1\nline2\nmain.go:10: error: undefined: foo\nline4\n")
		r := LoadBuild(path)
		if r.NotAvailable || r.ParseError != nil || r.Value == nil {
			t.Fatalf("happy: %+v", r)
		}
		if !r.Value.BuildError {
			t.Errorf("BuildError should be true when log contains 'error:'")
		}
		if !strings.Contains(r.Value.LastLines, "line1") {
			t.Errorf("LastLines missing line1: %q", r.Value.LastLines)
		}
	})

	t.Run("missing", func(t *testing.T) {
		r := LoadBuild(filepath.Join(t.TempDir(), "nope.log"))
		if !r.NotAvailable {
			t.Errorf("missing should be NotAvailable, got %+v", r)
		}
		if r.Value != nil {
			t.Errorf("missing should have nil Value")
		}
	})

	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "build.log", "")
		r := LoadBuild(path)
		if !r.NotAvailable {
			t.Errorf("empty should be NotAvailable, got %+v", r)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		dir := t.TempDir()
		path := unreadablePath(t, dir)
		r := LoadBuild(path)
		if !r.NotAvailable {
			t.Errorf("unreadable should be NotAvailable, got %+v", r)
		}
	})
}

// TestLoadTests_5CaseMatrix covers the same matrix for the
// test loader.
func TestLoadTests_5CaseMatrix(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		dir := t.TempDir()
		// Two NDJSON lines: one pass, one fail.
		ndjson := `{"Action":"run","Test":"TestFoo","Package":"pkg/x"}
{"Action":"pass","Test":"TestFoo","Package":"pkg/x"}
{"Action":"run","Test":"TestBar","Package":"pkg/x"}
{"Action":"fail","Test":"TestBar","Package":"pkg/x"}
`
		path := writeArtifact(t, dir, "test_results.json", ndjson)
		r := LoadTests(path)
		if r.NotAvailable || r.ParseError != nil || r.Value == nil {
			t.Fatalf("happy: %+v", r)
		}
		if r.Value.Pass != 1 {
			t.Errorf("Pass = %d, want 1", r.Value.Pass)
		}
		if r.Value.Fail != 1 {
			t.Errorf("Fail = %d, want 1", r.Value.Fail)
		}
		if r.Value.Total != 4 {
			t.Errorf("Total = %d, want 4", r.Value.Total)
		}
		if len(r.Value.Failures) != 1 || r.Value.Failures[0].Test != "TestBar" {
			t.Errorf("Failures = %+v", r.Value.Failures)
		}
	})

	t.Run("missing", func(t *testing.T) {
		r := LoadTests(filepath.Join(t.TempDir(), "nope.json"))
		if !r.NotAvailable {
			t.Errorf("missing should be NotAvailable")
		}
	})

	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "test_results.json", "")
		r := LoadTests(path)
		if !r.NotAvailable {
			t.Errorf("empty should be NotAvailable")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		dir := t.TempDir()
		// NDJSON with one valid line, one bad line.
		// Bad lines are tolerated (skipped) so we don't get
		// ParseError on a partial corruption — the loader
		// still produces a result with Total=1.
		path := writeArtifact(t, dir, "test_results.json",
			`{"Action":"pass","Test":"TestFoo","Package":"pkg/x"}
{not valid json at all
`)
		r := LoadTests(path)
		if r.NotAvailable {
			t.Errorf("partial corruption should NOT be NotAvailable")
		}
		if r.ParseError != nil {
			t.Errorf("partial corruption should NOT set ParseError (tolerated), got %v",
				r.ParseError)
		}
		if r.Value == nil || r.Value.Pass != 1 {
			t.Errorf("should still report 1 pass from the valid line, got %+v", r.Value)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		dir := t.TempDir()
		path := unreadablePath(t, dir)
		r := LoadTests(path)
		if !r.NotAvailable {
			t.Errorf("unreadable should be NotAvailable")
		}
	})
}

// TestLoadLint_5CaseMatrix covers the lint loader.
func TestLoadLint_5CaseMatrix(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		dir := t.TempDir()
		// Two issues: one error, one warning.
		arr := `[
			{"FromLinter":"govet","Severity":"error","Text":"x","SourceLine":"foo","Pos":{"Filename":"a.go","Line":3}},
			{"FromLinter":"staticcheck","Severity":"warning","Text":"y","SourceLine":"bar","Pos":{"Filename":"b.go","Line":7}}
		]`
		path := writeArtifact(t, dir, "lint.json", arr)
		r := LoadLint(path)
		if r.NotAvailable || r.ParseError != nil || r.Value == nil {
			t.Fatalf("happy: %+v", r)
		}
		if r.Value.Errors != 1 || r.Value.Warnings != 1 || r.Value.Total != 2 {
			t.Errorf("counts wrong: %+v", r.Value)
		}
		if len(r.Value.Issues) != 2 {
			t.Errorf("Issues = %d, want 2", len(r.Value.Issues))
		}
	})

	t.Run("missing", func(t *testing.T) {
		r := LoadLint(filepath.Join(t.TempDir(), "nope.json"))
		if !r.NotAvailable {
			t.Errorf("missing should be NotAvailable")
		}
	})

	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "lint.json", "")
		r := LoadLint(path)
		if !r.NotAvailable {
			t.Errorf("empty should be NotAvailable")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "lint.json", "{not json}")
		r := LoadLint(path)
		if r.NotAvailable {
			t.Errorf("malformed should NOT be NotAvailable")
		}
		if r.ParseError == nil {
			t.Errorf("malformed should set ParseError")
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		dir := t.TempDir()
		path := unreadablePath(t, dir)
		r := LoadLint(path)
		if !r.NotAvailable {
			t.Errorf("unreadable should be NotAvailable")
		}
	})
}

// TestLoadVulns_5CaseMatrix covers the vulns loader.
func TestLoadVulns_5CaseMatrix(t *testing.T) {
	t.Run("happy_newer_shape", func(t *testing.T) {
		dir := t.TempDir()
		doc := `{"findings":[
			{"ID":"GO-2023-0001","Summary":"bad","Details":"d","Package":"x","Version":"1.0","Severity":"HIGH"}
		]}`
		path := writeArtifact(t, dir, "vulns.json", doc)
		r := LoadVulns(path)
		if r.NotAvailable || r.ParseError != nil || r.Value == nil {
			t.Fatalf("happy: %+v", r)
		}
		if r.Value.Total != 1 {
			t.Errorf("Total = %d, want 1", r.Value.Total)
		}
	})

	t.Run("happy_older_shape", func(t *testing.T) {
		dir := t.TempDir()
		doc := `{"vulnerabilities":[
			{"ID":"GO-2023-0002","Summary":"old","Details":"d","Package":"y","Version":"1.0","Severity":"LOW"}
		]}`
		path := writeArtifact(t, dir, "vulns.json", doc)
		r := LoadVulns(path)
		if r.NotAvailable || r.ParseError != nil || r.Value == nil {
			t.Fatalf("happy older: %+v", r)
		}
		if r.Value.Total != 1 {
			t.Errorf("Total = %d, want 1", r.Value.Total)
		}
	})

	t.Run("missing", func(t *testing.T) {
		r := LoadVulns(filepath.Join(t.TempDir(), "nope.json"))
		if !r.NotAvailable {
			t.Errorf("missing should be NotAvailable")
		}
	})

	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "vulns.json", "")
		r := LoadVulns(path)
		if !r.NotAvailable {
			t.Errorf("empty should be NotAvailable")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "vulns.json", "{not json}")
		r := LoadVulns(path)
		if r.NotAvailable {
			t.Errorf("malformed should NOT be NotAvailable")
		}
		if r.ParseError == nil {
			t.Errorf("malformed should set ParseError")
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		dir := t.TempDir()
		path := unreadablePath(t, dir)
		r := LoadVulns(path)
		if !r.NotAvailable {
			t.Errorf("unreadable should be NotAvailable")
		}
	})
}

// TestSource_EmptyPath_NotConfigured covers the "operator
// hasn't configured this artifact" case. The Source.X is
// empty; the loader reports NotAvailable, never errors.
func TestSource_EmptyPath_NotConfigured(t *testing.T) {
	dir := t.TempDir()
	set, err := LoadAll(dir, Source{
		BuildPath: "build.log",
		TestsPath: "", // not configured
		LintPath:  "lint.json",
		VulnsPath: "", // not configured
	})
	if err != nil {
		t.Errorf("partial config should not error: %v", err)
	}
	if !set.Tests.NotAvailable {
		t.Error("tests (unconfigured) should be NotAvailable")
	}
	if !set.Vulns.NotAvailable {
		t.Error("vulns (unconfigured) should be NotAvailable")
	}
	// Build and lint are configured but missing — also
	// NotAvailable, no error.
	if !set.Build.NotAvailable {
		t.Error("build (missing) should be NotAvailable")
	}
	if !set.Lint.NotAvailable {
		t.Error("lint (missing) should be NotAvailable")
	}
}
