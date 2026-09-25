// Package bundled holds the always-on skills the mreview binary
// ships with. They're embedded at build time so the review agent
// has useful defaults regardless of whether the operator
// configured a central skills repo.
//
// Override semantics (issue #44): the Loader merges bundled and
// remote skills by Name, with **remote winning on collision**.
// That means a team can patch any bundled skill (e.g. swap
// `error-handling` for a stricter team convention) by putting
// a same-named .md in their central repo — no fork of mreview
// required.
//
// Each .md file uses YAML frontmatter to declare its
// `description:` (the value `mcp__skills__list_skills` surfaces)
// and `title:` (for editor previews). The loader reads
// `description:` from frontmatter verbatim; skills without
// frontmatter fall back to the first paragraph of the body.
// See internal/skills/bundled/skill-authoring.md for the
// authoring rules and internal/skills/loader.go for the
// extraction logic.
//
// The .md files in this directory are intentionally kept small.
// extractDescription caps descriptions at 200 chars and the
// loader passes bodies through verbatim, so anything you put
// here is what the agent sees.
package bundled

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.md
var content embed.FS

// FS returns the embedded filesystem of bundled .md files.
// File basenames (without the .md extension) become skill names
// the loader keys on.
//
// Tests construct their own bundled maps; production callers
// should pass the result of FS to the loader via its Config.
func FS() fs.FS {
	return content
}

// Names returns the sorted list of bundled skill names. Used
// by tests + the doctor command to verify the bundle didn't
// regress (e.g. a missing file would drop a name from the
// list).
func Names() ([]string, error) {
	entries, err := content.ReadDir(".")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".md"))
	}
	sort.Strings(out)
	return out, nil
}
