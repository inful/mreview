package gitlab

import (
	gl "gitlab.com/gitlab-org/api/client-go"
)

// NotePosition is the subset of *gitlab.NotePosition that the
// reviewer needs to identify the file:line of an inline
// comment (the source of our file:line dedup against prior
// findings). NewPath is the path at HEAD; NewLine is the line
// at HEAD. OldPath / OldLine exist for renames and
// deleted-file comments but aren't needed for dedup today.
type NotePosition struct {
	NewPath string `json:"new_path,omitempty"`
	NewLine int64  `json:"new_line,omitempty"`
}

// Note is the subset of *gitlab.Note that the reviewer needs.
//
// Note is what GitLab returns for non-inline MR comments (the summary
// thread) and for individual replies on a discussion.
//
// Resolved is set by the GitLab API on resolvable inline
// notes; true when the operator (or a prior mreview run via
// ResolveDiscussion) marked the parent discussion resolved.
// The reviewer uses this to skip dedup against findings the
// operator has already accepted / dismissed.
//
// Position is set on inline-finding notes — empty for
// summary notes and replies on a discussion. The file:line
// key for dedup is Position.NewPath + Position.NewLine.
type Note struct {
	ID         int64         `json:"id"`
	Body       string        `json:"body"`
	Author     User          `json:"author"`
	WebURL     string        `json:"web_url"`
	System     bool          `json:"system"`
	Resolvable bool          `json:"resolvable"`
	Resolved   bool          `json:"resolved"`
	Position   *NotePosition `json:"position,omitempty"`
}

// Discussion is the subset of *gitlab.Discussion the reviewer needs.
// One Discussion wraps one or more inline comments anchored to the
// same file:line; posting returns the freshly-created thread.
//
// Resolved is the derived boolean the reviewer uses to skip
// dedup against operator-managed findings. We compute it
// from the notes here rather than pulling a top-level
// `resolved` field from upstream (the upstream Discussion
// type doesn't expose one — resolution lives on the notes).
type Discussion struct {
	ID             string `json:"id"`
	IndividualNote bool   `json:"individual_note"`
	Notes          []Note `json:"notes"`
	WebURL         string `json:"web_url"`
	Resolved       bool   `json:"resolved"`
}

// InlineComment describes a single comment to post as an inline MR
// discussion thread. The fields mirror the GitLab position object:
//
//   - File         — required: the path at HEAD (new path). For
//     deleted files this is the path the file used to have (since
//     there's no new path).
//   - OldPath      — optional: the path in the base commit. Set when
//     commenting on a modification where old_path differs from new_path
//     (rename, or just to be safe).
//   - NewLine      — line in the new file. 0 means "no new line"
//     (deleted-file comment).
//   - OldLine      — line in the old file. 0 means "no old line"
//     (new-file comment).
//   - Body         — required: the comment text. Markdown supported.
//   - Suggestion   — optional: a fenced code block posted as a
//     "suggestion" block the reviewer can apply with one click.
//     Empty string means no suggestion.
type InlineComment struct {
	File       string
	OldPath    string
	NewLine    int
	OldLine    int
	Body       string
	Suggestion string
}

// projectNote projects an upstream *gitlab.Note onto our flat Note.
//
// Note.Author is a value type (NoteAuthor), so we detect "no
// author" by an empty username — the JSON unmarshaler leaves the
// zero value when the field is absent or null.
//
// A nil *gl.Note is treated as a zero-value Note so callers can
// safely pass through empty upstream results during decoding.
func projectNote(n *gl.Note) *Note {
	if n == nil {
		return &Note{}
	}
	out := &Note{
		ID:         n.ID,
		Body:       n.Body,
		WebURL:     "",
		System:     n.System,
		Resolvable: n.Resolvable,
		Resolved:   n.Resolved,
	}
	if n.Position != nil && n.Position.NewPath != "" {
		out.Position = &NotePosition{
			NewPath: n.Position.NewPath,
			NewLine: n.Position.NewLine,
		}
	}
	if n.Author.Username != "" {
		out.Author = User{Username: n.Author.Username, Name: n.Author.Name}
	}
	return out
}

// projectDiscussion projects an upstream *gitlab.Discussion onto
// our flat Discussion, recursively projecting the inner notes.
// The Resolved field is derived from the notes (true if any
// note has Resolved=true) since the upstream type doesn't
// expose resolution at the discussion level.
func projectDiscussion(d *gl.Discussion) *Discussion {
	out := &Discussion{
		ID:             d.ID,
		IndividualNote: d.IndividualNote,
		WebURL:         "",
	}
	if len(d.Notes) > 0 {
		out.Notes = make([]Note, 0, len(d.Notes))
		for _, n := range d.Notes {
			pn := projectNote(n)
			// Resolve propagates from any note up to the
			// discussion level — once any note is marked
			// resolved, the whole thread is.
			if pn.Resolved {
				out.Resolved = true
			}
			out.Notes = append(out.Notes, *pn)
		}
	}
	return out
}
