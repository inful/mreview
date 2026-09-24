// Package prompts owns the system + user prompts fed to the
// harness agent. The templates are plain Go strings for now
// (the orchestrator passes them to harness's AgentSpec.SystemPrompt
// and to the user message on RunSync); PR #6 may move them
// to embedded Markdown for team overlays.
//
// The prompts encode the read-only contract: the agent's
// only file-reading surface is the tokensave MCP server
// (mcp__tokensave__read for raw source + the full
// code-graph tool family under mcp__tokensave__*). It is
// instructed never to propose edits. This is the
// harness-side half of the "no shell access / no write
// access" guarantee; the orchestrator-side half is the
// (empty) read-only tool registry built in
// buildHarnessRuntime in cmd/mreview/clients.go.
package prompts

import (
	_ "embed"
	"strings"

	"github.com/inful/mreview/internal/ci/artifact"
	"github.com/inful/mreview/internal/diff"
)

// reviewSystemMarkdown is the source-of-truth for the agent's
// system prompt. Embedded into the binary at build time so
// the orchestrator doesn't need filesystem access at runtime.
//
// Stable across turns so the harness's prompt-cache prefix
// stays valid (one of the locked decisions in #42 is
// "prompt-cache discipline" — harness gives this for free,
// but only if the system prompt is stable).
//
// Pin changes here through golden-file tests
// (internal/prompts/review_test.go) — review quality is
// downstream of this exact wording.
//
//go:embed review_system.md
var reviewSystemMarkdown string

// ReviewSystemPrompt returns the static system prompt for
// the review agent. Kept as a function (not a const) so
// future enhancements (per-language overlays, etc.) can
// keep the same call site.
func ReviewSystemPrompt() string {
	return reviewSystemMarkdown
}

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
