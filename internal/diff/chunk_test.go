package diff

import (
	"errors"
	"strings"
	"testing"
)

// fakeSource is a test Source implementation. Tests build
// one with the fields they need; the methods default to
// the struct fields.
type fakeSource struct {
	path, oldPath, newPath, diffStr string
	isNew, isDeleted, isRenamed     bool
}

func (f fakeSource) Diff() string    { return f.diffStr }
func (f fakeSource) OldPath() string { return f.oldPath }
func (f fakeSource) NewPath() string { return f.newPath }
func (f fakeSource) IsNew() bool     { return f.isNew }
func (f fakeSource) IsDeleted() bool { return f.isDeleted }
func (f fakeSource) IsRenamed() bool { return f.isRenamed }
func (f fakeSource) Path() string    { return f.path }

// TestChunkByFile_OneChunkPerFile covers the happy path:
// three files, each fits in the budget → three chunks.
func TestChunkByFile_OneChunkPerFile(t *testing.T) {
	files := []Source{
		fakeSource{path: "a.go", diffStr: "@@ ... @@\n+hello"},
		fakeSource{path: "b.go", diffStr: "@@ ... @@\n+world"},
		fakeSource{path: "c.md", diffStr: "@@ ... @@\n+docs"},
	}
	chunks, err := ChunkByFile(files, 1000)
	if err != nil {
		t.Fatalf("ChunkByFile: %v", err)
	}
	if len(chunks) != 3 {
		t.Errorf("got %d chunks, want 3", len(chunks))
	}
	for i, c := range chunks {
		if c.Part != 0 || c.TotalParts != 0 {
			t.Errorf("chunk %d: Part=%d TotalParts=%d (single-part expected)",
				i, c.Part, c.TotalParts)
		}
	}
}

// TestChunkByFile_SkipsEmpty covers binary files and other
// zero-content changes — they produce no chunks.
func TestChunkByFile_SkipsEmpty(t *testing.T) {
	files := []Source{
		fakeSource{path: "a.go", diffStr: ""}, // binary
		fakeSource{path: "b.go", diffStr: "@@ ... @@\n+hello"},
	}
	chunks, err := ChunkByFile(files, 1000)
	if err != nil {
		t.Fatalf("ChunkByFile: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("got %d chunks, want 1 (empty diffs skipped)", len(chunks))
	}
}

// TestChunkByFile_InvalidMaxBytes returns an error for
// zero or negative budgets.
func TestChunkByFile_InvalidMaxBytes(t *testing.T) {
	cases := []int{0, -1, -1000}
	for _, n := range cases {
		t.Run("maxBytes="+itoaForTest(n), func(t *testing.T) {
			_, err := ChunkByFile(nil, n)
			if err == nil {
				t.Errorf("expected error for maxBytes=%d", n)
			}
			if !strings.Contains(err.Error(), "maxBytes") {
				t.Errorf("error should mention 'maxBytes', got %v", err)
			}
		})
	}
}

// TestChunkByFile_OversizedSingleFile returns OversizedError
// when a file can't fit even after hunk-level splitting.
func TestChunkByFile_OversizedSingleFile(t *testing.T) {
	// One hunk that's bigger than the budget.
	big := "@@ ... @@\n" + strings.Repeat("x", 100)
	files := []Source{
		fakeSource{path: "huge.go", diffStr: big},
	}
	_, err := ChunkByFile(files, 50)
	if err == nil {
		t.Fatal("expected OversizedError")
	}
	var oe *OversizedError
	if !asOversized(err, &oe) {
		t.Errorf("error type = %T, want *OversizedError", err)
	}
	if oe.File != "huge.go" {
		t.Errorf("File = %q, want huge.go", oe.File)
	}
}

// TestWrapDiffWithHeader_PreservesFileStatus covers the
// status flags — new / deleted / renamed — that get
// surfaced in the chunk label.
func TestWrapDiffWithHeader_PreservesFileStatus(t *testing.T) {
	cases := []struct {
		name         string
		src          fakeSource
		wantInHeader []string
	}{
		{
			name:         "new file",
			src:          fakeSource{path: "new.go", newPath: "new.go", diffStr: "@@ ... @@\n+x", isNew: true},
			wantInHeader: []string{"new file mode"},
		},
		{
			name:         "deleted file",
			src:          fakeSource{path: "old.go", oldPath: "old.go", diffStr: "@@ ... @@\n-x", isDeleted: true},
			wantInHeader: []string{"deleted file mode"},
		},
		{
			name:         "renamed file",
			src:          fakeSource{path: "new.go", oldPath: "old.go", newPath: "new.go", diffStr: "@@ ... @@\n+x", isRenamed: true},
			wantInHeader: []string{"rename from old.go", "rename to new.go"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wrapDiffWithHeader(c.src)
			for _, want := range c.wantInHeader {
				if !strings.Contains(got, want) {
					t.Errorf("wrapDiffWithHeader missing %q\n%s", want, got)
				}
			}
		})
	}
}

// TestEstimateTokens is a rough heuristic — just pins
// the contract that "char / 4" is the formula.
func TestEstimateTokens(t *testing.T) {
	chunks := []Chunk{
		{Diff: strings.Repeat("a", 4000)}, // 4000 chars → ~1000 tokens
		{Diff: strings.Repeat("b", 4000)},
	}
	got := EstimateTokens(chunks)
	want := 2000 // 8000 chars / 4
	if got != want {
		t.Errorf("EstimateTokens = %d, want %d", got, want)
	}
}

// asOversized is a small errors.As helper that survives
// the linter's errorlint check while staying concise.
func asOversized(err error, target **OversizedError) bool {
	var oe *OversizedError
	if errors.As(err, &oe) {
		*target = oe
		return true
	}
	return false
}

// itoaForTest is a local itoa for test usage; the package's
// itoa is unexported and tests live in the same package so
// they could call it directly, but the explicit indirection
// here keeps the dependency clean.
func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
