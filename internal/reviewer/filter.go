package reviewer

import (
	"log/slog"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

// filterChangesByPath drops change files whose path matches any
// of the supplied glob patterns. Returns a new slice; input is
// unchanged.
//
// Pattern semantics use the full doublestar syntax
// (`**/*.pb.go`, `vendor/**`, `**/generated/**`, character
// classes). See matchesAny for the matching rules; malformed
// patterns are silently skipped so a typo doesn't drop files from
// the review.
func filterChangesByPath(in []gitlab.ChangeFile, patterns []string) []gitlab.ChangeFile {
	if len(patterns) == 0 {
		return in
	}
	out := make([]gitlab.ChangeFile, 0, len(in))
	for _, c := range in {
		if matchesAny(c.Path(), patterns) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// matchesAny reports whether path matches any doublestar glob
// pattern. Patterns support the full doublestar syntax
// (`**/*.pb.go`, `vendor/**`, `**/generated/**`, character
// classes, etc.).
//
// A malformed pattern (e.g. unmatched `[`) is silently skipped
// — operators can fix the pattern, but a typo shouldn't silently
// drop files from the review.
func matchesAny(path string, patterns []string) bool {
	for _, pattern := range patterns {
		ok, err := doublestar.PathMatch(pattern, path)
		if err != nil {
			continue
		}
		if ok {
			return true
		}
	}
	return false
}

// filterFindings drops findings that:
//   - cite a file path not in the diff (hallucination)
//   - have an empty body
//   - have an out-of-range line (negative)
//
// The remaining findings pass through to the poster.
func filterFindings(in []llm.Finding, validFiles map[string]bool, logger *slog.Logger) []llm.Finding {
	out := make([]llm.Finding, 0, len(in))
	for _, f := range in {
		if strings.TrimSpace(f.Body) == "" {
			logger.Debug("drop finding: empty body", "file", f.File)
			continue
		}
		if !validFiles[f.File] {
			logger.Warn("drop finding: file not in diff (LLM hallucination?)",
				"file", f.File, "line", f.Line,
			)
			continue
		}
		if f.Line < 0 {
			logger.Debug("drop finding: negative line", "file", f.File, "line", f.Line)
			continue
		}
		out = append(out, f)
	}
	return out
}
