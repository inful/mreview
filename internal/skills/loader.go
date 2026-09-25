// Package skills loads review guidance (.md files) from two
// sources and merges them into a single skill set the review
// agent can browse via the skills MCP server.
//
// Sources (issue #44):
//
//  1. Bundled — a fixed set of .md files embedded in the mreview
//     binary (see internal/skills/bundled/). These ship with
//     every release so the agent has useful defaults regardless
//     of operator configuration.
//
//  2. Remote — a directory of .md files in a GitLab repository
//     (the "central skills repo"). Configured via --skills-repo.
//
// Override semantics: remote wins on Name collision. A team can
// patch any bundled skill by putting a same-named .md in their
// central repo — no fork of mreview required. This mirrors how
// /etc overrides /etc/skel and how config files override
// package defaults.
//
// Graceful degradation: if the remote fetch fails (list error,
// network outage), the loader logs a warning and falls back to
// the bundled set. The agent never sees an empty skill list
// when bundled skills exist.
//
// One Loader is built per mreview invocation. Loader discovers
// skills once via Load, caches them, and serves subsequent
// Read calls from the cache. The agent reaches the cache via
// the MCP server's list_skills / read_skill tools.
package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// bundledPathPrefix marks the Path field on bundled Skills so
// callers can tell at a glance where the skill came from.
// Remote skills use their repo-relative path; bundled use
// "bundled://<name>.md".
const bundledPathPrefix = "bundled://"

// Skill is one merged skill (bundled or remote).
type Skill struct {
	// Name is the filename without the directory prefix and the
	// .md extension (e.g. "go-review"). It's the agent's handle
	// for read_skill.
	Name string

	// Path is the source path. For remote skills: the
	// repo-relative path the GitLab tree API returned (e.g.
	// "skills/go-review.md"). For bundled skills: a
	// "bundled://<name>.md" sentinel. Useful for provenance
	// logging and list_skills output.
	Path string

	// Description is the first non-empty paragraph of the body,
	// trimmed and capped at descriptionCap characters. Surfaced
	// by list_skills so the agent can decide whether to read
	// without paying the body bytes.
	Description string

	// SHA is the GitLab blob ID for remote skills. Empty for
	// bundled skills (no version identity beyond the binary
	// release that shipped them).
	SHA string

	// Body is the raw markdown bytes. Returned verbatim by
	// read_skill — no frontmatter parsing, no markdown
	// rendering. The agent decides how to interpret it.
	Body []byte
}

// IsBundled reports whether the skill came from the embedded
// set rather than the central repo. Useful for list_skills
// output and operator logging.
func (s Skill) IsBundled() bool {
	return strings.HasPrefix(s.Path, bundledPathPrefix)
}

// SkillFetcher is the transport-side dependency for remote
// skills. Satisfied by *gitlab.Client; fakes drive unit tests.
type SkillFetcher interface {
	ListRepositoryTree(ctx context.Context, project, path, ref string) ([]gitlab.TreeNode, error)
	GetRepositoryFileRaw(ctx context.Context, project, path, ref string) ([]byte, error)
}

// Config bundles the inputs to New. New takes a Config rather
// than positional args because the param list grew with the
// bundled-set feature and a struct keeps call sites readable.
type Config struct {
	// Fetcher is the GitLab transport for remote skills. May
	// be nil when only bundled skills are desired (e.g. tests
	// that don't exercise the network path).
	Fetcher SkillFetcher

	// RepoPath is the GitLab project path the remote skills
	// live in (e.g. "inful/mreview-skills"). Empty disables
	// remote loading — only bundled skills are available.
	RepoPath string

	// Directory is the directory within RepoPath that
	// contains the .md files (e.g. "skills"). Defaults to
	// "skills" when RepoPath is non-empty.
	Directory string

	// Ref is the branch / tag / SHA the loader reads from.
	// Empty means "use the project's default branch".
	Ref string

	// Bundled is the set of always-on skills, keyed by Name.
	// May be empty (no bundled skills) or nil (same).
	// Production callers pass bundled.FS() parsed into a
	// {name: body} map; tests pass synthetic maps.
	Bundled map[string][]byte

	// Logger is used for one-line status messages and per-file
	// warnings. Nil falls back to slog.Default().
	Logger *slog.Logger
}

