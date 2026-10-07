package reviewer

import (
	"log/slog"
	"sort"
	"strconv"

	"github.com/inful/mreview/internal/gitlab"
)

// FindingStatus tags a finding row in the summary table with
// its provenance relative to a prior mreview run on the same
// MR. The string values match the LLM's JSON spelling (the
// LLM emits these in its prior_findings response, and the
// orchestrator's StatusNew value is set by the orchestrator
// itself for new findings). DisplayLabel() and Emoji() map
// these to operator-facing labels / glyphs.
type FindingStatus string

const (
	// StatusNew marks a finding the LLM surfaced for the
	// first time on this run. The orchestrator sets this
	// (the LLM doesn't emit a status field on new
	// findings) and posts the finding as a new GitLab
	// inline discussion.
	StatusNew FindingStatus = "new"

	// StatusStillValid marks a prior finding the LLM
	// judged is still present in the new diff. The
	// orchestrator leaves the prior discussion open
	// (no resolve, no new post). The table shows the
	// prior body with a "carried over" display label.
	StatusStillValid FindingStatus = "still_valid"

	// StatusResolved marks a prior finding the LLM
	// judged addressed by the new changes. The
	// orchestrator auto-resolves the prior discussion
	// and shows the finding with a "resolved" display
	// label.
	StatusResolved FindingStatus = "resolved"

	// StatusOutOfScope marks a prior finding the LLM
	// judged no longer relevant to this MR (e.g. the
	// file was deleted, the symbol was renamed). The
	// orchestrator auto-resolves the prior discussion;
	// the finding appears in the table with a "resolved"
	// display label (the table groups resolved +
	// out_of_scope under the same display label since
	// both are auto-resolved).
	StatusOutOfScope FindingStatus = "out_of_scope"
)

// DisplayLabel returns the short label the summary table
// uses for the status. Kept here (not at the call site) so
// the wording is consistent across the renderer, the tests,
// and any future "filter by status" UI.
func (s FindingStatus) DisplayLabel() string {
	switch s {
	case StatusNew:
		return "new"
	case StatusStillValid:
		return "carried over"
	case StatusResolved:
		return "resolved"
	case StatusOutOfScope:
		return "out of scope"
	default:
		return string(s)
	}
}

// Emoji returns the leading glyph the summary table puts
// before the status label. Distinct shapes so the operator
// can scan the column without reading the label.
func (s FindingStatus) Emoji() string {
	switch s {
	case StatusNew:
		return "🆕"
	case StatusStillValid:
		return "↻"
	case StatusResolved:
		return "✓"
	case StatusOutOfScope:
		return "∅"
	default:
		return "•"
	}
}

// IsResolved reports whether the status warrants a
// ResolveDiscussion call. Both "resolved" (the change
// addressed the issue) and "out_of_scope" (the issue is
// no longer relevant) auto-resolve the prior discussion.
func (s FindingStatus) IsResolved() bool {
	return s == StatusResolved || s == StatusOutOfScope
}

// severityEmoji maps a severity string to its
// operator-facing emoji. Returns "" for unknown
// severities so the caller can leave the column bare
// rather than render "• unknown". The mapping matches
// the pre-Status-column summary table so existing
// operator muscle memory carries over.
func severityEmoji(severity string) string {
	switch severity {
	case "error":
		return "🛑"
	case "warning":
		return "⚠️"
	case "info":
		return "ℹ️"
	default:
		return ""
	}
}

// PriorFinding is the shape of a single prior mreview
// inline comment that the orchestrator passes to the agent
// in the user prompt. The agent uses (file, line, body,
// suggestion) to judge "is this still present in the new
// diff" — those are the only fields the LLM needs.
//
// DiscussionID and NoteID are kept here for the
// orchestrator's resolve-after-the-fact step: once the
// LLM renders a verdict, the orchestrator uses these IDs
// to call ResolveDiscussion on the right GitLab thread.
type PriorFinding struct {
	File         string
	Line         int
	Body         string
	Suggestion   string
	DiscussionID string // GitLab discussion ID for ResolveDiscussion
	NoteID       int64  // GitLab note ID (first note of the discussion)
}

// PriorFindingStatus is one entry in the LLM's
// `prior_findings` response array. The LLM mirrors the
// input prior findings (file, line) and tags each with a
// status. The orchestrator uses the status to decide
// whether to keep, resolve, or out-of-scope-resolve the
// prior GitLab discussion.
//
// Rationale is required for resolved/out_of_scope (so the
// operator can see WHY the LLM judged the prior finding
// addressed) and optional for still_valid.
type PriorFindingStatus struct {
	File      string        `json:"file"`
	Line      int           `json:"line"`
	Status    FindingStatus `json:"status"`
	Rationale string        `json:"rationale,omitempty"`
}

