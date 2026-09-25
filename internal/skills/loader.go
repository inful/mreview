// Package skills loads review guidance (.md files) from a shared
// GitLab repository's skills/ directory. The loader is the
// application layer on top of the gitlab repository-files transport
// (internal/gitlab/repository_files.go) and the source of truth for
// the skills MCP server (internal/skills/mcp).
//
// One Loader is built per mreview invocation. Loader discovers
// every .md blob in <repo>/<directory> at <ref>, fetches its body,
// extracts a one-paragraph description, and caches the result for
// the rest of the run. The agent reads the cache via the MCP
// server's list_skills / read_skill tools.
//
// Design choices:
//   - Filtering happens at Load: only blobs whose Name ends in
//     ".md" become Skills. Trees (subdirectories) and non-markdown
//     files are silently skipped — the agent never sees them.
//   - Description extraction is bounded (200 chars) so list_skills
//     output stays compact regardless of skill size. Long
//     descriptions belong in the skill body.
//   - One failed file does not fail Load. The error is logged at
//     WARN and the file is skipped; the agent gets the rest.
//   - The cache is process-lifetime. Refreshing across runs is
//     handled by mreview being rebuilt per CLI invocation — no
//     in-process invalidation needed.
package skills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/inful/mreview/internal/gitlab"
)

// ErrSkillNotFound is returned by Read when no skill matches the
// given name. Distinct from transport errors so callers can branch
// cleanly with errors.Is(err, ErrSkillNotFound).
var ErrSkillNotFound = errors.New("skills: not found")

// descriptionCap is the upper bound on extracted descriptions.
// Keeps list_skills output compact regardless of skill size.
const descriptionCap = 200

// Skill is one .md file under <repo>/<directory>/.
type Skill struct {
	// Name is the filename without the directory prefix and the
	// .md extension (e.g. "go-review"). It's the agent's handle
	// for read_skill.
	Name string

	// Path is the repo-relative path the GitLab tree API
	// returned (e.g. "skills/go-review.md"). Useful for
	// provenance logging and for read_skill clients that want
	// to display the source location.
	Path string

	// Description is the first non-empty paragraph of the body,
	// trimmed and capped at descriptionCap characters. Surfaced
	// by list_skills so the agent can decide whether to read
	// without paying the body bytes.
	Description string

	// SHA is the GitLab blob ID. Carried for cache-invalidation
	// possibilities; unused by the loader today.
	SHA string

	// Body is the raw markdown bytes. Returned verbatim by
	// read_skill — no frontmatter parsing, no markdown
	// rendering. The agent decides how to interpret it.
	Body []byte
}

// SkillFetcher is the transport-side dependency. Satisfied by
// *gitlab.Client; fakes drive unit tests.
//
// The interface deliberately mirrors the two methods the existing
// repository-files transport exposes, so production wiring is a
// one-liner (`skills.New(glClient, ...)`) and tests don't need a
// real GitLab.
type SkillFetcher interface {
	ListRepositoryTree(ctx context.Context, project, path, ref string) ([]gitlab.TreeNode, error)
	GetRepositoryFileRaw(ctx context.Context, project, path, ref string) ([]byte, error)
}

// Loader discovers and caches skills from a GitLab repo.
//
// Loader is safe for concurrent use after construction. Load
// populates the cache once; subsequent Loads return the cached
// slice without re-fetching. Read returns one cached skill by
// name and never hits GitLab on its own.
//
// The zero value is not usable; construct via New.
type Loader struct {
	fetcher   SkillFetcher
	repoPath  string // e.g. "inful/mreview-skills"
	directory string // e.g. "skills"
	ref       string // e.g. "main" or a pinned SHA; "" = project's default branch
	logger    *slog.Logger

	mu     sync.Mutex
	cache  map[string]Skill // keyed by Name
	loaded bool
}

// New returns a Loader pointed at repoPath's directory at ref.
// logger may be nil; slog.Default() is used in that case.
func New(fetcher SkillFetcher, repoPath, directory, ref string, logger *slog.Logger) *Loader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Loader{
		fetcher:   fetcher,
		repoPath:  repoPath,
		directory: directory,
		ref:       ref,
		logger:    logger,
	}
}

// RepoPath returns the configured repo path (e.g.
// "inful/mreview-skills"). Exposed so the MCP server can log the
// source for debuggability.
func (l *Loader) RepoPath() string { return l.repoPath }

// Directory returns the configured directory within the repo
// (e.g. "skills").
func (l *Loader) Directory() string { return l.directory }

// Ref returns the configured ref (branch / tag / SHA; "" for
// the project's default branch).
func (l *Loader) Ref() string { return l.ref }

