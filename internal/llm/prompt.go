package llm

import (
	"fmt"
	"strings"
)

// ReviewMetadata is the MR-level context the prompt needs.
//
// Not exported as a flat struct for callers — they pass a subset
// of GitLab's MergeRequest fields directly. Keeping this minimal
// keeps the test surface narrow.
type ReviewMetadata struct {
	IID          int64
	Title        string
	Description  string
	Author       string // username, e.g. "alice"
	SourceBranch string
	TargetBranch string
}

// PromptOptions tunes the prompt template. Zero-value is sensible.
//
// The system-owned prefix (JSON schema, output rules, severity
// semantics) is ALWAYS emitted and CANNOT be overridden. The
// fields below layer team-specific guidance ON TOP of the prefix —
// use them to steer the LLM toward your team's conventions
// without breaking parsing.
type PromptOptions struct {
	// Categories lists the severity/category vocabulary the model
	// should use. Empty means the default set.
	Categories []Category

	// IncludeMRDescription prepends the MR description to the
	// user prompt. Off when the operator wants a leaner prompt.
	IncludeMRDescription bool

	// SystemPromptSuffix is team-specific text appended AFTER
	// the system-owned schema and rules. Use for documentation
	// standards, language-specific dependency preferences, project
	// conventions, "this team uses X for Y", etc. Empty means
	// "no team-specific guidance."
	//
	// The system-owned prefix is always present and CANNOT be
	// disabled — your text is appended, not a replacement.
	SystemPromptSuffix string

	// UserPromptSuffix is team-specific text appended at the
	// END of the user prompt (after the diff chunks). Use for
	// per-MR context the LLM should consider when reviewing
	// (e.g. "this PR is a WIP, focus on architecture not naming").
	// Empty means no extra context.
	UserPromptSuffix string
}

// defaultCategories is the vocabulary embedded in the system
// prompt when PromptOptions.Categories is empty. The reviewer
// doesn't strictly enforce these (the parser accepts any string)
// but steering the model toward a fixed set improves consistency.
var defaultCategories = []Category{
	CategorySecurity,
	CategoryCorrectness,
	CategoryStyle,
	CategoryPerf,
	CategoryTest,
	CategoryDocs,
}

// BuildReviewPrompt produces the (system, user) pair for one
// review call against a single batch of chunks.
//
// The system prompt embeds:
//   - the JSON schema the model must emit
//   - the severity / category vocabulary
//   - the line-anchoring rules (1-indexed, paths must match the
//     diff exactly)
//
// The user prompt embeds:
//   - MR header (IID, title, author, branches)
//   - MR description (when IncludeMRDescription)
//   - one labeled block per chunk
//
// Multiple chunks go into a SINGLE user message so the model
// reviews the whole MR in one shot when it fits. When the MR is
// large enough to need multiple Chat calls, the reviewer calls
// BuildReviewPrompt once per chunk batch (phase 5 wires this up).
func BuildReviewPrompt(meta ReviewMetadata, chunks []Chunk, opts PromptOptions) (system, user string, err error) {
	if len(chunks) == 0 {
		return "", "", fmt.Errorf("llm: BuildReviewPrompt called with no chunks")
	}

	cats := opts.Categories
	if len(cats) == 0 {
		cats = defaultCategories
	}

	system = buildSystemPrompt(cats, opts.SystemPromptSuffix)
	user = buildUserPrompt(meta, chunks, opts.IncludeMRDescription, opts.UserPromptSuffix)
	return system, user, nil
}

