package main

import (
	"context"
	"testing"

	"github.com/inful/mreview/internal/reviewer"
)

// fakeReviewer is the test double for reviewerInterface.
// Configured at construction with the canned result and error
// it should return from Run; tests inspect callCount /
// lastProject / lastIID to assert runReview used the seam
// correctly.
//
// Lives in a separate file from the production code so the
// //nolint:testpackage boundary is clear (this type would
// also need to be excluded from production binaries in a
// separate-compilation world, but Go's test build tags make
// _test.go files inert in the production build).
type fakeReviewer struct {
	// Canned return values. err wins over result when both
	// are set.
	result *reviewer.Result
	err    error

	// Call assertions.
	callCount   int
	lastCtx     context.Context
	lastProject string
	lastIID     int
	lastAction  string
}

// Run satisfies reviewerInterface. Records the call, then
// returns the canned values.
func (f *fakeReviewer) Run(ctx context.Context, project string, iid int, action ...string) (*reviewer.Result, error) {
	f.callCount++
	f.lastCtx = ctx
	f.lastProject = project
	f.lastIID = iid
	if len(action) > 0 {
		f.lastAction = action[0]
	}
	return f.result, f.err
}

// installFakeReviewer swaps reviewRunner for a function that
// returns the given fake, registers a t.Cleanup to restore
// the original, and returns the fake for the test to assert
// against.
//
// Callers should construct a fakeReviewer with the desired
// canned result/error, then call installFakeReviewer(t,
// &fake). When the test ends the original reviewRunner
// (buildReviewer) is restored.
func installFakeReviewer(t *testing.T, fake *fakeReviewer) {
	t.Helper()
	orig := reviewRunner
	reviewRunner = func(ctx context.Context, deps clientDeps) (reviewerInterface, error) {
		return fake, nil
	}
	t.Cleanup(func() {
		reviewRunner = orig
	})
}
