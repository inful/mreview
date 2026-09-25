// Package mcp exposes the skills loader as an MCP server over
// stdio. The server is meant to be spawned as a subprocess by the
// harness library (the same way tokensave is); mreview invokes it
// via `mreview skills-mcp` (a hidden subcommand) and the harness
// connects over stdio.
//
// Two tools:
//
//   - list_skills — returns one entry per discovered .md file with
//     name, description, and path. Cheap (no body bytes).
//   - read_skill  — returns the full body of one skill by name.
//
// The agent discovers skills via list_skills, then reads the
// relevant ones via read_skill. This list-then-read pattern
// mirrors tokensave's mcp__tokensave__search / mcp__tokensave__read
// shape — the agent already knows the drill.
package mcp

import (
	"context"
	"fmt"
	"os"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/inful/mreview/internal/skills"
)

// ServerImplementation is the name + version the server reports
// during the MCP initialize handshake. Pinned so client-side
// tooling can branch on it.
var ServerImplementation = &sdkmcp.Implementation{
	Name:    "mreview-skills",
	Version: "v0.1.0",
}

// listSkillsOutput is the structured response for the list_skills
// tool. The agent sees this as JSON via the MCP CallToolResult
// payload.
type listSkillsOutput struct {
	// Skills is the discovered skill metadata. Bodies are
	// omitted to keep the response bounded; the agent reads
	// individual skills via read_skill.
	Skills []skillSummary `json:"skills"`

	// Count mirrors len(Skills) for clients that prefer a scalar.
	Count int `json:"count"`

	// Source is the GitLab repo path the skills came from
	// (e.g. "inful/mreview-skills"). Surfaced for the agent's
	// provenance logging.
	Source string `json:"source"`
}

// skillSummary is one row of the list_skills output.
type skillSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
}

// readSkillInput is the input shape for read_skill. The MCP SDK
// unmarshals the JSON arguments into this struct.
type readSkillInput struct {
	// Name is the skill name (filename without .md). e.g.
	// "go-review" for "skills/go-review.md".
	Name string `json:"name"`
}

// readSkillOutput is the structured response for read_skill.
type readSkillOutput struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Body string `json:"body"`
}

// Serve starts the MCP server on stdio and blocks until ctx is
// cancelled or the client disconnects. The harness library is the
// only intended caller — it spawns the process and connects via
// stdio.
//
// The server registers two tools (list_skills, read_skill). Each
// invocation calls loader.Load exactly once (idempotent and
// cached) before the first tool call so list_skills / read_skill
// are pure cache reads.
//
// Returns nil on graceful shutdown (ctx cancelled or client
// disconnect). Transport errors surface as the returned error.
func Serve(ctx context.Context, loader *skills.Loader) error {
	if loader == nil {
		return fmt.Errorf("skills/mcp: loader is required")
	}

	// Pre-warm the cache so the first tool call doesn't pay
	// the GitLab round-trip. A Load failure here surfaces as
	// the server returning a tool error on the first call,
	// not a startup error — the server still comes up (the
	// harness treats the connection as alive) and the agent
	// sees a clear "skills unavailable" message.
	if _, err := loader.Load(ctx); err != nil {
		// Log via stderr — the server's structured logging is
		// over MCP; this is a one-time startup note. The
		// loader already logged the underlying error.
		fmt.Fprintf(os.Stderr, "skills/mcp: prewarm failed: %v\n", err)
	}

	server := sdkmcp.NewServer(ServerImplementation, &sdkmcp.ServerOptions{
		// Keep the default capabilities ("logging": {}) —
		// tools/call is added automatically when we register
		// tools below.
	})

	// Register tools. Each handler is a typed function whose
	// signature is decoded by the SDK at registration time —
	// see the go-sdk's AddTool helper.
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "list_skills",
		Description: "List available skills (review guidance .md files). Returns each skill's name, description, and source path. Cheap — no body bytes; use read_skill to fetch a body.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, listSkillsOutput, error) {
		return handleListSkills(ctx, loader)
	})

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "read_skill",
		Description: "Read the full body of one skill by name. Use list_skills first to discover names.",
	}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, input readSkillInput) (*sdkmcp.CallToolResult, readSkillOutput, error) {
		return handleReadSkill(ctx, loader, input)
	})

	return server.Run(ctx, &sdkmcp.StdioTransport{})
}

// handleListSkills returns the cached skill set as list_skills
// output. Loader.Load is called once (cached after that).
func handleListSkills(ctx context.Context, loader *skills.Loader) (*sdkmcp.CallToolResult, listSkillsOutput, error) {
	all, err := loader.Load(ctx)
	if err != nil {
		return nil, listSkillsOutput{}, fmt.Errorf("skills/mcp: list_skills: %w", err)
	}

	out := listSkillsOutput{
		Skills: make([]skillSummary, 0, len(all)),
		Source: loader.RepoPath(),
	}
	for _, s := range all {
		out.Skills = append(out.Skills, skillSummary{
			Name:        s.Name,
			Description: s.Description,
			Path:        s.Path,
		})
	}
	out.Count = len(out.Skills)
	return nil, out, nil
}

// handleReadSkill returns one cached skill's body. Returns an
// MCP-shaped error when the name is unknown so the agent sees a
// clear "no such skill" message rather than an empty payload.
func handleReadSkill(ctx context.Context, loader *skills.Loader, input readSkillInput) (*sdkmcp.CallToolResult, readSkillOutput, error) {
	if input.Name == "" {
		return nil, readSkillOutput{}, fmt.Errorf("skills/mcp: read_skill: name is required")
	}
	// Ensure the cache is populated. Read doesn't load, but
	// list-then-read callers may not have called list yet.
	if _, err := loader.Load(ctx); err != nil {
		return nil, readSkillOutput{}, fmt.Errorf("skills/mcp: read_skill: %w", err)
	}
	s, err := loader.Read(ctx, input.Name)
	if err != nil {
		return nil, readSkillOutput{}, fmt.Errorf("skills/mcp: read_skill: %w", err)
	}
	return nil, readSkillOutput{
		Name: s.Name,
		Path: s.Path,
		Body: string(s.Body),
	}, nil
}
