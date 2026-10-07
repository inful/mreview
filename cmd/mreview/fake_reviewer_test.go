package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/inful/mreview/internal/reviewer"
)

// fakeReviewer is the test double for reviewerInterface.
// Configured at construction with the canned result and error
// it should return from Run; tests inspect callCount /
// lastProject / lastIID to assert runReview used the seam
// correctly.
//
// lastProject is stored as a string (via fmt.Sprint) for
// convenience: most tests want to assert on the value
// passed in, which is either a string path or a numeric ID
// formatted to its string representation. Tests that care
// about the int-vs-string distinction can read the
// lastProjectKind field.
type fakeReviewer struct {
	// Canned return values. err wins over result when both
	// are set.
	result *reviewer.Result
	err    error

	// Call assertions.
	callCount       int
	lastCtx         context.Context
	lastProject     string // string form of project, for assertions
	lastProjectKind string // "string" or "int", for type-distinction assertions
	lastIID         int
	lastAction      string
}

// Run satisfies reviewerInterface. Records the call, then
// returns the canned values.
func (f *fakeReviewer) Run(ctx context.Context, project any, iid int, action ...string) (*reviewer.Result, error) {
	f.callCount++
	f.lastCtx = ctx
	switch v := project.(type) {
	case string:
		f.lastProject = v
		f.lastProjectKind = "string"
	case int:
		f.lastProject = fmt.Sprintf("%d", v)
		f.lastProjectKind = "int"
	default:
		f.lastProject = fmt.Sprintf("%v", v)
		f.lastProjectKind = "other"
	}
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
