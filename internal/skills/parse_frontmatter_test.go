package skills

import (
	"reflect"
	"testing"
)

// TestParseFrontmatter pins the narrow YAML frontmatter
// parser used by the skills loader. The parser is
// intentionally limited (single-line `key: value` only, no
// multi-line scalars) so that one short description line is
// the only shape we need to support. A future expansion
// (e.g. multiline, list values) should be a deliberate
// decision, not an accidental side effect of a YAML
// dependency — this test pins the current shape.
//
// The cases cover the three documented exit paths plus the
// two "graceful fallback" paths:
//
//   - Well-formed:  `---\nkey: val\n---\nbody`
//   - Unclosed w/blank: `---\nkey: val\n\nbody` (blank line
//     acts as a soft close)
//   - Unclosed no-blank: `---\nkey: val` (EOF; treat as
//     malformed → no FM extracted)
//   - No opening fence: `body` (no FM)
//   - Comments + keys: `---\n# comment\nkey: val\n---\n`
//   - Quoted values: `---\nkey: "quoted value"\n---\n`
func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantFM     map[string]string
		wantOffset int // 0 for the no-FM cases; byte offset otherwise
		wantHasFM  bool
	}{
		{
			name:       "no opening fence -> no FM",
			body:       "just a body, no frontmatter\n",
			wantFM:     nil,
			wantOffset: 0,
			wantHasFM:  false,
		},
		{
			name: "well-formed description",
			body: "---\ndescription: a review skill\n---\nbody content\n",
			wantFM: map[string]string{
				"description": "a review skill",
			},
			// offset points to start of "body content"
			wantHasFM: true,
		},
		{
			name: "well-formed with comments",
			body: "---\n# this is a comment\ndescription: hello\n---\nbody\n",
			wantFM: map[string]string{
				"description": "hello",
			},
			wantHasFM: true,
		},
		{
			name: "double-quoted value (quotes stripped)",
			body: "---\ndescription: \"quoted value\"\n---\nbody\n",
			wantFM: map[string]string{
				"description": "quoted value",
			},
			wantHasFM: true,
		},
		{
			name: "single-quoted value (quotes stripped)",
			body: "---\ndescription: 'single quoted'\n---\nbody\n",
			wantFM: map[string]string{
				"description": "single quoted",
			},
			wantHasFM: true,
		},
		{
			name: "multiple keys",
			body: "---\nname: my-skill\ndescription: does X\n---\nbody\n",
			wantFM: map[string]string{
				"name":        "my-skill",
				"description": "does X",
			},
			wantHasFM: true,
		},
		{
			name: "unclosed FM with blank line (soft close)",
			body: "---\ndescription: hello\n\nbody content\n",
			wantFM: map[string]string{
				"description": "hello",
			},
			wantHasFM: true,
		},
		{
			name:       "unclosed FM no blank line -> graceful fallback (no FM)",
			body:       "---\ndescription: hello",
			wantFM:     nil,
			wantOffset: 0,
			wantHasFM:  false,
		},
		{
			name: "opening fence with no body at all -> empty FM (soft close on blank)",
			// The function enters the loop, the first
			// (and only) iteration hits the blank-line
			// soft close, and returns an EMPTY map (not
			// nil — the map is allocated, just no keys
			// were parsed) plus an offset to "after the
			// blank line" (which is EOF here). This is
			// a deliberate behaviour: the FM block was
			// syntactically present, just empty.
			body:      "---\n",
			wantFM:    map[string]string{},
			wantHasFM: true,
		},
		{
			name:       "empty body -> no FM",
			body:       "",
			wantFM:     nil,
			wantOffset: 0,
			wantHasFM:  false,
		},
		{
			name: "blank line in unclosed FM ends the parse early",
			// The description line is parsed; the blank line
			// ends the FM (soft close). Everything after
			// becomes content, including the orphaned
			// "name:" line which is never reached.
			body: "---\ndescription: hello\n\nname: not parsed\n---\n",
			wantFM: map[string]string{
				"description": "hello",
			},
			wantHasFM: true,
		},
		{
			name: "key with no colon is silently skipped (not an error)",
			body: "---\nthis is not a key value pair\ndescription: hello\n---\nbody\n",
			wantFM: map[string]string{
				"description": "hello",
			},
			wantHasFM: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm, offset := parseFrontmatter([]byte(tc.body))

			if tc.wantHasFM {
				if fm == nil {
					t.Fatalf("parseFrontmatter returned nil FM, want %v", tc.wantFM)
				}
				if !reflect.DeepEqual(fm, tc.wantFM) {
					t.Errorf("FM = %v, want %v", fm, tc.wantFM)
				}
				// The offset should be > 0 (some content
				// was parsed and there's body after).
				if offset <= 0 {
					t.Errorf("offset = %d, want > 0 (content after FM)", offset)
				}
			} else {
				if fm != nil {
					t.Errorf("FM = %v, want nil", fm)
				}
				if offset != 0 {
					t.Errorf("offset = %d, want 0", offset)
				}
			}
		})
	}
}
