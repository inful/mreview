package prompts

import (
	"strings"
	"testing"

	"github.com/inful/mreview/internal/ci/artifact"
)

// TestReviewSystemPrompt_CoreContract pins the
// non-negotiable contract for the system prompt. These
// checks are deliberately strict — every one of these
// guarantees is a #42 acceptance criterion.
//
// If a contributor weakens any of these to "improve" the
// prompt, this test catches it before merge.
func TestReviewSystemPrompt_CoreContract(t *testing.T) {
	p := ReviewSystemPrompt()

	// The read-only contract: the prompt must enumerate the
	// exact tool surface the agent sees. Adding or removing a
	// tool here changes what the agent can do.
	//
	// As of the tokensave-only refactor, the local harness
	// tool registry is empty — every read goes through
	// tokensave's MCP server. The agent must see at least the
	// three friendly-named code-graph tools it has always
	// relied on, plus the raw-file reader that replaces
	// read_file.
	requiredTools := []string{
		"mcp__tokensave__read",
		"mcp__tokensave__smart_context",
		"mcp__tokensave__semantic_search",
		"mcp__tokensave__impact_analysis",
		// Skills tools (issue #44). These are optional at
		// runtime (only present when --skills-repo is set),
		// but the prompt must mention them so the agent
		// knows the names exist and can use them when
		// they're available.
		"mcp__skills__list_skills",
		"mcp__skills__read_skill",
	}
	for _, tool := range requiredTools {
		if !strings.Contains(p, tool) {
			t.Errorf("system prompt missing required tool %q", tool)
		}
	}

	// skill-authoring bundled skill must be mentioned by
	// name in the prompt so the agent reaches for it when
	// reviewing a skills-repo MR (the meta-circular check).
	if !strings.Contains(p, "skill-authoring") {
		t.Errorf("system prompt must reference the bundled skill-authoring skill by name")
	}

	// Forbidden-tool mentions: the prompt must explicitly
	// tell the agent it does NOT have shell / write / edit.
	forbiddenMentions := []string{
		// shell access denied
		"do NOT have shell access",
		// write tools denied
		"do NOT have write or edit tools",
	}
	for _, mention := range forbiddenMentions {
		if !strings.Contains(p, mention) {
			t.Errorf("system prompt missing forbidden-tool guard %q", mention)
		}
	}

	// The output format must include the JSON schema with
	// every field. We don't golden-file the whole schema
	// (that's what review.md's //go:embed is for), but the
	// contract is "every field name appears in the prompt".
	requiredFields := []string{
		"\"findings\"",
		"\"file\"",
		"\"line\"",
		"\"severity\"",
		"\"category\"",
		"\"body\"",
		"\"suggestion\"",
		"\"summary\"",
		// Prior-findings block: the LLM is asked to mirror
		// the input prior findings and tag each with a
		// status (still_valid / resolved / out_of_scope).
		// Without these fields, the orchestrator can't
		// auto-resolve prior discussions, which is the
		// whole point of running on a new commit.
		"\"prior_findings\"",
		"\"status\"",
		"\"rationale\"",
		"\"still_valid\"",
		"\"resolved\"",
		"\"out_of_scope\"",
	}
	for _, field := range requiredFields {
		if !strings.Contains(p, field) {
			t.Errorf("system prompt missing required field %q", field)
		}
	}

	// Severity semantics: the prompt must explain the
	// three levels (error / warning / info).
	for _, sev := range []string{"error", "warning", "info"} {
		if !strings.Contains(p, "\""+sev+"\"") {
			t.Errorf("system prompt missing severity level %q", sev)
		}
	}

	// Categories: same for the six standard categories.
	for _, cat := range []string{"security", "correctness", "style", "perf", "test", "docs"} {
		if !strings.Contains(p, cat) {
			t.Errorf("system prompt missing category %q", cat)
		}
	}

	// The artifact-aware contract: the prompt must reference
	// the four artifacts by name and the NOT AVAILABLE marker.
	for _, art := range []string{
		"build.log",
		"test_results.json",
		"lint.json",
		"vulns.json",
		"NOT AVAILABLE",
	} {
		if !strings.Contains(p, art) {
			t.Errorf("system prompt missing artifact marker %q", art)
		}
	}

	// Policy awareness: the prompt mentions policy.yaml
	// verdict binding.
	for _, token := range []string{"policy.yaml", "severity_override"} {
		if !strings.Contains(p, token) {
			t.Errorf("system prompt missing policy marker %q", token)
		}
	}
}

