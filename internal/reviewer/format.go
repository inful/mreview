package reviewer

import (
	"fmt"
	"strings"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

// formatFindings renders a findings list as a compact block for
// the merge prompt. One finding per line.
func formatFindings(fs []llm.Finding) string {
	var b strings.Builder
	for i, f := range fs {
		fmt.Fprintf(&b, "%d. %s:%d [%s/%s] %s\n",
			i+1, f.File, f.Line, f.Severity, f.Category, f.Body)
	}
	return b.String()
}

// buildMergePrompt composes the (system, user) prompt pair that
// asks the LLM to consolidate per-chunk ReviewResponses into one
// final verdict. The schema is included inline (not just described)
// because observed behavior: when the schema is implied, merge
// LLMs occasionally rename "body" to "message" or "description",
// and the downstream filterFindings() drops anything with an empty
// Body. Pinning the field names explicitly makes the merge LLM
// preserve them.
//
// Extracted as a helper so tests can assert the schema is present
// without driving a full consolidate() call.
func buildMergePrompt(mr *gitlab.MergeRequest, chunks []llm.ReviewResponse) (system, user string) {
	var summaries strings.Builder
	var allFindings []llm.Finding
	for i, c := range chunks {
		fmt.Fprintf(&summaries, "Chunk %d summary: %s\n", i+1, c.Summary)
		allFindings = append(allFindings, c.Findings...)
	}

	system = "You are merging per-chunk code review outputs into one verdict. " +
		"Output ONLY a JSON object matching the ReviewResponse schema below — " +
		"every field name must match exactly so the result can be parsed:\n\n" +
		"{\n" +
		`  "findings": [` + "\n" +
		"    {\n" +
		`      "file": "<path at HEAD>",` + "\n" +
		`      "line": <1-indexed line number>,` + "\n" +
		`      "severity": "info" | "warning" | "error",` + "\n" +
		`      "category": "<one of: security, correctness, style, perf, test, docs>",` + "\n" +
		`      "body": "<one or two sentences of markdown — REQUIRED, do NOT rename to 'message' or 'description'>",` + "\n" +
		`      "suggestion": "<optional code block; empty string if none>"` + "\n" +
		"    }\n" +
		"  ],\n" +
		`  "summary": "<one consolidated paragraph>"` + "\n" +
		"}\n\n" +
		"Preserve every finding from the inputs — do not drop any. " +
		"Every field above (file, line, severity, category, body, suggestion) " +
		"must be carried through verbatim; renaming 'body' to 'message' will " +
		"cause the finding to be silently dropped on the reviewer side.\n\n" +
		"STRICT OUTPUT RULE: This is a reducer, not a generator. Emit ONLY " +
		"findings that already appear in the 'Combined findings' list in the " +
		"user message below — do not synthesise new findings based on your own " +
		"assessment of the MR. If you cannot point a claim at a specific " +
		"file:line from the input list, omit it. The observed failure mode " +
		"otherwise is hallucinated claims (e.g. \"the MR contains no Go code\" " +
		"when it clearly does) that the reviewer has no way to catch downstream."

	user = fmt.Sprintf(
		"MR: !%d %q\n\nPer-chunk summaries:\n%s\n\nCombined findings (count=%d):\n%s\n\nEmit the merged JSON object.",
		mr.IID, mr.Title, summaries.String(), len(allFindings), formatFindings(allFindings),
	)
	return system, user
}

// dedupeFindings collapses findings that share (file, line, body).
// Used by consolidate when the merge dropped entries.
func dedupeFindings(in []llm.Finding) []llm.Finding {
	seen := make(map[string]bool, len(in))
	out := make([]llm.Finding, 0, len(in))
	for _, f := range in {
		key := strings.TrimSpace(f.File) + "|" + fmt.Sprint(f.Line) + "|" + strings.TrimSpace(f.Body)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

// countPosted returns the number of PostedFinding entries with a
// non-nil Discussion.
func countPosted(pfs []PostedFinding) int {
	n := 0
	for _, pf := range pfs {
		if pf.Discussion != nil {
			n++
		}
	}
	return n
}

// shouldPostSummary decides whether to post the summary note for
// this review action.
//
//   - "open" / "reopen" → post (the MR is freshly active; the
//     human reviewer benefits from a fresh verdict).
//   - "update" → skip (every push would pile up a fresh
//     summary in the timeline; inline findings carry the per-
//     push signal).
//   - empty / unknown → post (the standalone `mreview review`
//     CLI is operator-driven; they expect a complete report).
func shouldPostSummary(action string) bool {
	switch action {
	case "update":
		return false
	default:
		// "open", "reopen", "", anything else
		return true
	}
}
