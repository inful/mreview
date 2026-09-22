package llm

import (
	"strings"
	"testing"
)

func sampleMeta() ReviewMetadata {
	return ReviewMetadata{
		IID:          42,
		Title:        "Add caching layer",
		Description:  "Caches expensive calls in memory.",
		Author:       "alice",
		SourceBranch: "feat/cache",
		TargetBranch: "main",
	}
}

func sampleChunks() []Chunk {
	return []Chunk{
		{
			File: "internal/cache/cache.go",
			Diff: "@@ -1 +1 @@\n-old\n+new\n",
			Size: 100,
		},
		{
			File:       "internal/auth/jwt.go",
			IsNew:      true,
			Diff:       "@@ -0,0 +1 @@\n+package auth\n",
			Size:       80,
			Part:       1,
			TotalParts: 2,
		},
		{
			File:       "internal/auth/jwt.go",
			IsNew:      true,
			Diff:       "@@ -1 +1 @@\n+var x = 1\n",
			Size:       60,
			Part:       2,
			TotalParts: 2,
		},
	}
}

func TestBuildReviewPrompt_EmptyChunksRejected(t *testing.T) {
	_, _, err := BuildReviewPrompt(sampleMeta(), nil, PromptOptions{})
	if err == nil {
		t.Fatal("expected error for empty chunks")
	}
}

func TestBuildReviewPrompt_SystemMentionsSchema(t *testing.T) {
	system, _, err := BuildReviewPrompt(sampleMeta(), sampleChunks(), PromptOptions{})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	for _, want := range []string{
		"JSON object",
		"findings",
		"summary",
		"severity",
		"category",
		"1-indexed",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

func TestBuildReviewPrompt_SystemEmbedsCategories(t *testing.T) {
	system, _, err := BuildReviewPrompt(sampleMeta(), sampleChunks(), PromptOptions{
		Categories: []Category{CategorySecurity, CategoryPerf},
	})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	if !strings.Contains(system, "security") || !strings.Contains(system, "perf") {
		t.Errorf("expected custom categories in system prompt, got %q", system)
	}
	// Default categories not requested: must NOT appear.
	if strings.Contains(system, "correctness") {
		t.Errorf("default categories should not appear when Categories is set, got %q", system)
	}
}

func TestBuildReviewPrompt_UserHasMRHeader(t *testing.T) {
	_, user, err := BuildReviewPrompt(sampleMeta(), sampleChunks(), PromptOptions{})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	for _, want := range []string{
		"!42",
		"Add caching layer",
		"alice",
		"feat/cache -> main",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q", want)
		}
	}
}

func TestBuildReviewPrompt_DescriptionToggle(t *testing.T) {
	meta := sampleMeta()
	_, userOn, err := BuildReviewPrompt(meta, sampleChunks(), PromptOptions{IncludeMRDescription: true})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	if !strings.Contains(userOn, "Caches expensive calls in memory.") {
		t.Error("description should be present when IncludeMRDescription=true")
	}

	_, userOff, err := BuildReviewPrompt(meta, sampleChunks(), PromptOptions{IncludeMRDescription: false})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	if strings.Contains(userOff, "Caches expensive calls in memory.") {
		t.Error("description should NOT be present when IncludeMRDescription=false")
	}
}

func TestBuildReviewPrompt_EmptyDescriptionIgnored(t *testing.T) {
	meta := sampleMeta()
	meta.Description = "   \n  "
	_, user, err := BuildReviewPrompt(meta, sampleChunks(), PromptOptions{IncludeMRDescription: true})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	if strings.Contains(user, "Description:") {
		t.Errorf("empty description should not produce a 'Description:' header, got %q", user)
	}
}

func TestBuildReviewPrompt_ChunkLabels(t *testing.T) {
	_, user, err := BuildReviewPrompt(sampleMeta(), sampleChunks(), PromptOptions{})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	for _, want := range []string{
		"=== File: internal/cache/cache.go",
		"=== File: internal/auth/jwt.go (NEW) (part 1/2)",
		"=== File: internal/auth/jwt.go (NEW) (part 2/2)",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing chunk label %q", want)
		}
	}
}

