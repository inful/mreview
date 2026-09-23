package prompts

import (
	"strings"
	"testing"
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
	requiredTools := []string{
		"read_file",
		"mcp__tokensave__smart_context",
		"mcp__tokensave__semantic_search",
		"mcp__tokensave__impact_analysis",
	}
	for _, tool := range requiredTools {
		if !strings.Contains(p, tool) {
			t.Errorf("system prompt missing required tool %q", tool)
		}
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