// Load discovers all skills in the configured directory, fetches
// each body, and caches them.
//
// First call hits GitLab (one ListRepositoryTree + N
// GetRepositoryFileRaw calls). Subsequent calls return the cached
// slice without re-fetching.
//
// Filtering: only TreeNode.Type=="blob" AND Name ends in ".md"
// survive. Subdirectories and non-markdown files are silently
// skipped — the agent never sees them in list_skills.
//
// Error handling: a ListRepositoryTree error is fatal (the loader
// has no idea what skills exist). A per-file fetch error is
// logged at WARN and the file is skipped; Load still returns the
// successfully-fetched skills. This keeps a single transient
// failure from blanking out the whole skill set.
func (l *Loader) Load(ctx context.Context) ([]Skill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.loaded {
		return l.snapshotLocked(), nil
	}

	nodes, err := l.fetcher.ListRepositoryTree(ctx, l.repoPath, l.directory, l.ref)
	if err != nil {
		return nil, fmt.Errorf("skills: list %s/%s: %w", l.repoPath, l.directory, err)
	}

	out := make([]Skill, 0, len(nodes))
	for _, n := range nodes {
		if !isMarkdownBlob(n) {
			continue
		}
		body, err := l.fetcher.GetRepositoryFileRaw(ctx, l.repoPath, n.Path, l.ref)
		if err != nil {
			// Don't fail the whole load for one bad file.
			// Log and skip; the agent sees the rest.
			l.logger.Warn("skills: fetch failed; skipping",
				"repo", l.repoPath,
				"path", n.Path,
				"err", err.Error(),
			)
			continue
		}
		out = append(out, Skill{
			Name:        skillNameFromFilename(n.Name),
			Path:        n.Path,
			Description: extractDescription(body),
			SHA:         n.ID,
			Body:        body,
		})
	}

	l.cache = make(map[string]Skill, len(out))
	for _, s := range out {
		l.cache[s.Name] = s
	}
	l.loaded = true
	l.logger.Info("skills loaded",
		"repo", l.repoPath,
		"directory", l.directory,
		"ref", l.ref,
		"count", len(out),
	)
	return out, nil
}

// Read returns the cached skill whose Name matches name. Returns
// ErrSkillNotFound (wrapped with the requested name) when no
// cached skill matches.
//
// Read does NOT call the fetcher on its own. The contract is:
// Load once, Read many times. This keeps Read cheap (a map
// lookup) and the cache authoritative. Callers needing on-demand
// fetch should call Load first.
func (l *Loader) Read(_ context.Context, name string) (Skill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.cache[name]
	if !ok {
		return Skill{}, fmt.Errorf("%w: %q", ErrSkillNotFound, name)
	}
	return s, nil
}

// snapshotLocked returns the cached skills as a slice. Order is
// not guaranteed (map iteration). Caller must hold l.mu.
func (l *Loader) snapshotLocked() []Skill {
	out := make([]Skill, 0, len(l.cache))
	for _, s := range l.cache {
		out = append(out, s)
	}
	return out
}

// isMarkdownBlob returns true for tree entries that should
// become Skills. Trees (subdirectories) and non-markdown files
// are filtered out here so Load's loop stays simple.
func isMarkdownBlob(n gitlab.TreeNode) bool {
	if n.Type != "blob" {
		return false
	}
	return strings.HasSuffix(n.Name, ".md")
}

// skillNameFromFilename strips the directory and the .md
// extension: "skills/go-review.md" → "go-review". The base +
// trim combination handles both flat ("go-review.md") and nested
// ("skills/go-review.md") layouts.
func skillNameFromFilename(name string) string {
	base := filepath.Base(name)
	return strings.TrimSuffix(base, ".md")
}

// extractDescription returns the first non-empty paragraph of
// body, trimmed and capped at descriptionCap characters.
//
// A "paragraph" here is a run of non-blank lines joined by a
// single space — markdown headings, single-line descriptions, and
// multi-line summaries all collapse to one line for the
// list_skills preview. The cap keeps list output bounded
// regardless of skill size; long descriptions belong in the
// skill body itself.
//
// Frontmatter handling: a leading YAML block between two `---`
// markers (the convention for title / description / author
// metadata in markdown tooling) is skipped before paragraph
// extraction. Without this, the first paragraph would be the
// frontmatter opener ("--- author: jone ---") and the actual
// description would be invisible to list_skills.
//
// Returns "" when body is empty or all-blank.
func extractDescription(body []byte) string {
	lines := strings.Split(string(body), "\n")

	// Skip a leading YAML frontmatter block: starts with "---"
	// on line 0, ends with another "---" line. Anything after
	// the closing fence is treated as content.
	i := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		i++
		for i < len(lines) && strings.TrimSpace(lines[i]) != "---" {
			i++
		}
		if i < len(lines) {
			// Skip the closing fence too.
			i++
		}
	}

	// Skip leading blank lines.
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}

	// Collect non-blank lines until the first blank line or EOF.
	var para []string
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			break
		}
		para = append(para, line)
		i++
	}

	out := strings.TrimSpace(strings.Join(para, " "))
	if len(out) > descriptionCap {
		out = strings.TrimSpace(out[:descriptionCap])
	}
	return out
}
