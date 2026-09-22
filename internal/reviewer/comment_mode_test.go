package reviewer

import (
	"strings"
	"testing"
)

func TestParseCommentMode(t *testing.T) {
	for _, tc := range []struct {
		in       string
		want     CommentMode
		wantErr  bool
		wantText string
	}{
		{"", CommentModeBoth, false, "both"},
		{"both", CommentModeBoth, false, "both"},
		{"inline", CommentModeInlineOnly, false, "inline-only"},
		{"inline-only", CommentModeInlineOnly, false, "inline-only"},
		{"summary", CommentModeSummaryOnly, false, "summary-only"},
		{"summary-only", CommentModeSummaryOnly, false, "summary-only"},
		{"BOTH", CommentModeBoth, false, "both"},                         // case-insensitive
		{"  Inline-Only  ", CommentModeInlineOnly, false, "inline-only"}, // whitespace-tolerant
		{"bogus", CommentModeBoth, true, "unknown comment-mode"},         // error path
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseCommentMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ParseCommentMode(%q): expected error", tc.in)
				} else if !strings.Contains(err.Error(), tc.wantText) {
					t.Errorf("error = %q, want substring %q", err.Error(), tc.wantText)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseCommentMode(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseCommentMode(%q) = %v, want %v", tc.in, got, tc.want)
			}
			if got.String() != tc.wantText {
				t.Errorf("String() = %q, want %q", got.String(), tc.wantText)
			}
		})
	}
}

func TestCommentModeString_RoundTrip(t *testing.T) {
	// Every constant should round-trip through String → Parse.
	for _, m := range []CommentMode{CommentModeBoth, CommentModeInlineOnly, CommentModeSummaryOnly} {
		got, err := ParseCommentMode(m.String())
		if err != nil {
			t.Errorf("ParseCommentMode(%q): %v", m.String(), err)
		}
		if got != m {
			t.Errorf("round-trip %v → %q → %v", m, m.String(), got)
		}
	}
}
