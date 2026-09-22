package llm

import (
	"errors"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
)

// smallDiff is a single-hunk change that fits any reasonable budget.
const smallDiff = "@@ -1,3 +1,3 @@\n-old\n+new\n context\n"

// bigDiff is a synthetic oversized diff: many hunks, well over
// any sane budget when used in a test. Built programmatically so
// the size is exact.
func makeBigDiff(numHunks, hunkSize int) string {
	var b strings.Builder
	for i := 0; i < numHunks; i++ {
		b.WriteString("@@ -1 +1 @@\n")
		for j := 0; j < hunkSize; j++ {
			b.WriteString("+some content line that adds bytes to this hunk\n")
		}
	}
	return b.String()
}

func TestChunkByFile_EmptyDiffSkipped(t *testing.T) {
	changes := []gitlab.ChangeFile{
		{NewPath: "binary.png", Diff: ""}, // empty diff = binary
		{NewPath: "a.go", Diff: smallDiff},
	}
	chunks, err := ChunkByFile(changes, 1024)
	if err != nil {
		t.Fatalf("ChunkByFile: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk (binary skipped), got %d", len(chunks))
	}
	if chunks[0].File != "a.go" {
		t.Errorf("File = %q", chunks[0].File)
	}
	if chunks[0].Part != 0 || chunks[0].TotalParts != 0 {
		t.Errorf("Part/TotalParts = %d/%d, want 0/0", chunks[0].Part, chunks[0].TotalParts)
	}
}

func TestChunkByFile_SingleChunk(t *testing.T) {
	changes := []gitlab.ChangeFile{
		{NewPath: "a.go", Diff: smallDiff},
	}
	chunks, err := ChunkByFile(changes, 4096)
	if err != nil {
		t.Fatalf("ChunkByFile: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if !strings.Contains(chunks[0].Diff, "diff --git a/") {
		t.Errorf("diff should be prefixed with a header, got %q", chunks[0].Diff)
	}
	if chunks[0].Size != len(chunks[0].Diff) {
		t.Errorf("Size = %d, want %d", chunks[0].Size, len(chunks[0].Diff))
	}
}

func TestChunkByFile_NewFileFlag(t *testing.T) {
	changes := []gitlab.ChangeFile{
		{NewPath: "new.go", NewFile: true, Diff: smallDiff},
	}
	chunks, err := ChunkByFile(changes, 4096)
	if err != nil {
		t.Fatalf("ChunkByFile: %v", err)
	}
	if !chunks[0].IsNew {
		t.Error("IsNew should be true")
	}
	if !strings.Contains(chunks[0].Diff, "new file mode") {
		t.Errorf("expected 'new file mode' in diff, got %q", chunks[0].Diff)
	}
}

func TestChunkByFile_DeletedFileFlag(t *testing.T) {
	changes := []gitlab.ChangeFile{
		{OldPath: "old.go", NewPath: "old.go", DeletedFile: true, Diff: smallDiff},
	}
	chunks, err := ChunkByFile(changes, 4096)
	if err != nil {
		t.Fatalf("ChunkByFile: %v", err)
	}
	if !chunks[0].IsDeleted {
		t.Error("IsDeleted should be true")
	}
	if !strings.Contains(chunks[0].Diff, "deleted file mode") {
		t.Errorf("expected 'deleted file mode' in diff, got %q", chunks[0].Diff)
	}
}

func TestChunkByFile_RenamedFileFlag(t *testing.T) {
	changes := []gitlab.ChangeFile{
		{OldPath: "old.go", NewPath: "new.go", RenamedFile: true, Diff: smallDiff},
	}
	chunks, err := ChunkByFile(changes, 4096)
	if err != nil {
		t.Fatalf("ChunkByFile: %v", err)
	}
	if !chunks[0].IsRenamed {
		t.Error("IsRenamed should be true")
	}
	if !strings.Contains(chunks[0].Diff, "rename from") {
		t.Errorf("expected 'rename from' in diff, got %q", chunks[0].Diff)
	}
}

func TestChunkByFile_MultipleFilesOneChunkEach(t *testing.T) {
	changes := []gitlab.ChangeFile{
		{NewPath: "a.go", Diff: smallDiff},
		{NewPath: "b.go", Diff: smallDiff},
		{NewPath: "c.go", Diff: smallDiff},
	}
	chunks, err := ChunkByFile(changes, 4096)
	if err != nil {
		t.Fatalf("ChunkByFile: %v", err)
	}
	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks, got %d", len(chunks))
	}
}