// extractPriorFindings walks the prior mreview summary's
// FindingDiscussionIDs, looks up each discussion in discs,
// and returns a PriorFinding for every bot-authored inline
// comment. Non-inline (summary) discussions and human
// comments are skipped — only the bot's own findings
// are passed to the LLM as "the prior review's findings".
//
// botUsername is the same filter findPriorFindingLocations
// uses; empty botUsername treats every comment as the bot's
// (used in tests).
//
// The returned slice is in the order the prior summary
// stored them (i.e. the order they were posted in the
// FindingDiscussionIDs list). The orchestrator preserves
// that order in the prompt so the LLM can refer to entries
// by index.
func extractPriorFindings(prior *PriorReview, discs []gitlab.Discussion, botUsername string) []PriorFinding {
	if prior == nil || len(prior.FindingDiscussionIDs) == 0 {
		return nil
	}
	// Build a discussion-ID → discussion map for O(1)
	// lookup. The prior's FindingDiscussionIDs are
	// GitLab discussion IDs (one per inline finding).
	byID := make(map[string]gitlab.Discussion, len(discs))
	for _, d := range discs {
		byID[d.ID] = d
	}
	out := make([]PriorFinding, 0, len(prior.FindingDiscussionIDs))
	for _, discID := range prior.FindingDiscussionIDs {
		d, ok := byID[discID]
		if !ok {
			// Discussion not in the current list —
			// either deleted out-of-band or pagination
			// skipped it. Skip silently; the LLM won't
			// be asked about it, the orchestrator won't
			// try to resolve it.
			continue
		}
		if d.IndividualNote {
			// Summary-style discussion; not an inline
			// finding. Skip.
			continue
		}
		if len(d.Notes) == 0 {
			continue
		}
		first := d.Notes[0]
		if botUsername != "" && first.Author.Username != botUsername {
			// Not authored by the bot — skip. We only
			// pass the bot's own findings to the LLM.
			continue
		}
		if first.Position == nil || first.Position.NewPath == "" {
			// No file/line anchor — not a real
			// inline finding.
			continue
		}
		out = append(out, PriorFinding{
			File:         first.Position.NewPath,
			Line:         int(first.Position.NewLine),
			Body:         first.Body,
			DiscussionID: d.ID,
			NoteID:       first.ID,
		})
	}
	return out
}

// priorKey builds the dedup key for a prior finding.
// File-scoped findings (line == 0) use just the file path.
// Used by the orchestrator to match LLM-emitted
// prior_findings status entries back to the GitLab
// discussion to resolve / keep.
func priorKey(file string, line int) string {
	if line > 0 {
		return file + ":" + strconv.Itoa(line)
	}
	return file
}

// priorActions is the orchestrator's grouping of the
// LLM's prior_findings verdict into "act on" buckets.
// All slices hold reviewer.PriorFinding values (the
// GitLab-plumbing side, with DiscussionID/NoteID) so the
// orchestrator can act on each without re-resolving the
// (file, line) → ID mapping.
type priorActions struct {
	// carryOver is the set of prior findings the LLM
	// marked still_valid. The orchestrator leaves
	// these open (no resolve, no new post).
	carryOver []PriorFinding
	// carryOverIDs is the convenience list of
	// DiscussionIDs for carryOver, in the same order.
	// Used to build the next-run's marker finding-IDs.
	carryOverIDs []string
	// resolve is the set of prior findings the LLM
	// marked resolved or out_of_scope. The orchestrator
	// calls ResolveDiscussion on each.
	resolve []PriorFinding
	// resolveStatuses mirrors `resolve` 1:1 with the
	// LLM's original status (StatusResolved or
	// StatusOutOfScope). The orchestrator uses this to
	// render the right display label in the summary
	// table — the two statuses collapse to the same
	// action (resolve) but show different labels.
	resolveStatuses []FindingStatus
	// resolveRationale mirrors `resolve` 1:1 with the
	// LLM's rationale string for each. Used by
	// buildSummaryRows to render the "Resolved: ..."
	// suffix in the summary table.
	resolveRationale []string
}

