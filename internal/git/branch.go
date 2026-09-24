// Package git is a thin shim for the few git-CLI operations
// mreview needs. It is intentionally minimal: shelling out to
// `git` is the cheapest way to read branch / working-tree
// state without pulling in go-git as a dependency (which would
// add roughly 5 MiB to the binary and ~30 transitive deps).
//
// The distroless runtime image (cmd/mreview/Dockerfile) does
// NOT bundle a `git` binary, by design — see the Dockerfile
// comments on adding yet-another-binary. As a result, every
// function in this package returns a graceful no-op when
// `git` is not on PATH. That is the expected behaviour in CI;
// it lets the same binary work for local dev (where git is
// usually available) without forcing the image to grow.
//
// All functions take a workdir and run the git command scoped
// to that directory via `git -C <workdir>` — no need for
// callers to chdir or to assume the process's CWD.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrNoGit is returned when `git` is not on PATH. Callers
// should treat this as "the check is unavailable in this
// environment" and downgrade to a debug-level log; it is
// NOT a hard error condition.
var ErrNoGit = errors.New("git: binary not found in PATH")

// ErrNotARepo is returned when workdir does not contain a
// git working tree (.git / gitdir file / etc.). The caller
// can usually ignore this — workdir may not need to be a
// git repo (e.g. the operator is reviewing a vendored
// snapshot).
var ErrNotARepo = errors.New("git: workdir is not a git repository")

// errBranchUnknown is the sentinel CurrentBranch returns
// (with empty string) when the branch could not be determined
// for any reason other than missing git or non-repo. The
// helper deliberately swallows stderr so logs stay readable.
var errBranchUnknown = errors.New("git: could not determine current branch")

// CurrentBranch returns the branch name workdir is checked
// out on. Empty string + nil means "branch cannot be
// determined, but it's not a hard error" (e.g. detached HEAD,
// or workdir has no commits yet).
//
// Error return values:
//   - ErrNoGit:    `git` is not on PATH (the distroless CI
//                  image; the caller should debug-log and
//                  move on — see package doc).
//   - ErrNotARepo: workdir is not a git repo; not an error
//                  in the reviewer's view.
//   - errBranchUnknown (wrapped): any other failure (e.g.
//                  detached HEAD, shallow clone, timeout).
//
// The command has a 2-second ceiling. Reading a branch is
// microseconds of work in a normal repo; a slow git here is
// usually a sign of an oversized working tree or a broken
// repo, and we'd rather skip the diagnostic than block the
// review.
func CurrentBranch(workdir string) (string, error) {
	if workdir == "" {
		return "", fmt.Errorf("git: %w: empty workdir", errBranchUnknown)
	}
	bin, err := exec.LookPath("git")
	if err != nil {
		// PATH lookup failure. Either git isn't installed or
		// the distroless image we're running in lacks it.
		// Either way: not actionable, not a hard error.
		return "", ErrNoGit
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// `git -C <workdir> rev-parse --abbrev-ref HEAD`:
	//   - on a branch: prints the branch name ("develop", etc.)
	//   - detached HEAD: prints "HEAD"
	//   - non-repo:    exits non-zero with "fatal: not a git repository"
	// We strip whitespace, treat "HEAD" as empty (detached), and
	// map the non-repo error to ErrNotARepo so callers don't
	// have to parse git's stderr.
	cmd := exec.CommandContext(ctx, bin, "-C", workdir,
		"rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		// Distinguish "not a repo" from other failures.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if strings.Contains(stderr, "not a git repository") ||
				strings.Contains(stderr, "not a git working tree") {
				return "", ErrNotARepo
			}
		}
		return "", fmt.Errorf("%w: %v", errBranchUnknown, err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || branch == "HEAD" {
		// Detached HEAD or empty repo: not a branch.
		return "", nil
	}
	return branch, nil
}

// IsWorkdirRepo reports whether workdir is the root of a git
// working tree (i.e. contains .git, or .git is a file pointing
// at the real gitdir — worktrees, submodules). Pure Go; does
// not shell out.
//
// This exists so callers can render a friendlier hint than
// `git: workdir is not a git repository` when the operator
// pointed --workdir at e.g. /tmp. The detect happens once
// per run; the rest of the current-branch dance uses the
// current-branch helper above.
//
// Note: the check is shallow (file existence), not
// git-validated. A non-readable .git can exist in a corrupt
// repo and we wouldn't catch it here; the helper above
// would still get a real answer from the git CLI in that
// case (or an ErrNotARepo on a deeper failure).
func IsWorkdirRepo(workdir string) bool {
	if workdir == "" {
		return false
	}
	// Try both <workdir>/.git (regular repo) and
	// <workdir>/.git as a regular file (worktree submodule,
	// where .git is a file with contents like "gitdir:
	// /abs/path/to/real/.git/worktrees/foo").
	if info, err := os.Stat(workdir); err == nil && !info.IsDir() {
		return false
	}
	if _, err := os.Stat(workdir + "/.git"); err == nil {
		return true
	}
	if data, err := os.ReadFile(workdir + "/.git"); err == nil {
		return strings.HasPrefix(strings.TrimSpace(string(data)), "gitdir:")
	}
	return false
}
