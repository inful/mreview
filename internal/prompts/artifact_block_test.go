package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/ci/artifact"
)

// TestRenderArtifactBlock_AllAvailable covers the happy
// path: every artifact present + parsed, the block lists
// all four with their content.
func TestRenderArtifactBlock_AllAvailable(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "build.log", "line1\nmain.go:5: error: undefined: x\n")
	mustWrite(t, dir, "test_results.json",
		`{"Action":"pass","Test":"T1","Package":"x"}
{"Action":"fail","Test":"T2","Package":"x"}
`)
	mustWrite(t, dir, "lint.json", `[{"FromLinter":"govet","Severity":"error","Text":"x","Pos":{"Filename":"a.go","Line":1}}]`)
	mustWrite(t, dir, "vulns.json", `{"findings":[{"ID":"GO-1","Summary":"s","Details":"d","Package":"x","Version":"1","Severity":"HIGH"}]}`)

	set, err := artifact.LoadAll(dir, artifact.Source{
		BuildPath: "build.log",
		TestsPath: "test_results.json",
		LintPath:  "lint.json",
		VulnsPath: "vulns.json",
	})
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	block := RenderArtifactBlock(set)

	// Status lines for each artifact.
	for _, want := range []string{
		"build.log", "test_results.json", "lint.json", "vulns.json",
		"present (", // every artifact's status line
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q", want)
		}
	}
	// Content.
	for _, want := range []string{
		"line1", // build content
		"T2",    // test failure
		"govet", // linter name
		"GO-1",  // vuln ID
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing content %q", want)
		}
	}
	// NOT AVAILABLE should NOT appear when everything is present.
	if strings.Contains(block, "NOT AVAILABLE") {
		t.Errorf("'NOT AVAILABLE' should not appear in the all-available case")
	}
}

// TestRenderArtifactBlock_AllMissing covers the degraded
// case: every artifact is missing. The block renders the
// status list with four "NOT AVAILABLE" markers and
// per-artifact "Not available" notes.
func TestRenderArtifactBlock_AllMissing(t *testing.T) {
	dir := t.TempDir()
	set, err := artifact.LoadAll(dir, artifact.Source{
		BuildPath: "build.log",
		TestsPath: "test_results.json",
		LintPath:  "lint.json",
		VulnsPath: "vulns.json",
	})
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	block := RenderArtifactBlock(set)

	// Each artifact should appear with "NOT AVAILABLE".
	if got := strings.Count(block, "NOT AVAILABLE"); got < 4 {
		t.Errorf("expected ≥4 NOT AVAILABLE markers, got %d", got)
	}
	// And the "Review with the available artifacts" footer.
	if !strings.Contains(block, "Review with the available artifacts") {
		t.Errorf("block missing footer guidance")
	}
}

// TestRenderArtifactBlock_Malformed covers the partial
// degradation: one artifact malformed, others fine. The
// block surfaces the "present, malformed" status so the
// agent knows the content is unreliable.
func TestRenderArtifactBlock_Malformed(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "lint.json", "{not valid json")

	set, _ := artifact.LoadAll(dir, artifact.Source{
		BuildPath: "",
		TestsPath: "",
		LintPath:  "lint.json",
		VulnsPath: "",
	})

	block := RenderArtifactBlock(set)

	if !strings.Contains(block, "present, malformed JSON") {
		t.Errorf("block missing 'present, malformed JSON' marker, got:\n%s", block)
	}
}

// TestRenderArtifactBlock_EmptySourceDir covers the empty
// SourceDir case (when the artifacts-dir is empty). The
// block still renders, just with no path echo.
func TestRenderArtifactBlock_EmptySourceDir(t *testing.T) {
	set := artifact.Set{} // zero-value; SourceDir = ""

	block := RenderArtifactBlock(set)

	// Status list still has the four entries.
	for _, want := range []string{
		"build.log", "test_results.json", "lint.json", "vulns.json",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q", want)
		}
	}
}

// mustWrite is a tiny helper to keep the table-driven
// fixtures readable.
func mustWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
