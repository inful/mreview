package reviewer

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

func TestFingerprintFromBody_Normalizes(t *testing.T) {
	// Whitespace + newlines collapse to a stable form.
	a := fingerprintFromBody("**[warning]**  jwt\n  secret\n")
	b := fingerprintFromBody("**[warning]** jwt secret")
	if a != b {
		t.Errorf("whitespace should normalize: %q vs %q", a, b)
	}
}

func TestFingerprintFromBody_DistinctBodies(t *testing.T) {
	a := fingerprintFromBody("jwt secret leak")
	b := fingerprintFromBody("jwt secret leak ")
	c := fingerprintFromBody("JWT secret leak")
	// a == b (trailing space normalized); a != c (case-sensitive).
	if a != b {
		t.Errorf("trailing space should normalize: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("case difference should produce distinct fingerprint: %q", a)
	}
}

func TestNewFingerprintSet_BotFilter(t *testing.T) {
	// Discussion 1: bot author → included.
	// Discussion 2: human author → excluded (when BotUsername set).
	// Discussion 3: IndividualNote (summary) → excluded.
	discs := []gitlab.Discussion{
		{
			IndividualNote: false,
			Notes: []gitlab.Note{
				{Body: "**[warning]** x", Author: gitlab.User{Username: "review-bot"}},
				{Body: "agree", Author: gitlab.User{Username: "alice"}},
			},
		},
		{
			IndividualNote: false,
			Notes: []gitlab.Note{
				{Body: "**[info]** human comment", Author: gitlab.User{Username: "alice"}},
			},
		},
		{
			IndividualNote: true,
			Notes: []gitlab.Note{
				{Body: "summary note body", Author: gitlab.User{Username: "review-bot"}},
			},
		},
	}

	withBot := newFingerprintSet(discs, "review-bot")
	if withBot.Size() != 1 {
		t.Errorf("with bot filter: size = %d, want 1", withBot.Size())
	}

	withoutBot := newFingerprintSet(discs, "")
	if withoutBot.Size() != 2 {
		// discussion 1 (bot body) + discussion 2 (alice body).
		// discussion 3 is excluded because IndividualNote.
		t.Errorf("without bot filter: size = %d, want 2", withoutBot.Size())
	}
}

func TestNewFingerprintSet_EmptyDiscussions(t *testing.T) {
	fs := newFingerprintSet(nil, "bot")
	if fs.Size() != 0 {
		t.Errorf("size = %d, want 0", fs.Size())
	}
	if fs.Contains(llm.Finding{Body: "anything"}) {
		t.Error("Contains on empty set should return false")
	}
}

func TestNewFingerprintSet_NilDiscussionNotes(t *testing.T) {
	// Defensive: a discussion with no notes doesn't crash.
	discs := []gitlab.Discussion{
		{IndividualNote: false, Notes: nil},
		{IndividualNote: false, Notes: []gitlab.Note{}},
	}
	fs := newFingerprintSet(discs, "")
	if fs.Size() != 0 {
		t.Errorf("size = %d, want 0", fs.Size())
	}
}

func TestFingerprintSet_Contains(t *testing.T) {
	fs := newFingerprintSet([]gitlab.Discussion{
		{
			IndividualNote: false,
			Notes: []gitlab.Note{
				{Body: "**[warning]** jwt leak", Author: gitlab.User{Username: "bot"}},
			},
		},
	}, "bot")

	matches := llm.Finding{File: "x.go", Line: 1, Body: "**[warning]** jwt leak"}
	noMatch := llm.Finding{File: "x.go", Line: 1, Body: "different body"}

	if !fs.Contains(matches) {
		t.Error("expected match")
	}
	if fs.Contains(noMatch) {
		t.Error("expected no match")
	}
}

func TestDedupeAgainstSet(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	fs := newFingerprintSet([]gitlab.Discussion{
		{
			IndividualNote: false,
			Notes: []gitlab.Note{
				{Body: "**[warning]** jwt leak", Author: gitlab.User{Username: "bot"}},
			},
		},
	}, "bot")

	in := []llm.Finding{
		{File: "a.go", Line: 1, Body: "**[warning]** jwt leak"}, // duplicate
		{File: "b.go", Line: 5, Body: "new finding"},            // new
		{File: "c.go", Line: 9, Body: "**[warning]** jwt leak"}, // dup (same body, diff file)
	}
	out := dedupeAgainstSet(in, fs, logger)
	if len(out) != 1 {
		t.Fatalf("expected 1 surviving finding, got %d", len(out))
	}
	if out[0].File != "b.go" {
		t.Errorf("survivor file = %q, want b.go", out[0].File)
	}
}

func TestDedupeAgainstSet_NilSet(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	in := []llm.Finding{{Body: "x"}}
	out := dedupeAgainstSet(in, nil, logger)
	if len(out) != 1 {
		t.Errorf("nil set should pass through; got %d", len(out))
	}
}

func TestFingerprintSet_String(t *testing.T) {
	fs := newFingerprintSet(nil, "review-bot")
	got := fs.String()
	if !strings.Contains(got, "review-bot") {
		t.Errorf("String() should mention bot, got %q", got)
	}
	if !strings.Contains(got, "size=0") {
		t.Errorf("String() should mention size, got %q", got)
	}
}
