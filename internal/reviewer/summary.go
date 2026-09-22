package reviewer

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

// renderSummary composes the summary note body.
//
// Layout:
//
//	## mreview summary
//
//	<LLM-generated summary paragraph>
//
//	---
//	<details>
//	<summary>Findings (N)</summary>
//
//	| Severity | File:Line | Category | Comment |
//	|---|---|---|---|
//	| ... | ... | ... | ... |
//
//	</details>
//
// Empty summary → empty body → caller skips the post.
func renderSummary(mr *gitlab.MergeRequest, summaryText string, findings []llm.Finding) string {
	var b strings.Builder
	b.WriteString("## mreview summary\n\n")

	summaryText = strings.TrimSpace(summaryText)
	if summaryText == "" {
		b.WriteString("_No summary produced by the LLM._\n")
	} else {
		b.WriteString(summaryText)
		b.WriteString("\n")
	}

	if len(findings) == 0 {
		b.WriteString("\nNo findings.\n")
		return b.String()
	}

	b.WriteString("\n---\n\n<details>\n<summary>Findings (")
	b.WriteString(strconv.Itoa(len(findings)))
	b.WriteString(")</summary>\n\n")
	b.WriteString("| Severity | File:Line | Category | Comment |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, f := range findings {
		sev := string(f.Severity)
		if sev == "" {
			sev = "info"
		}
		cat := string(f.Category)
		if cat == "" {
			cat = "—"
		}
		body := strings.ReplaceAll(f.Body, "\n", " ")
		body = strings.ReplaceAll(body, "|", "\\|")
		fmt.Fprintf(&b, "| %s | `%s:%d` | %s | %s |\n",
			sev, f.File, f.Line, cat, body)
	}
	b.WriteString("\n</details>\n")
	return b.String()
}
