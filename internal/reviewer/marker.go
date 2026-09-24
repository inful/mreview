package reviewer

import (
	"regexp"
	"strings"

	"github.com/inful/mreview/internal/gitlab"
)

// MarkerFormat is the HTML-comment line that mreview embeds at
// the top of every summary discussion post. Hidden in GitLab's
// rendered view, parseable by a follow-up mreview run on the
// same MR to detect prior runs and decide whether to skip /
// update.
//
//	<!-- mreview:commit=<sha> findings=<id1>,<id2>,... -->
//
// Two space-separated key/value pairs:
//   - commit=<sha>: the MR's source-branch HEAD SHA at the
//     time of the run (matches mr.SHA). When a follow-up run
//     finds the same SHA, the run is skipped.
//   - findings=<ids>: comma-separated discussion IDs of the
//     inline findings mreview posted on this run. The next
//     run resolves these so they collapse in the UI.
//
// Format is intentionally simple — it's a hidden HTML
// comment, not a real structured log. We only need the parse
// to be robust enough to identify prior runs; the worst-case
// failure is "no prior marker found → fresh review", which is
// the current behaviour.
const MarkerFormat = `<!-- mreview:commit=%s findings=%s -->`

// markerLineRE matches the marker shape with the two capture
// groups. The findings group is optional — when the run
// produced no inline findings (clean review), the marker
// collapses to just the commit field. The regex is permissive
// about whitespace so hand-edited bodies or bodies that
// lost trailing whitespace still parse.
var markerLineRE = regexp.MustCompile(`<!--\s*mreview:commit=(\S+)(?:\s+findings=(\S*))?\s*-->`)

// PriorReview is the parsed shape of an mreview summary that
// a previous run left on the MR. The orchestrator reads it
// from ListDiscussions before deciding whether to skip the
// current run (Commit == mr.SHA), update the prior (Commit
// != mr.SHA), or post fresh (no prior found).
type PriorReview struct {
	// Commit is the MR's source-branch HEAD SHA captured at
	// the time of the prior run.
	Commit string

	// SummaryDiscussionID is the GitLab discussion ID of the
	// prior summary post. Resolving this collapses the prior
	// summary in the MR's activity feed.
	SummaryDiscussionID string

	// FindingDiscussionIDs is the list of inline-finding
	// discussion IDs posted by the prior run. Each is resolved
	// so the prior findings collapse alongside the prior
	// summary.
	FindingDiscussionIDs []string
}

// FormatMarkerLine returns the marker line for a run whose
// summary was just posted. Used to embed commit + finding
// IDs at the top of the summary body so the NEXT run can
// find them.
//
// When findingIDs is empty (clean review, no inline findings
// posted), the marker collapses to just the commit field:
//
//	<!-- mreview:commit=fff -->
//
// rather than emitting an empty findings= slot. The regex
// accepts both shapes, but the empty-findings form is the
// one GitLab will see most often and we want it to render
// compactly.
func FormatMarkerLine(commit string, findingIDs []string) string {
	if len(findingIDs) == 0 {
		return "<!-- mreview:commit=" + commit + " -->"
	}
	return "<!-- mreview:commit=" + commit +
		" findings=" + strings.Join(findingIDs, ",") + " -->"
}

// ParseMarkerLine reads a discussion body and returns the
// PriorReview if the body contains a marker, or nil
// otherwise. Multi-line bodies are supported: the marker
// may appear anywhere in the body, including after the
// human-readable header.
func ParseMarkerLine(body string) *PriorReview {
	matches := markerLineRE.FindStringSubmatch(body)
	if matches == nil {
		return nil
	}
	commit := matches[1]
	rawFindings := matches[2]
	var findingIDs []string
	if rawFindings != "" {
		findingIDs = strings.Split(rawFindings, ",")
	}
	return &PriorReview{
		Commit:               commit,
		FindingDiscussionIDs: findingIDs,
	}
}

// FindPriorMReviewSummary scans the MR's existing discussions
// for one whose first note carries the mreview marker. The
// most recent such summary wins (GitLab's ListDiscussions
// returns newest-first). Returns nil if no prior summary
// exists — the caller treats that as "fresh review".
//
// We match against the FIRST note's body because that's
// what mreview writes the marker into (the summary post
// itself). Subsequent notes (replies, system messages) are
// ignored.
func FindPriorMReviewSummary(discussions []gitlab.Discussion) *PriorReview {
	for i := range discussions {
		body := discussions[i].FirstNoteBody()
		if prior := ParseMarkerLine(body); prior != nil {
			prior.SummaryDiscussionID = discussions[i].ID
			return prior
		}
	}
	return nil
}