// Loader discovers and caches skills from a GitLab repo + a
// bundled set, merged with central-wins precedence.
//
// Loader is safe for concurrent use after construction. Load
// populates the cache once; subsequent Loads return the cached
// slice without re-fetching. Read returns one cached skill by
// name and never hits GitLab on its own.
//
// The zero value is not usable; construct via New.
type Loader struct {
	cfg Config

	mu     sync.Mutex
	cache  map[string]Skill // keyed by Name; bundled ∪ remote, remote wins
	loaded bool
}

// New returns a Loader configured by cfg.
func New(cfg Config) *Loader {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Directory == "" && cfg.RepoPath != "" {
		cfg.Directory = "skills"
	}
	return &Loader{cfg: cfg}
}

// Bundled returns the always-on skill bodies the loader was
// configured with. Useful for tests that want to assert which
// names shipped with a given build.
func (l *Loader) Bundled() map[string][]byte { return l.cfg.Bundled }

// RepoPath returns the configured remote repo path (e.g.
// "inful/mreview-skills"). Empty means "remote is disabled".
// Exposed so the MCP server can log the source for
// debuggability.
func (l *Loader) RepoPath() string { return l.cfg.RepoPath }

// Directory returns the configured directory within the remote
// repo (e.g. "skills"). Empty when remote is disabled.
func (l *Loader) Directory() string { return l.cfg.Directory }

// Ref returns the configured remote ref (branch / tag / SHA;
// "" for the project's default branch). Empty when remote is
// disabled.
func (l *Loader) Ref() string { return l.cfg.Ref }

// Load discovers all skills (bundled ∪ remote, central-wins)
// and caches them.
//
// First call hits GitLab (one ListRepositoryTree + N
// GetRepositoryFileRaw calls). Subsequent calls return the
// cached slice without re-fetching.
//
// Filtering (remote): only TreeNode.Type=="blob" AND Name
// ends in ".md" survive. Subdirectories and non-markdown
// files are silently skipped — the agent never sees them in
// list_skills.
//
// Error handling:
//   - Bundled skills are always loaded; they cannot fail at
//     runtime (the embedded FS is build-time-validated).
//   - A remote list error is logged at WARN and falls back to
//     the bundled set. The agent never sees an empty list
//     when bundled is non-empty.
//   - A per-file remote fetch error is logged at WARN and the
//     file is skipped; the load continues.
//
// Override: when a remote skill has the same Name as a
// bundled skill, the remote wins and the bundled version is
// discarded. The agent never sees the bundled variant of an
// overridden skill.
//
// Returns the merged skill set as a slice (order is not
// guaranteed). The first call returns a non-nil slice even
// when the remote fails; the second call returns the cached
// slice.
func (l *Loader) Load(ctx context.Context) ([]Skill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.loaded {
		return l.snapshotLocked(), nil
	}

	out := l.bundledLocked()

	// Fetch remote when configured. A failure here is
	// surfaced as a Load error when there's no bundled
	// fallback (operators need to know the remote is broken);
	// when bundled skills exist, the error is logged at WARN
	// and the bundled set is returned as-is.
	if l.cfg.Fetcher != nil && l.cfg.RepoPath != "" {
		if err := l.fetchRemoteLocked(ctx, out); err != nil {
			if len(l.cfg.Bundled) == 0 {
				return nil, err
			}
			l.cfg.Logger.Warn("skills: remote fetch failed; using bundled only",
				"repo", l.cfg.RepoPath,
				"directory", l.cfg.Directory,
				"err", err.Error(),
			)
		}
	}

	l.cache = out
	l.loaded = true
	l.cfg.Logger.Info("skills loaded",
		"repo", l.cfg.RepoPath,
		"directory", l.cfg.Directory,
		"ref", l.cfg.Ref,
		"bundled_count", len(l.cfg.Bundled),
		"merged_count", len(out),
	)
	return l.snapshotLocked(), nil
}

