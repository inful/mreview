package gitlab

// NotesBodies returns the body of every note in the
// discussion (including replies). Empty bodies are filtered.
//
// The reviewer uses this to enumerate the existing bot-authored
// comments when computing the fingerprint set for dedupe.
func (d Discussion) NotesBodies() []string {
	out := make([]string, 0, len(d.Notes))
	for _, n := range d.Notes {
		if n.Body != "" {
			out = append(out, n.Body)
		}
	}
	return out
}

// FirstNoteBody returns the first note's body in the discussion,
// which is the one the LLM-emitted inline comment will have
// produced (subsequent notes are replies).
//
// Returns "" for a discussion with no notes.
func (d Discussion) FirstNoteBody() string {
	if len(d.Notes) == 0 {
		return ""
	}
	return d.Notes[0].Body
}
