package reviewer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

// fingerprintSet is the deduplication set built from prior
// bot-authored comments on the MR.
//
// Two comments share a fingerprint when their (file, line, body)
// triple hashes to the same value. Re-running mreview on the same
// MR skips any finding whose fingerprint is already in the set —
// making the operation idempotent.
type fingerprintSet struct {
	set map[string]string // fingerprint → body that produced it (for logs)
	bot string            // bot username; only that author's notes count
}

// newFingerprintSet computes the set from a slice of existing
// discussions.
//
// botUsername identifies the bot's own comments. Empty botUsername
// is allowed (treats every comment as the bot's) — useful in
// tests where the stub doesn't pin an author.
//
// We use the FIRST note's body in each discussion (that's the
// inline comment the LLM originally posted; replies are skipped
// because they're not the bot's authoritative finding).
func newFingerprintSet(discs []gitlab.Discussion, botUsername string) *fingerprintSet {
	fs := &fingerprintSet{
		set: make(map[string]string),
		bot: botUsername,
	}
	for _, d := range discs {
		// Skip the "summary note" — it's not anchored to a file,
		// so it has no (file, line) tuple and can't match a
		// finding. Filtering it out also keeps a re-run from
		// deduping the summary itself.
		if d.IndividualNote {
			continue
		}
		body := d.FirstNoteBody()
		if body == "" {
			continue
		}
		// Author filter: when botUsername is set, only count
		// notes authored by the bot.
		if botUsername != "" {
			if len(d.Notes) == 0 {
				continue
			}
			if d.Notes[0].Author.Username != botUsername {
				continue
			}
		}
		// Extract (file, line) from the FIRST note's body prefix
		// (the inline comment carries a "[severity]" header and
		// the GitLab UI embeds the file:line at the start of the
		// position; we can't read the position back through the
		// discussions API, so the body itself is the fingerprint
		// key).
		//
		// To make fingerprints robust against minor wording drift
		// while still unique enough to dedupe, we hash the
		// trimmed body. Future enhancement: also parse the
		// position out of `note.position.{new_path,new_line}`
		// when GitLab exposes it.
		fp := fingerprintFromBody(body)
		fs.set[fp] = body
	}
	return fs
}

// fingerprintFromBody computes the dedupe key for a body string.
// We trim whitespace + collapse internal newlines so cosmetic
// edits to the same finding still match.
func fingerprintFromBody(body string) string {
	normalized := strings.Join(strings.Fields(body), " ")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// Contains reports whether the given finding's fingerprint is in
// the set. A finding matches when its body normalizes to the same
// hash as a prior bot comment.
func (f *fingerprintSet) Contains(finding llm.Finding) bool {
	if f == nil {
		return false
	}
	fp := fingerprintFromBody(finding.Body)
	_, ok := f.set[fp]
	return ok
}

// Size returns the number of distinct fingerprints tracked.
func (f *fingerprintSet) Size() int {
	if f == nil {
		return 0
	}
	return len(f.set)
}

// String returns a one-line description for logging.
func (f *fingerprintSet) String() string {
	if f == nil {
		return "<nil>"
	}
	return fmt.Sprintf("fingerprintSet{bot=%q, size=%d}", f.bot, f.Size())
}