// Read returns the cached skill whose Name matches name.
// Returns ErrSkillNotFound (wrapped with the requested name)
// when no cached skill matches.
//
// Read does NOT call the fetcher on its own. The contract is:
// Load once, Read many times. This keeps Read cheap (a map
// lookup) and the cache authoritative. Callers needing
// on-demand fetch should call Load first.
func (l *Loader) Read(_ context.Context, name string) (Skill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.cache[name]
	if !ok {
		return Skill{}, fmt.Errorf("%w: %q", ErrSkillNotFound, name)
	}
	return s, nil
}

// bundledLocked returns a name→Skill map populated from the
// configured bundled set. Each Skill's Path uses the
// "bundled://" prefix so the source is identifiable in
// list_skills output. Caller must hold l.mu.
func (l *Loader) bundledLocked() map[string]Skill {
	out := make(map[string]Skill, len(l.cfg.Bundled))
	for name, body := range l.cfg.Bundled {
		out[name] = Skill{
			Name:        name,
			Path:        bundledPathPrefix + name + ".md",
			Description: extractDescription(body),
			Body:        body,
		}
	}
	return out
}

// fetchRemoteLocked lists the configured directory, fetches
// each .md body, and merges into out. Remote wins on Name
// collision with a bundled skill — the bundled entry is
// silently replaced. A non-nil error is returned only for
// catastrophic failures (list error); per-file fetch errors
// are logged and skipped.
//
// Caller must hold l.mu.
func (l *Loader) fetchRemoteLocked(ctx context.Context, out map[string]Skill) error {
	nodes, err := l.cfg.Fetcher.ListRepositoryTree(ctx, l.cfg.RepoPath, l.cfg.Directory, l.cfg.Ref)
	if err != nil {
		return fmt.Errorf("skills: list %s/%s: %w", l.cfg.RepoPath, l.cfg.Directory, err)
	}

	for _, n := range nodes {
		if !isMarkdownBlob(n) {
			continue
		}
		body, err := l.cfg.Fetcher.GetRepositoryFileRaw(ctx, l.cfg.RepoPath, n.Path, l.cfg.Ref)
		if err != nil {
			// Don't fail the whole load for one bad file.
			// Log and skip; the agent sees the rest (and
			// the bundled version of this name if any).
			l.cfg.Logger.Warn("skills: fetch failed; skipping",
				"repo", l.cfg.RepoPath,
				"path", n.Path,
				"err", err.Error(),
			)
			continue
		}
		out[skillNameFromFilename(n.Name)] = Skill{
			Name:        skillNameFromFilename(n.Name),
			Path:        n.Path,
			Description: extractDescription(body),
			SHA:         n.ID,
			Body:        body,
		}
	}
	return nil
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

// LoadBundled is a convenience for callers that have an
// embedded fs.FS (e.g. internal/skills/bundled.FS()) and want
// to materialise its .md files into the {name: body} map
// the Loader expects. Files whose Name doesn't end in ".md"
// are skipped.
//
// Returns an error when the FS can't be walked; per-file read
// errors are also returned (defensive — production FSes
// shouldn't fail per-file).
func LoadBundled(fsys fs.FS) (map[string][]byte, error) {
	if fsys == nil {
		return map[string][]byte{}, nil
	}
	out := map[string][]byte{}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("skills: read bundled dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("skills: read bundled %s: %w", e.Name(), err)
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		out[name] = data
	}
	return out, nil
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
