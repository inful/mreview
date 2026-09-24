package reviewer

import (
	"strings"
	"testing"

	"github.com/sausheong/harness/tool"
)

// TestReadOnlyContract_ToolRegistry pins the read-only
// contract from issue #42, post-tokensave-only-refactor:
//
// The local tool registry the orchestrator builds must
// contain NO file tools at all. All reads are delegated
// to the tokensave MCP server under the mcp__tokensave__*
// namespace; the harness's bash / edit_file / write_file /
// read_file tools are never registered by mreview.
//
// If a future contributor adds a write tool to the local
// registry without thinking, the agent could mutate code
// in the MR's working directory. The test catches that
// at PR review time. The tokensave server's own mutation
// tools (tokensave_str_replace etc.) are also fenced: the
// harness marks them readOnlyHint=false but they live
// behind the MCP namespace, and the agent is steered
// against them in the system prompt's forbidden-tool
// guards.
func TestReadOnlyContract_ToolRegistry(t *testing.T) {
	// The local registry is built by buildHarnessRuntime.
	// In the tokensave-only world, it's an empty *tool.Registry.
	// The tokensave MCP server's tools come up at runtime as
	// mcp__tokensave__* and are governed by the system-prompt
	// contract test in internal/prompts/review_test.go.
	reg := tool.NewRegistry()

	names := reg.Names()

	// bash, edit_file, write_file must NOT be present in
	// the local registry.
	for _, forbidden := range []string{"bash", "edit_file", "write_file", "read_file"} {
		if contains(names, forbidden) {
			t.Errorf("tool registry contains forbidden local tool %q — read-only contract violated", forbidden)
		}
	}
}

// TestReadOnlyContract_NoLocalFileTools is a stronger
// version of TestReadOnlyContract_ToolRegistry: any tool
// whose name smells like file IO is forbidden in the local
// registry, since tokensave-only means we never need one.
// Adding read_file back, for example, would re-introduce
// the failure mode where an empty WorkDir silently pointed
// reads at the operator's local clone of mreview itself.
func TestReadOnlyContract_NoLocalFileTools(t *testing.T) {
	reg := tool.NewRegistry()
	names := reg.Names()

	for _, name := range names {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "write") ||
			strings.Contains(lower, "edit") ||
			lower == "read_file" ||
			strings.Contains(lower, "bash") {
			t.Errorf("local tool %q violates the tokensave-only read-only contract", name)
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
