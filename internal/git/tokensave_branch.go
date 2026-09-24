package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// EnsureBranchTracked runs `tokensave branch add <branch>
// -p <workdir>` so the given branch is added to tokensave's
// multi-branch tracker. Idempotent — tokensave treats
// re-tracking of an already-tracked branch as a no-op
// (incremental sync only).
//
// This is used by mreview's orchestrator right after
// FetchMR succeeds: knowing the MR's source branch is the
// signal tokensave needs to suppress the "branch X is not
// tracked — serving from Y" warning that would otherwise
// appear in EVERY tool response. ~150 chars of WARNING +
// tokensave_metrics noise × ~6 tool calls is ~900 tokens of
// pure headers eating into the model's context budget.
//
// The distroless image bundles tokensave (see
// cmd/mreview/Dockerfile comments), so the binary is
// available wherever mreview runs. We still degrade
// gracefully via a sentinel when it isn't, so a local-dev
// environment without tokensave doesn't break mreview —
// the helper just becomes a no-op.
//
// All other exec failures come through as wrapped errors.
//
// The call has a 30s ceiling — `tokensave branch add` does
// an incremental sync, which can take a while on a large
// repo but should not block the agent loop indefinitely. If
// it times out, tokensave may still finish indexing in the
// background (the serve subprocess shares its .tokensave DB
// on disk, so a slow pre-track doesn't lose data, only adds
// latency to the first `graph_branch=<X>` query).
func EnsureBranchTracked(workdir, branch string) error {
	if workdir == "" {
		return fmt.Errorf("git: %w", ErrBranchNoWorkdir)
	}
	if branch == "" {
		return ErrBranchInvalid
	}
	bin, err := exec.LookPath("tokensave")
	if err != nil {
		return ErrNoTokensave
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// tokensave's `branch add` accepts the project path via
	// `-p` (sibling of the long flags we saw for `serve`).
	// `branch` (the literal verb) goes first; the branch
	// name goes at the end.  We invoke CombinedOutput so
	// the helper carries stderr verbatim into the wrapped
	// error on failure — useful when the operator turns
	// on --debug-llm and wants to see why the pre-track
	// didn't take.
	cmd := exec.CommandContext(ctx, bin, "branch", "add",
		"-p", workdir, branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text == "" {
			return fmt.Errorf("git: tokensave branch add failed: %w", err)
		}
		// Surface the stderr; the caller debug-logs it.
		return fmt.Errorf("git: tokensave branch add failed: %w: %s", err, text)
	}
	return nil
}

// ErrNoTokensave is returned when the `tokensave` binary
// isn't on PATH. Callers should treat this as "the
// branch-tracking step is unavailable in this environment";
// debug-log and move on.
var ErrNoTokensave = errors.New("git: tokensave binary not found in PATH")

// ErrBranchNoWorkdir is returned when the caller passed an
// empty workdir.
var ErrBranchNoWorkdir = errors.New("git: empty workdir")

// ErrBranchInvalid is returned when the caller passed an
// empty branch name.
var ErrBranchInvalid = errors.New("git: empty branch name")
