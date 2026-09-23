// Package tokensave wires the tokensave MCP server into the
// harness runtime as mreview's primary code-graph tool.
//
// Why this package exists: tokensave (a Rust binary at
// https://tokensave.dev/) is the language-agnostic answer to
// "what does this code do / what depends on it." It exposes
// its tool surface over the Model Context Protocol; mreview
// embeds it as a one-shot subprocess that the harness
// library spawns and tears down per Run.
//
// Per the architecture reset (#42, migration step 4),
// tokensave is the ONLY non-read_file tool the agent sees.
// The MCP server names its tools under the `tokensave__`
// namespace (harness's MCP convention), so they show up as
// `mcp__tokensave__smart_context`, `mcp__tokensave__...`
// etc. in the agent's tool registry.
package tokensave

import (
	"time"

	"github.com/sausheong/harness/tools/mcp"
)

// DefaultBin is the PATH-resolved name mreview uses when no
// --tokensave-bin override is given. Operators running tokensave
// from a non-PATH location pass the absolute path via the flag.
const DefaultBin = "tokensave"

// Config bundles the inputs SpawnMCPServer needs. Kept as a
// struct (rather than positional args) so future fields
// (e.g. server URL for HTTP transport, custom env vars) don't
// break callers.
type Config struct {
	// ProjectRoot is the repo root tokensave will index /
	// query. Passed as `--project-root <path>` to the subprocess.
	ProjectRoot string

	// Bin is the tokensave binary path. Empty = DefaultBin.
	// Operators with a non-PATH install set this to the
	// absolute path of their tokensave binary.
	Bin string

	// Args are extra arguments passed to the tokensave
	// subprocess after `--project-root`. Rarely needed; the
	// default invocation works for the common case.
	Args []string
}

// SpawnMCPServer returns the harness ServerConfig the
// orchestrator passes to runtime.BuildRuntime's
// AgentSpec.MCPServers. The harness library connects the
// subprocess, discovers tools, and registers them under the
// `mcp__tokensave__*` namespace; mreview doesn't need to
// touch the subprocess directly.
//
// Optional=true: when tokensave is unavailable (binary not
// installed, offline environment, repo too big to index in
// the connect window), the harness library logs a warning
// and continues without the tokensave tools. The agent
// falls back to read_file-only mode. A hard failure here
// would mean mreview can't review at all on repos where
// tokensave isn't deployed — too strict for the rollout
// transition.
//
// ConnectTimeout caps the connect phase at 30s — generous
// enough for a cold tokensave sync on a large repo. The
// harness library defaults to 5s for *Optional* servers
// (the connect still aborts with EOF / connect errors within
// this window).
func SpawnMCPServer(cfg Config) mcp.ServerConfig {
	bin := cfg.Bin
	if bin == "" {
		bin = DefaultBin
	}

	// tokensave's CLI exposes a `serve` subcommand that
	// starts the MCP server over stdio. We pass `--path` so
	// the server knows which project to index. (Older
	// tokensave builds called this subcommand `mcp` and the
	// flag `--project-root`; current releases use the names
	// below. The pinned bundle in the Dockerfile matches.)
	args := make([]string, 0, 3+len(cfg.Args))
	args = append(args, "serve", "--path", cfg.ProjectRoot)
	args = append(args, cfg.Args...)

	return mcp.ServerConfig{
		Name:           "tokensave",
		Command:        bin,
		Args:           args,
		ConnectTimeout: 30 * time.Second,
		Optional:       true,
	}
}
