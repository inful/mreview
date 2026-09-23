// Package prompts owns the system + user prompts fed to the
// harness agent. The templates are plain Go strings for now
// (the orchestrator passes them to harness's AgentSpec.SystemPrompt
// and to the user message on RunSync); PR #6 may move them
// to embedded Markdown for team overlays.
//
// The prompts encode the read-only contract: the agent sees
// exactly two tool types (read_file, tokensave MCP) and is
// instructed never to propose edits. This is the harness-side
// half of the "no shell access / no write access" guarantee;
// the orchestrator-side half is the read-only tool registry
// built in orchestrator.go.
package prompts

import (
	"strings"

	"github.com/inful/mreview/internal/ci/artifact"
	"github.com/inful/mreview/internal/diff"
)

// ReviewSystemPrompt is the static system prompt for the
// review agent. Stable across turns so the harness's prompt-
// cache prefix stays valid (one of the locked decisions in #42
// is "prompt-cache discipline" — harness gives this for free,
// but only if the system prompt is stable).
//
// The prompt is large because the agent has to internalise the
// finding schema, severity semantics, and the read-only tool
// contract. It is the single most important file in the
// codebase for review quality; pin changes here through tests
// in PR #6.
const ReviewSystemPrompt = `You are a senior code reviewer reviewing a GitLab merge request.

TOOL SURFACE — read-only by contract:
- read_file: for raw source, configs, READMEs the agent needs verbatim
- mcp__tokensave__smart_context: code-graph queries ("what does this code do / what depends on it")
- mcp__tokensave__semantic_search: semantic search across the codebase
- mcp__tokensave__impact_analysis: blast radius ("if I change this, what breaks")

You do NOT have shell access. You do NOT have write/edit tools. You do NOT re-run CI tools
the pipeline already ran (build, test, lint, vulncheck). CI artifacts are pre-loaded
into your context. Do not invoke external commands. Do not propose edits — your role
is review, not fix.

OUTPUT FORMAT — strict JSON, no prose, no Markdown fences:
{
  "findings": [
    {
      "file": "<path at HEAD>",
      "line": <1-indexed line number>,
      "severity": "info" | "warning" | "error",
      "category": "<one of: security, correctness, style, perf, test, docs>",
      "body": "<one or two sentences of markdown>",
      "suggestion": "<optional code block; empty string if none>"
    }
  ],
  "summary": "<one paragraph verdict for the MR overall>"
}

RULES:
- Every file path must match exactly one of the paths in the diff.
- Line numbers are 1-indexed and refer to the file at HEAD.
- severity "error" = blocker (do not merge). "warning" = must fix before merge. "info" = nit.
- Be terse. One finding per real issue. Skip trivial style nits unless they obscure a bug.
- Every finding must reference a specific file:line from the diff and explain a real issue.
- The findings array is the primary output. Each concrete issue MUST appear as a finding.
- The summary is a SHORT narrative recap (2-4 sentences); it does NOT substitute for findings.
- Every issue in the summary MUST have a matching finding with a specific file:line. Drop
  unmatched claims from the summary too.
- An empty findings array is ONLY valid when the diff is genuinely clean. In that case,
  emit summary as a single short sentence ("LGTM, no issues found").
- A long summary that describes real issues alongside empty findings is malformed; do not
  produce that.
- State explicitly when a finding's corroboration depends on a CI artifact that was
  marked NOT AVAILABLE or malformed in the loaded context.

CI ARTIFACTS — when the orchestrator pre-loads build.log, test_results.json, lint.json,
vulns.json into your context, you can reason about them. Cite them in findings when
they're decisive. If an artifact is marked NOT AVAILABLE or malformed, your confidence
in findings that would have depended on it must drop — say so in the finding body.

POLICY — when the orchestrator pre-loads a policy.yaml, the policy's verdict (info /
warning / error) is binding. Findings you produce at "info" that the policy escalates to
"error" (because the file matches a severity_override or the MR carries a label) will be
posted as "error". Match your severity to the *strongest* verdict you expect.
`

// ReviewUserPrompt builds the user message from the MR
// metadata + diff chunks + CI artifacts. The function is a
// pure renderer — no I/O, no logging — so tests can assert
// exact strings.
//
// The chunks argument is the output of diff.ChunkByFile. The
// orchestrator passes the MR's diff there; this function turns
// the chunks into labeled blocks the model can read.
//
// The artifactSet argument carries the CI artifacts (build
// log, test results, lint, vulns) the central pipeline
// produced before mreview ran. A zero-value set still
// renders the "all NOT AVAILABLE" block (see
// RenderArtifactBlock) so the agent sees an explicit
// "artifacts are absent" signal rather than an omission.
func ReviewUserPrompt(meta ReviewMetadata, chunks []diff.Chunk, artifactSet artifact.Set) string {
	var b strings.Builder
	b.WriteString("Merge request: !")
	b.WriteString(itoa(meta.IID))
	b.WriteString(" ")
	if meta.Title != "" {
		b.WriteString(meta.Title)
	}
	b.WriteString("\nAuthor: ")
	b.WriteString(meta.Author)
	b.WriteString("\nSource branch: ")
	b.WriteString(meta.SourceBranch)
	b.WriteString(" → Target branch: ")
	b.WriteString(meta.TargetBranch)
	b.WriteString("\n\n")

	if meta.Description != "" {
		b.WriteString("Description:\n")
		b.WriteString(strings.TrimSpace(meta.Description))
		b.WriteString("\n\n")
	}

	// CI artifact block goes BEFORE the diff so the agent
	// reads the artifact status list first and self-
	// calibrates confidence on findings that depend on
	// them.
	b.WriteString(RenderArtifactBlock(artifactSet))
	b.WriteString("\n")

	if len(chunks) == 0 {
		b.WriteString("(no files changed)\n")
		return b.String()
	}

	b.WriteString("Files changed:\n\n")
	for _, c := range chunks {
		writeChunk(&b, c)
	}

	b.WriteString("\nReview the above diff and emit the JSON object described in your instructions.\n")
	return b.String()
}

// ReviewMetadata is the MR-level context the prompt needs.
// Decoupled from internal/gitlab so the prompts package
// doesn't import GitLab plumbing.
type ReviewMetadata struct {
	IID          int64
	Title        string
	Description  string
	Author       string // username
	SourceBranch string
	TargetBranch string
}

func writeChunk(b *strings.Builder, c diff.Chunk) {
	var label strings.Builder
	label.WriteString("File: ")
	label.WriteString(c.File)
	switch {
	case c.IsNew:
		label.WriteString(" (NEW)")
	case c.IsDeleted:
		label.WriteString(" (DELETED)")
	case c.IsRenamed:
		label.WriteString(" (RENAMED)")
	}
	if c.Part > 0 {
		// (part N/M)
		label.WriteString(" (part ")
		label.WriteString(itoa(int64(c.Part)))
		label.WriteString("/")
		label.WriteString(itoa(int64(c.TotalParts)))
		label.WriteString(")")
	}
	label.WriteString("\n")

	b.WriteString("=== ")
	b.WriteString(label.String())
	b.WriteString(c.Diff)
	if !strings.HasSuffix(c.Diff, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("=== End ")
	b.WriteString(label.String())
}

// itoa formats an int without pulling strconv. The prompts
// package is imported by the orchestrator; keeping it stdlib-
// only avoids pulling new transitive deps.
func itoa(n int64) string {
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