func TestChunkByFile_SplitAtHunks(t *testing.T) {
	// 5 hunks, each ~30 bytes; budget of 400 bytes forces a split.
	diff := makeBigDiff(5, 3) // ~5 * (hunk header + 3 lines) ≈ 5 * 200 bytes
	changes := []gitlab.ChangeFile{
		{NewPath: "big.go", Diff: diff},
	}
	// Budget small enough that the wrapped diff doesn't fit.
	const budget = 400
	chunks, err := ChunkByFile(changes, budget)
	if err != nil {
		t.Fatalf("ChunkByFile: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected ≥2 chunks after split, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.TotalParts == 0 {
			t.Errorf("chunk %d has TotalParts=0; split should set it", i)
		}
		if c.Part == 0 {
			t.Errorf("chunk %d has Part=0; split should set it", i)
		}
		if c.Size > budget {
			t.Errorf("chunk %d size %d exceeds budget %d", i, c.Size, budget)
		}
	}
}

func TestChunkByFile_OversizedSingleHunk(t *testing.T) {
	// A single line that's larger than the budget — even
	// line-level splitting can't help, so ChunkByFile must
	// surface OversizedError.
	big := "+" + strings.Repeat("x", 500) + "\n"
	changes := []gitlab.ChangeFile{
		{NewPath: "huge.go", Diff: "@@ -1 +1 @@\n" + big},
	}
	const budget = 200
	_, err := ChunkByFile(changes, budget)
	if err == nil {
		t.Fatal("expected OversizedError")
	}
	var oe *OversizedError
	if !errors.As(err, &oe) {
		t.Fatalf("expected *OversizedError, got %T", err)
	}
	if oe.File != "huge.go" {
		t.Errorf("File = %q", oe.File)
	}
	if oe.MaxBytes != budget {
		t.Errorf("MaxBytes = %d, want %d", oe.MaxBytes, budget)
	}
}

func TestChunkByFile_LargeHunkSucceedsViaLineSplit(t *testing.T) {
	// A large hunk whose individual lines all fit — should split
	// cleanly into multiple chunks at line boundaries, no
	// OversizedError.
	diff := "@@ -1 +1 @@\n" + strings.Repeat("+line of content here\n", 50)
	changes := []gitlab.ChangeFile{
		{NewPath: "medium.go", Diff: diff},
	}
	const budget = 200
	chunks, err := ChunkByFile(changes, budget)
	if err != nil {
		t.Fatalf("expected line-level split success, got %v", err)
	}
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Size > budget {
			t.Errorf("chunk %d size %d exceeds budget %d", i, c.Size, budget)
		}
	}
}

func TestChunkByFile_RejectsBadBudget(t *testing.T) {
	for _, budget := range []int{0, -1, -1000} {
		_, err := ChunkByFile(nil, budget)
		if err == nil {
			t.Errorf("expected error for budget %d", budget)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	c := Chunk{Diff: strings.Repeat("a", 400)} // 400 bytes
	if got := EstimateTokens([]Chunk{c}); got != 100 {
		t.Errorf("EstimateTokens = %d, want 100 (400/4)", got)
	}
	if got := EstimateTokens(nil); got != 0 {
		t.Errorf("EstimateTokens(nil) = %d, want 0", got)
	}
}
