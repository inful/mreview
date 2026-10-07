package reviewer

import (
	"context"

	"github.com/inful/mreview/internal/gitlab"
)

// gitlabClient is the subset of the GitLab client the
// orchestrator depends on. Defined as an interface so
// tests can substitute a fake without touching the real
// gl-go-backed *gitlab.Client.
//
// The production wiring passes the concrete *gitlab.Client
// (which satisfies this interface by accident of method
// set — the orchestrator only ever called the methods
// listed below on the concrete type). The interface is
// declared here (not in internal/gitlab) so the test
// fakes don't need to import the heavy gl-go dependency.
//
// Adding a method to this list: if you find yourself
// wanting to call a new *gitlab.Client method from
// Orchestrator.Run, add it here. The compile error on
// the fake will tell you which test to update.
type gitlabClient interface {
	FetchMR(ctx context.Context, project any, iid int) (*gitlab.MergeRequest, error)
	FetchChanges(ctx context.Context, project any, iid int) ([]gitlab.ChangeFile, error)
	ListDiscussions(ctx context.Context, project any, iid int) ([]gitlab.Discussion, error)
	PostSummary(ctx context.Context, project any, iid int, body string) (*gitlab.Note, error)
	EditSummary(ctx context.Context, project any, iid int, noteID int64, body string) (*gitlab.Note, error)
	PostDiscussion(ctx context.Context, project any, iid int, diffRefs gitlab.DiffRefs, body gitlab.InlineComment) (*gitlab.Discussion, error)
	ResolveDiscussion(ctx context.Context, project string, iid int, discussionID string) error
}

// Compile-time assertion that *gitlab.Client satisfies
// gitlabClient. If a new *gitlab.Client method gets added
// without being added to the interface, the orchestrator's
// production wiring will still work but a test that adds
// a fake may surface the gap. Keep this line.
var _ gitlabClient = (*gitlab.Client)(nil)
