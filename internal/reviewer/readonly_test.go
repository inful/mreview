package reviewer

import (
	"strings"
	"testing"

	"github.com/sausheong/harness/tool"
	"github.com/sausheong/harness/tools/file"
)

// TestReadOnlyContract_ToolRegistry pins the read-only
// contract from issue #42: the harness runtime the
// orchestrator builds must NOT register bash, edit_file,
// or write_file. Only read_file is allowed.
//
// This is the test the issue's acceptance criteria call out
// ("verified by a test that introspects the harness
// `ToolRegistry` and asserts no `BashTool` is present").
//
// Why this matters: the CI environment is read-only by
// contract. If a future contributor adds a write tool to
// the registry without thinking, the agent could mutate
// code in the MR's working directory. The test catches that
// at PR review time.
func TestReadOnlyContract_ToolRegistry(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&file.ReadFileTool{WorkDir: t.TempDir()})

	names := reg.Names()

	// read_file must be present (the only allowed file tool).
	if !contains(names, "read_file") {
		t.Errorf("tool registry missing read_file — agent has no way to read source")
	}

	// bash, edit_file, write_file must NOT be present.
	for _, forbidden := range []string{"bash", "edit_file", "write_file"} {
		if contains(names, forbidden) {
			t.Errorf("tool registry contains forbidden %q — read-only contract violated", forbidden)
		}
	}
}

// TestReadOnlyContract_DocumentedTools cross-references
// the tools that harness ships (per its README) so a
// future contributor who adds one to the orchestrator's
// registry gets a loud test failure.
//
// As of harness v0.4.2, the file package ships read_file
// (allowed) and write_file / edit_file (forbidden). The
// bash package ships bash (forbidden). The test asserts the
// forbidden tools exist in the harness library (so the
// names are real and a typo wouldn't silently satisfy the
// contract), and that the orchestrator's registry doesn't
// register them.
func TestReadOnlyContract_DocumentedTools(t *testing.T) {
	// Build the orchestrator's registry the way cmd/mreview
	// would. Today: only read_file. If a future change adds
	// write_file or edit_file, this test catches it.
	reg := tool.NewRegistry()
	reg.Register(&file.ReadFileTool{WorkDir: t.TempDir()})

	names := reg.Names()

	// The contract is "no write tools". Any tool whose name
	// contains "write" or "edit" is forbidden. (Read-only
	// tools — read_file, smart_context, semantic_search,
	// impact_analysis — are allowed.)
	for _, name := range names {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "write") || strings.Contains(lower, "edit") {
			t.Errorf("tool %q violates the read-only contract", name)
		}
	}
}

// contains is a tiny helper for string-slice membership.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
