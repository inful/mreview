package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCurrentBranch_RealRepo creates a temp git repo with
// one commit and asserts CurrentBranch returns the branch
// name. The branch name on a fresh `git init` with no
// commits yet is empty (no HEAD) — we make a commit so a
// branch exists.
func TestCurrentBranch_RealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH; skipping")
	}

	dir := initTempGitRepo(t)
	branch := "feature/test-branch"

	// Create a branch with a commit on it (so HEAD resolves to
	// the new branch, not the default branch's tip).
	checkoutCmd(t, dir, "-b", branch)
	touchCommitCmd(t, dir)

	got, err := CurrentBranch(dir)
	if err != nil {
		t.Fatalf("CurrentBranch(%q) error: %v", dir, err)
	}
	if got != branch {
		t.Errorf("CurrentBranch = %q, want %q", got, branch)
	}
}

// TestCurrentBranch_NotARepo asserts a non-git directory
// returns ErrNotARepo with empty branch.
func TestCurrentBranch_NotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH; skipping")
	}
	dir := t.TempDir() // empty dir

	branch, err := CurrentBranch(dir)
	if !errors.Is(err, ErrNotARepo) {
		t.Errorf("err = %v, want ErrNotARepo", err)
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty", branch)
	}
}

// TestCurrentBranch_EmptyWorkdir asserts that an empty
// workdir doesn't blow up; CurrentBranch treats it as
// "unknown" rather than calling git.
func TestCurrentBranch_EmptyWorkdir(t *testing.T) {
	branch, err := CurrentBranch("")
	if err == nil {
		t.Errorf("err = nil, want non-nil for empty workdir")
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty", branch)
	}
	if errors.Is(err, ErrNotARepo) || errors.Is(err, ErrNoGit) {
		t.Errorf("err = %v, want errBranchUnknown (not NoGit / NotARepo)", err)
	}
}

// TestCurrentBranch_DetachedHead asserts the helper
// returns an empty string (no error) for a detached HEAD.
// Detached-HEAD reviews are a legitimate operator choice
// (CI may pin to a specific SHA); we don't want to mislead
// with a branch-mismatch warning.
func TestCurrentBranch_DetachedHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH; skipping")
	}
	dir := initTempGitRepo(t)
	touchCommitCmd(t, dir)

	// Detach HEAD.
	shaCmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := shaCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		t.Fatalf("no SHA from rev-parse HEAD")
	}
	checkoutCmd(t, dir, "--detach", sha)

	branch, err := CurrentBranch(dir)
	// git rev-parse --abbrev-ref HEAD on a detached HEAD
	// prints "HEAD". The helper returns empty string, no
	// error (the caller treats empty branch as "skip the
	// mismatch warning").
	if err != nil {
		t.Errorf("err = %v, want nil for detached HEAD", err)
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty for detached HEAD", branch)
	}
}

// TestCurrentBranch_NoGitInPath simulates a PATH without
// `git` available — e.g. the distroless CI image. The
// helper should return ErrNoGit, not crash, not call git
// at all (which would fail with a confusing "exec: file
// not found").
//
// We simulate this by setting PATH to a temp dir that
// intentionally has no git binary.
func TestCurrentBranch_NoGitInPath(t *testing.T) {
	dir := t.TempDir()

	// PATH-only env var PATH empty would let the test
	// process find its own inherited git; instead we
	// construct a wholly-empty PATH so exec.LookPath
	// definitively fails.
	origPath, hadPath := os.LookupEnv("PATH")
	t.Setenv("PATH", "")
	defer func() {
		if hadPath {
			os.Setenv("PATH", origPath)
		} else {
			os.Unsetenv("PATH")
		}
	}()

	branch, err := CurrentBranch(dir)
	if !errors.Is(err, ErrNoGit) {
		t.Errorf("err = %v, want ErrNoGit", err)
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty", branch)
	}
}

// TestIsWorkdirRepo covers both regular repos (.git is a
// directory) and worktree-style repos (.git is a file
// containing "gitdir: …"). Empty / non-existent workdirs
// are also negative-tested.
func TestIsWorkdirRepo(t *testing.T) {
	t.Run("regular repo (.git dir)", func(t *testing.T) {
		dir := initTempGitRepo(t)
		if !IsWorkdirRepo(dir) {
			t.Errorf("IsWorkdirRepo(%q) = false, want true (regular repo)", dir)
		}
	})

	t.Run("non-repo (empty dir)", func(t *testing.T) {
		dir := t.TempDir()
		if IsWorkdirRepo(dir) {
			t.Errorf("IsWorkdirRepo(%q) = true, want false", dir)
		}
	})

	t.Run("empty workdir", func(t *testing.T) {
		if IsWorkdirRepo("") {
			t.Errorf("IsWorkdirRepo(\"\") = true, want false")
		}
	})

	t.Run("non-dir workdir (file passed as workdir)", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/file.txt"
		if err := os.WriteFile(path, []byte("hi"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if IsWorkdirRepo(path) {
			t.Errorf("IsWorkdirRepo(%q) on a file path = true, want false", path)
		}
	})
}

// initTempGitRepo is a small helper. It runs `git init`
// inside t.TempDir() and returns the path. We don't bother
// with initial-branch configuration; the tests that need
// a specific branch create one with checkout -b.
func initTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	runCmd := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		// Suppress prompts in `git init` on some CI hosts
		// (e.g. template hooks asking for identity).
		env := append(os.Environ(),
			"GIT_AUTHOR_NAME=mreview-test",
			"GIT_AUTHOR_EMAIL=test@mreview.local",
			"GIT_COMMITTER_NAME=mreview-test",
			"GIT_COMMITTER_EMAIL=test@mreview.local",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}

	runCmd("init", "-q")
	// Sanity: .git should exist as a directory after init.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf(".git not present after init: %v", err)
	}
	return dir
}

// touchCommitCmd creates an empty commit in dir to seed the
// repo with one commit (so HEAD resolves — a fresh
// `git init` has no commits → rev-parse fails).
func touchCommitCmd(t *testing.T, dir string) {
	t.Helper()
	c := exec.Command("git", "-C", dir, "commit", "--allow-empty",
		"-m", "init", "-q")
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=mreview-test",
		"GIT_AUTHOR_EMAIL=test@mreview.local",
		"GIT_COMMITTER_NAME=mreview-test",
		"GIT_COMMITTER_EMAIL=test@mreview.local",
	)
	c.Env = env
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("empty commit: %s: %s", err, out)
	}
}

// checkoutCmd runs `git checkout …` in dir. Helper for tests
// that need to create branches or detach HEAD.
func checkoutCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir, "checkout"}, args...)...)
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=mreview-test",
		"GIT_AUTHOR_EMAIL=test@mreview.local",
		"GIT_COMMITTER_NAME=mreview-test",
		"GIT_COMMITTER_EMAIL=test@mreview.local",
	)
	c.Env = env
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("checkout %v: %s: %s", args, err, out)
	}
}