// TestReviewSystemPrompt_Stability guards against the
// "we changed the system prompt and broke the prompt cache"
// failure mode. The harness library's prompt-cache prefix
// stays valid only if the system prompt is byte-stable
// across turns.
//
// We don't golden-file the entire content (that's what the
// //go:embed is for, and the file itself is the source of
// truth). What we DO assert: the prompt doesn't change on
// subsequent calls.
func TestReviewSystemPrompt_Stability(t *testing.T) {
	first := ReviewSystemPrompt()
	second := ReviewSystemPrompt()
	if first != second {
		t.Errorf("system prompt is not stable across calls")
	}
	if len(first) < 500 {
		t.Errorf("system prompt suspiciously short: %d bytes", len(first))
	}
}

// TestReviewSystemPrompt_NoToolSprawl guards against
// future contributors silently adding new tools to the
// system prompt (e.g. write_file, bash) without updating
// the read-only contract tests. The prompt should only
// mention the four allowed tools by name.
func TestReviewSystemPrompt_NoToolSprawl(t *testing.T) {
	p := ReviewSystemPrompt()
	for _, forbidden := range []string{
		"bash",
		"edit_file",
		"write_file",
		"shell_exec",
		"run_command",
	} {
		// We allow these to appear in the "you do NOT have"
		// prohibition text. So we look for them as tool
		// definitions, not as substrings.
		if strings.Contains(p, "tool: "+forbidden) ||
			strings.Contains(p, "- "+forbidden+":") {
			t.Errorf("system prompt appears to register forbidden tool %q", forbidden)
		}
	}
}

// TestReviewUserPrompt_NoPriorFindings covers the "first
// run" path: no prior findings, so the prompt omits the
// "Prior findings" block. This is the common case on a
// fresh MR and the agent should see a clean prompt that
// doesn't mention the prior_findings array at all in the
// user message (it's still defined in the system prompt
// schema).
func TestReviewUserPrompt_NoPriorFindings(t *testing.T) {
	meta := ReviewMetadata{IID: 42, Title: "Test"}
	p := ReviewUserPrompt(meta, nil, artifact.Set{}, nil)
	if strings.Contains(p, "Prior findings") {
		t.Errorf("prompt should NOT contain a 'Prior findings' block when priorFindings is empty; got:\n%s", p)
	}
}

// TestReviewUserPrompt_WithPriorFindings covers the "follow-
// up run" path: a prior mreview run left inline comments,
// so the prompt includes a numbered "Prior findings" block
// that the agent is asked to mirror in its prior_findings
// response.
func TestReviewUserPrompt_WithPriorFindings(t *testing.T) {
	meta := ReviewMetadata{IID: 42, Title: "Test"}
	prior := []PriorFinding{
		{File: "a.go", Line: 10, Severity: "warning", Body: "Missing test"},
		{File: "b.go", Line: 20, Severity: "error", Body: "Bad name", Suggestion: "rename to `foo`"},
	}
	p := ReviewUserPrompt(meta, nil, artifact.Set{}, prior)
	for _, want := range []string{
		"Prior findings",
		"1. `a.go:10` [warning] Missing test",
		"2. `b.go:20` [error] Bad name",
		"suggestion: rename to `foo`",
		"prior_findings", // the schema field the agent must emit
		"still_valid",
		"resolved",
		"out_of_scope",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
