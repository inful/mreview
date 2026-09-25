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
// The .md files in this directory are intentionally kept small.
// extractDescription caps descriptions at 200 chars (see
// internal/skills/loader.go) and the loader passes bodies
// through verbatim, so anything you put here is what the agent
// sees.
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