// buildSystemPrompt returns the persona + schema instructions,
// optionally followed by operator-supplied team guidance.
//
// The system-owned prefix (JSON schema, output rules, severity
// semantics) is always present. The optional suffix is appended
// AFTER the rules section so the LLM sees schema first and team
// guidance second.
//
// Kept as a separate function so tests can pin the exact wording.
func buildSystemPrompt(categories []Category, suffix string) string {
	var b strings.Builder
	b.WriteString("You are a senior code reviewer reviewing a GitLab merge request.\n\n")
	b.WriteString("Output ONLY a JSON object matching this exact shape — no prose, no Markdown fences:\n\n")
	b.WriteString("{\n")
	b.WriteString(`  "findings": [` + "\n")
	b.WriteString("    {\n")
	b.WriteString(`      "file": "<path at HEAD>",` + "\n")
	b.WriteString(`      "line": <1-indexed line number>,` + "\n")
	b.WriteString(`      "severity": "info" | "warning" | "error",` + "\n")
	b.WriteString(`      "category": "<one of: `)
	b.WriteString(strings.Join(toStringSlice(categories), ", "))
	b.WriteString(`>",` + "\n")
	b.WriteString(`      "body": "<one or two sentences of markdown>",` + "\n")
	b.WriteString(`      "suggestion": "<optional code block; empty string if none>"` + "\n")
	b.WriteString("    }\n")
	b.WriteString("  ],\n")
	b.WriteString(`  "summary": "<one paragraph verdict for the MR overall>"` + "\n")
	b.WriteString("}\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Every file path must match exactly one of the paths in the diff.\n")
	b.WriteString("- Line numbers are 1-indexed and refer to the file at HEAD.\n")
	b.WriteString("- severity \"error\" = blocker (do not merge). \"warning\" = must fix before merge. \"info\" = nit or suggestion.\n")
	b.WriteString("- Be terse. One finding per real issue. Skip trivial style nits unless they obscure a real bug.\n")
	b.WriteString("- Every finding must reference a specific file:line from the diff and explain a real issue or observation.\n")
	b.WriteString("\n")
	b.WriteString("Findings vs. summary:\n")
	b.WriteString("- The `findings` array is the primary output. Each concrete issue you identify MUST appear as a separate finding object with file, line, severity, category, and body.\n")
	b.WriteString("- The `summary` is a SHORT narrative recap of the findings (2-4 sentences). It is NOT a substitute for findings.\n")
	b.WriteString("- Every issue you mention in the summary MUST have a corresponding entry in the findings array with a specific file:line. If you cannot point at a file:line for an issue, drop it from the summary too.\n")
	b.WriteString("- An empty findings array is ONLY valid when the diff is genuinely clean. In that case, emit summary as a single short sentence confirming cleanliness (e.g. \"LGTM, no issues found.\").\n")
	b.WriteString("- A long summary that describes real issues alongside an empty findings array is a malformed response. Do not produce that.\n")
	b.WriteString("\n")
	b.WriteString("- Output the JSON object directly. Do NOT wrap it in ``` fences or preamble prose.\n")
	if suffix = strings.TrimSpace(suffix); suffix != "" {
		b.WriteString("\n# Team-specific guidance (operator-supplied)\n\n")
		b.WriteString(suffix)
		b.WriteString("\n")
	}
	return b.String()
}

// buildUserPrompt composes the per-MR user message, optionally
// followed by operator-supplied team context.
func buildUserPrompt(meta ReviewMetadata, chunks []Chunk, includeDescription bool, suffix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Merge request: !%d %q by %s (%s -> %s)\n\n",
		meta.IID, meta.Title, meta.Author, meta.SourceBranch, meta.TargetBranch)

	if includeDescription && strings.TrimSpace(meta.Description) != "" {
		b.WriteString("Description:\n")
		b.WriteString(strings.TrimSpace(meta.Description))
		b.WriteString("\n\n")
	}

	b.WriteString("Files changed:\n\n")
	for _, c := range chunks {
		writeChunk(&b, c)
	}
	b.WriteString("Review the above diff and emit the JSON object described in your instructions.\n")
	if suffix = strings.TrimSpace(suffix); suffix != "" {
		b.WriteString("\n# Team-specific context (operator-supplied)\n\n")
		b.WriteString(suffix)
		b.WriteString("\n")
	}
	return b.String()
}

// writeChunk emits one labeled diff block. The label carries file
// status (NEW / DELETED / RENAMED) and the part number when a file
// was split across chunks.
func writeChunk(b *strings.Builder, c Chunk) {
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
		fmt.Fprintf(&label, " (part %d/%d)", c.Part, c.TotalParts)
	}
	label.WriteString("\n")

	fmt.Fprintf(b, "=== %s", label.String())
	b.WriteString(c.Diff)
	if !strings.HasSuffix(c.Diff, "\n") {
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "=== End %s", label.String())
}

// toStringSlice converts []Category to []string for joining.
func toStringSlice(cs []Category) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}
