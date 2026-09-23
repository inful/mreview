package prompts

import (
	"fmt"
	"strings"

	"github.com/inful/mreview/internal/ci/artifact"
)

// RenderArtifactBlock produces the "CI artifacts" markdown
// block the orchestrator injects into the harness prompt.
//
// The block surfaces each artifact's status explicitly:
//   - present + parsed → "present (N KB) — see below"
//   - present + malformed → "present, malformed JSON — see raw"
//   - missing / empty / unreadable → "NOT AVAILABLE (file not
//     found)" / "NOT AVAILABLE (file empty)"
//
// The agent sees the status list BEFORE the content so it
// can self-calibrate confidence — a finding that depends
// on `test_results.json` is weaker when that artifact is
// unavailable.
func RenderArtifactBlock(set artifact.Set) string {
	var b strings.Builder
	b.WriteString("## CI artifacts available in this run\n\n")
	fmt.Fprintf(&b, "Source directory: `%s`\n\n", set.SourceDir)

	// Per-artifact status list.
	b.WriteString("- **build.log**: ")
	b.WriteString(formatStatus(set.Build))
	b.WriteString("\n")
	b.WriteString("- **test_results.json**: ")
	b.WriteString(formatStatus(set.Tests))
	b.WriteString("\n")
	b.WriteString("- **lint.json**: ")
	b.WriteString(formatStatus(set.Lint))
	b.WriteString("\n")
	b.WriteString("- **vulns.json**: ")
	b.WriteString(formatStatus(set.Vulns))
	b.WriteString("\n\n")

	// Per-artifact content.
	writeBuildArtifact(&b, set.Build)
	writeTestsArtifact(&b, set.Tests)
	writeLintArtifact(&b, set.Lint)
	writeVulnsArtifact(&b, set.Vulns)

	b.WriteString("**Review with the available artifacts in mind.** Missing artifacts should reduce your confidence in any finding that would have been corroborated by them. State explicitly when a finding depends on an artifact that was unavailable.\n")

	return b.String()
}

// formatStatus is the per-artifact one-liner the agent sees
// in the status list. Three cases:
//
//   - parsed successfully  → "present (1.2 KB) — see below"
//   - malformed JSON       → "present, malformed — see raw"
//   - missing / unreadable → "NOT AVAILABLE (file not found)"
func formatStatus[T any](r artifact.LoadResult[T]) string {
	switch {
	case r.Value != nil:
		return fmt.Sprintf("present (%.1f KB) — see below",
			float64(r.SizeBytes)/1024)
	case r.ParseError != nil:
		return "present, malformed JSON — see raw"
	default:
		return "NOT AVAILABLE"
	}
}

func writeBuildArtifact(b *strings.Builder, r artifact.LoadResult[artifact.BuildResult]) {
	b.WriteString("### build.log\n\n")
	if r.Value == nil {
		b.WriteString(artifactMissingNote())
		return
	}
	if r.Value.BuildError {
		b.WriteString("**Build errors detected.** Tail:\n```\n")
	} else {
		b.WriteString("Tail:\n```\n")
	}
	b.WriteString(r.Value.LastLines)
	b.WriteString("\n```\n")
}

func writeTestsArtifact(b *strings.Builder, r artifact.LoadResult[artifact.TestResult]) {
	b.WriteString("### test_results.json\n\n")
	if r.Value == nil {
		b.WriteString(artifactMissingNote())
		return
	}
	v := r.Value
	fmt.Fprintf(b,
		"Pass: %d · Fail: %d · Skip: %d · Total: %d\n\n",
		v.Pass, v.Fail, v.Skip, v.Total,
	)
	if len(v.Failures) > 0 {
		b.WriteString("Failing tests:\n")
		for _, f := range v.Failures {
			fmt.Fprintf(b, "- `%s` (%s)\n", f.Test, f.Package)
		}
	}
}

func writeLintArtifact(b *strings.Builder, r artifact.LoadResult[artifact.LintResult]) {
	b.WriteString("### lint.json\n\n")
	if r.Value == nil {
		b.WriteString(artifactMissingNote())
		return
	}
	v := r.Value
	fmt.Fprintf(b,
		"Total: %d · Errors: %d · Warnings: %d\n\n",
		v.Total, v.Errors, v.Warnings,
	)
	if len(v.Issues) == 0 {
		return
	}
	b.WriteString("Top issues (capped at ")
	fmt.Fprintf(b, "%d):\n", len(v.Issues))
	for _, iss := range v.Issues {
		fmt.Fprintf(b, "- [%s] %s:%d — %s (%s)\n",
			iss.Severity, iss.Pos.Filename, iss.Pos.Line,
			iss.Text, iss.FromLinter,
		)
	}
}

func writeVulnsArtifact(b *strings.Builder, r artifact.LoadResult[artifact.VulnResult]) {
	b.WriteString("### vulns.json\n\n")
	if r.Value == nil {
		b.WriteString(artifactMissingNote())
		return
	}
	v := r.Value
	fmt.Fprintf(b, "Total findings: %d\n\n", v.Total)
	if len(v.Findings) == 0 {
		return
	}
	for _, f := range v.Findings {
		fmt.Fprintf(b, "- **%s** [%s] `%s@%s` — %s\n",
			f.ID, f.Severity, f.Package, f.Version, f.Summary,
		)
	}
}

// artifactMissingNote is the "NOT AVAILABLE" body the prompt
// uses for missing / empty / unreadable artifacts. Same
// shape for every artifact so the agent can pattern-match.
func artifactMissingNote() string {
	return "*Not available — file missing, empty, or unreadable.*\n\n"
}