// classifyPriorFindings groups the LLM's prior_findings
// response into carryOver / resolve buckets, joined back
// to the reviewer.PriorFinding (with GitLab IDs) by
// (file, line). Prior findings the LLM didn't mention
// are treated as carryOver (the safe default — silence
// from the LLM shouldn't trigger an auto-resolve the
// operator didn't ask for).
//
// logger may be nil (tests pass nil); the classifier is
// silent in that case.
func classifyPriorFindings(llmStatuses []PriorFindingStatus, priors []PriorFinding, logger ...*slog.Logger) priorActions {
	log := func() *slog.Logger {
		if len(logger) > 0 {
			return logger[0]
		}
		return nil
	}

	// Index the LLM's statuses by (file, line) for O(1)
	// lookup. Entries not present default to carryOver.
	llmByKey := make(map[string]PriorFindingStatus, len(llmStatuses))
	for _, s := range llmStatuses {
		llmByKey[priorKey(s.File, s.Line)] = s
	}

	out := priorActions{}
	for _, pf := range priors {
		key := priorKey(pf.File, pf.Line)
		s, ok := llmByKey[key]
		if !ok {
			out.carryOver = append(out.carryOver, pf)
			out.carryOverIDs = append(out.carryOverIDs, pf.DiscussionID)
			continue
		}
		switch s.Status {
		case StatusStillValid:
			out.carryOver = append(out.carryOver, pf)
			out.carryOverIDs = append(out.carryOverIDs, pf.DiscussionID)
		case StatusResolved, StatusOutOfScope:
			out.resolve = append(out.resolve, pf)
			out.resolveStatuses = append(out.resolveStatuses, s.Status)
			out.resolveRationale = append(out.resolveRationale, s.Rationale)
			if l := log(); l != nil {
				l.Debug("prior finding marked resolved",
					"file", pf.File, "line", pf.Line,
					"status", s.Status,
					"rationale", s.Rationale,
				)
			}
		default:
			// Unknown status value — treat as
			// carryOver (safe default) and log so
			// a future schema change is visible.
			if l := log(); l != nil {
				l.Warn("prior finding has unknown status; treating as still_valid",
					"file", pf.File, "line", pf.Line,
					"status", s.Status,
				)
			}
			out.carryOver = append(out.carryOver, pf)
			out.carryOverIDs = append(out.carryOverIDs, pf.DiscussionID)
		}
	}
	return out
}

// buildSummaryRows combines the new findings, the
// carried-over prior findings, and the resolved prior
// findings into the display order for the summary table:
//  1. New findings (sorted by file:line)
//  2. Carried-over prior findings (sorted by file:line)
//  3. Resolved prior findings (sorted by file:line)
//
// The orchestrator passes the result to renderSummary.
// Rationale is taken from the LLM's prior_findings
// response (indexed positionally against `resolved`).
//
// newFindings is the post-policy, post-dedup slice
// (Finding already in the reviewer's local shape).
// carryOver and resolved are slices of PriorFinding from
// classifyPriorFindings.
func buildSummaryRows(newFindings []Finding, carryOver []PriorFinding, resolved []priorResolvedEntry) []SummaryRow {
	rows := make([]SummaryRow, 0, len(newFindings)+len(carryOver)+len(resolved))
	// New findings: StatusNew. Use the LLM's severity /
	// category from the Finding struct.
	for _, f := range newFindings {
		rows = append(rows, SummaryRow{
			File:     f.File,
			Line:     f.Line,
			Severity: string(f.Severity),
			Category: f.Category,
			Body:     f.Body,
			Status:   StatusNew,
		})
	}
	// Carried-over: use the prior body; severity/category
	// placeholder (the original is one click away on the
	// still-open discussion).
	for _, pf := range carryOver {
		rows = append(rows, SummaryRow{
			File:     pf.File,
			Line:     pf.Line,
			Severity: "(prior)",
			Category: "(prior)",
			Body:     pf.Body,
			Status:   StatusStillValid,
		})
	}
	// Resolved: same as carried-over but with the LLM's
	// rationale and a resolved status.
	for _, re := range resolved {
		rows = append(rows, SummaryRow{
			File:      re.Finding.File,
			Line:      re.Finding.Line,
			Severity:  "(prior)",
			Category:  "(prior)",
			Body:      re.Finding.Body,
			Status:    re.Status,
			Rationale: re.Rationale,
		})
	}
	// Sort each group by (file, line) for stable
	// rendering across runs. Use a single sort with
	// status as the primary key (new < still_valid <
	// resolved < out_of_scope) and (file, line) as
	// secondary.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Status != rows[j].Status {
			return statusOrder(rows[i].Status) < statusOrder(rows[j].Status)
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})
	return rows
}

// priorResolvedEntry pairs a PriorFinding with the LLM's
// status + rationale. Pulled out of priorActions so the
// buildSummaryRows call site doesn't need to know about
// rationale positional indexing.
type priorResolvedEntry struct {
	Finding   PriorFinding
	Status    FindingStatus
	Rationale string
}

// statusOrder returns a stable ordering index for the
// Status* values. Used by buildSummaryRows to put new
// findings at the top of the table and resolved findings
// at the bottom.
func statusOrder(s FindingStatus) int {
	switch s {
	case StatusNew:
		return 0
	case StatusStillValid:
		return 1
	case StatusResolved:
		return 2
	case StatusOutOfScope:
		return 3
	default:
		return 4
	}
}
