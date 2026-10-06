package gitlab

import (
	"strings"
	"testing"
)

// TestValidatePath covers the two checks validatePath makes
// on the project path argument. The function is a cheap
// sanity check shared by every Client method that takes a
// project path; it catches the obvious programming errors
// (empty string, accidental whitespace from a CLI split)
// before they reach the GitLab API.
//
// The current implementation only checks emptiness and
// whitespace — it does NOT do path-traversal guards
// (../foo) or URL-encoding guards. Those are handled at the
// HTTP layer (the GitLab API rejects traversal paths with
// 404). The test pins exactly what the function does
// today, so a future refactor that adds more checks is
// caught by the new cases.
func TestValidatePath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"empty path rejected", "", true},
		{"single space rejected", " ", true},
		{"tab rejected", "group\tproject", true},
		{"newline rejected", "group\nproject", true},
		{"carriage return rejected", "group\rproject", true},
		{"leading whitespace rejected", " group/project", true},
		{"trailing whitespace rejected", "group/project ", true},
		{"nested path with whitespace rejected", "group/sub group", true},
		{"simple group/project accepted", "group/project", false},
		{"deeply nested path accepted", "group/sub/project", false},
		{"single segment accepted", "project", false},
		{"hyphenated group accepted", "my-group/my-project", false},
		{"dotted segments accepted (no whitespace)", "group.with.dots/project", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePath(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Errorf("validatePath(%q) = nil, want error", tc.path)
				}
				// The error message should mention 'gitlab'
				// so operators can identify the source when
				// it bubbles up.
				if !strings.Contains(err.Error(), "gitlab") {
					t.Errorf("error %q should mention 'gitlab'", err.Error())
				}
			} else if err != nil {
				t.Errorf("validatePath(%q) = %v, want nil", tc.path, err)
			}
		})
	}
}