func TestBuildReviewPrompt_DeletedAndRenamedLabels(t *testing.T) {
	chunks := []Chunk{
		{File: "old.go", IsDeleted: true, Diff: "x", Size: 1},
		{File: "renamed.go", OldPath: "old.go", NewPath: "renamed.go", IsRenamed: true, Diff: "y", Size: 1},
	}
	_, user, err := BuildReviewPrompt(sampleMeta(), chunks, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	if !strings.Contains(user, "(DELETED)") {
		t.Errorf("expected DELETED label, got %q", user)
	}
	if !strings.Contains(user, "(RENAMED)") {
		t.Errorf("expected RENAMED label, got %q", user)
	}
}

func TestBuildReviewPrompt_DiffContentIncluded(t *testing.T) {
	chunks := []Chunk{
		{
			File: "a.go",
			Diff: "@@ -1 +1 @@\n-old\n+new\n",
			Size: 30,
		},
	}
	_, user, err := BuildReviewPrompt(sampleMeta(), chunks, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	if !strings.Contains(user, "-old") || !strings.Contains(user, "+new") {
		t.Errorf("diff content not preserved in prompt, got %q", user)
	}
	if !strings.Contains(user, "=== End File: a.go") {
		t.Errorf("expected End marker, got %q", user)
	}
}

// TestBuildReviewPrompt_SystemSuffix_AppendedAfterRules confirms
// the operator-supplied suffix is layered onto the system prompt
// AFTER the system-owned schema and rules — so the LLM always
// sees the parse-required bits first.
func TestBuildReviewPrompt_SystemSuffix_AppendedAfterRules(t *testing.T) {
	suffix := "Our team prefers logrus over zap; flag any new zap import."
	system, _, err := BuildReviewPrompt(sampleMeta(), sampleChunks(), PromptOptions{
		SystemPromptSuffix: suffix,
	})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}

	// The suffix MUST appear after the system-owned rules block.
	rulesIdx := strings.Index(system, "- Output the JSON object directly.")
	if rulesIdx < 0 {
		t.Fatal("system-owned rules block not found in system prompt")
	}
	suffixIdx := strings.Index(system, suffix)
	if suffixIdx < 0 {
		t.Fatalf("operator suffix not found in system prompt\n--- prompt ---\n%s", system)
	}
	if suffixIdx <= rulesIdx {
		t.Errorf("suffix must come AFTER system rules; got suffixIdx=%d, rulesIdx=%d",
			suffixIdx, rulesIdx)
	}

	// The suffix is wrapped in a labeled section so the LLM can
	// tell where operator guidance starts.
	if !strings.Contains(system, "# Team-specific guidance (operator-supplied)") {
		t.Errorf("expected labeled section for operator guidance, got %q", system)
	}
}

// TestBuildReviewPrompt_SystemSuffix_EmptyNotAdded confirms the
// suffix block is omitted entirely when no operator text is
// supplied (no empty "Team-specific guidance" section).
func TestBuildReviewPrompt_SystemSuffix_EmptyNotAdded(t *testing.T) {
	system, _, err := BuildReviewPrompt(sampleMeta(), sampleChunks(), PromptOptions{
		SystemPromptSuffix: "   \n  \t", // whitespace only
	})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	if strings.Contains(system, "Team-specific guidance") {
		t.Errorf("whitespace-only suffix should be treated as empty, got %q", system)
	}
	if strings.Contains(system, "Operator-supplied") {
		t.Errorf("empty suffix should not appear in prompt")
	}
}

// TestBuildReviewPrompt_UserSuffix_AppendedAfterChunks confirms the
// operator-supplied user-prompt suffix is appended after the
// diff chunks (so it doesn't get accidentally truncated by the
// chunker's maxBytes budget — chunks have their own budget;
// the suffix lives outside that).
func TestBuildReviewPrompt_UserSuffix_AppendedAfterChunks(t *testing.T) {
	suffix := "This MR is a WIP — focus on architectural concerns, not naming."
	_, user, err := BuildReviewPrompt(sampleMeta(), sampleChunks(), PromptOptions{
		UserPromptSuffix: suffix,
	})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}

	// The "Files changed:" section must appear before the suffix.
	filesIdx := strings.Index(user, "Files changed:")
	suffixIdx := strings.Index(user, suffix)
	if filesIdx < 0 || suffixIdx < 0 {
		t.Fatalf("missing sections; filesIdx=%d suffixIdx=%d\n--- prompt ---\n%s",
			filesIdx, suffixIdx, user)
	}
	if suffixIdx <= filesIdx {
		t.Errorf("suffix must come AFTER diff chunks; got suffixIdx=%d, filesIdx=%d",
			suffixIdx, filesIdx)
	}
}

// TestBuildReviewPrompt_UserSuffix_EmptyNotAdded confirms empty
// suffix doesn't produce an empty section.
func TestBuildReviewPrompt_UserSuffix_EmptyNotAdded(t *testing.T) {
	_, user, err := BuildReviewPrompt(sampleMeta(), sampleChunks(), PromptOptions{
		UserPromptSuffix: "",
	})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	if strings.Contains(user, "Team-specific context") {
		t.Errorf("empty user suffix should not appear in prompt")
	}
}

// TestBuildReviewPrompt_SystemOwnedAlwaysPresent confirms the
// system-owned schema and rules cannot be displaced by the
// operator suffix — even a large suffix must not erase them.
func TestBuildReviewPrompt_SystemOwnedAlwaysPresent(t *testing.T) {
	huge := strings.Repeat("team rule. ", 500) // ~6 KB of fake guidance
	system, _, err := BuildReviewPrompt(sampleMeta(), sampleChunks(), PromptOptions{
		SystemPromptSuffix: huge,
	})
	if err != nil {
		t.Fatalf("BuildReviewPrompt: %v", err)
	}
	for _, mustHave := range []string{
		"You are a senior code reviewer",
		`"findings":`,
		`"severity": "info" | "warning" | "error"`,
		"- Output the JSON object directly.",
	} {
		if !strings.Contains(system, mustHave) {
			t.Errorf("system prompt missing required piece %q after large suffix", mustHave)
		}
	}
}
