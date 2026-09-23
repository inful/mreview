package tokensave

import (
	"testing"
	"time"
)

// TestSpawnMCPServer_Defaults pins the stdio-wired ServerConfig
// shape that the orchestrator passes to harness. The harness
// library consumes this struct directly — if any field is
// wrong (Name, Command, Args, ConnectTimeout), the MCP
// server won't connect or its tools won't show up.
func TestSpawnMCPServer_Defaults(t *testing.T) {
	cfg := SpawnMCPServer(Config{
		ProjectRoot: "/tmp/repo",
	})

	if cfg.Name != "tokensave" {
		t.Errorf("Name = %q, want tokensave (determines tool namespace)", cfg.Name)
	}
	if cfg.Command != DefaultBin {
		t.Errorf("Command = %q, want %q (PATH-resolved default)",
			cfg.Command, DefaultBin)
	}
	// tokensave's MCP subcommand takes `--project-root` as
	// the second positional. Pin the exact shape so a future
	// refactor doesn't accidentally flip the args.
	wantArgs := []string{"mcp", "--project-root", "/tmp/repo"}
	if len(cfg.Args) != len(wantArgs) {
		t.Fatalf("Args = %v, want %v", cfg.Args, wantArgs)
	}
	for i := range wantArgs {
		if cfg.Args[i] != wantArgs[i] {
			t.Errorf("Args[%d] = %q, want %q", i, cfg.Args[i], wantArgs[i])
		}
	}

	// ConnectTimeout should be generous enough for a cold
	// tokensave sync on a large repo. 5s (the harness default
	// for *Optional* servers) is too tight — tokensave's
	// first-call handshake can take 10–20s while it indexes
	// the repo.
	if cfg.ConnectTimeout <= 5*time.Second || cfg.ConnectTimeout >= 60*time.Second {
		t.Errorf("ConnectTimeout = %v; want in (5s, 60s)", cfg.ConnectTimeout)
	}
}

// TestSpawnMCPServer_CustomBin confirms that operators with a
// non-PATH tokensave install can pass an absolute path via
// Config.Bin.
func TestSpawnMCPServer_CustomBin(t *testing.T) {
	cfg := SpawnMCPServer(Config{
		ProjectRoot: "/tmp/repo",
		Bin:         "/opt/tools/bin/tokensave",
	})
	if cfg.Command != "/opt/tools/bin/tokensave" {
		t.Errorf("Command = %q, want custom path", cfg.Command)
	}
}

// TestSpawnMCPServer_ExtraArgs confirms the Args passthrough.
// Operators may want to forward `--log-level=debug` or
// similar tunables to the tokensave subprocess.
func TestSpawnMCPServer_ExtraArgs(t *testing.T) {
	cfg := SpawnMCPServer(Config{
		ProjectRoot: "/tmp/repo",
		Args:        []string{"--log-level=debug"},
	})
	want := []string{"mcp", "--project-root", "/tmp/repo", "--log-level=debug"}
	if len(cfg.Args) != len(want) {
		t.Fatalf("Args = %v, want %v", cfg.Args, want)
	}
	for i := range want {
		if cfg.Args[i] != want[i] {
			t.Errorf("Args[%d] = %q, want %q", i, cfg.Args[i], want[i])
		}
	}
}

// TestSpawnMCPServer_NamespaceContract pins the relationship
// between Name and the harness MCP tool-namespace convention.
// The harness library prefixes MCP tools with "mcp__<Name>__".
// tokensave's tools become "mcp__tokensave__smart_context",
// "mcp__tokensave__semantic_search", "mcp__tokensave__impact_analysis"
// in the agent's tool registry. The system prompt in
// internal/prompts/review.go references these exact names —
// renaming this constant will break the agent's ability to
// call the tools.
func TestSpawnMCPServer_NamespaceContract(t *testing.T) {
	cfg := SpawnMCPServer(Config{ProjectRoot: "/tmp/repo"})
	wantPrefix := "mcp__" + cfg.Name + "__"
	const want = "mcp__tokensave__"
	if wantPrefix != want {
		t.Errorf("namespace prefix = %q, want %q (used by the system prompt)", wantPrefix, want)
	}
}
