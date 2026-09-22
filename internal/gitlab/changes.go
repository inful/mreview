package gitlab

import (
	"context"
	"fmt"
	"io"
	"net/http"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// ChangeFile is a single file's worth of diff data from a merge
// request. It's the unit the LLM client chunks on; each ChangeFile
// becomes at most one chunk (or several, if its diff exceeds the
// per-file size cap).
type ChangeFile struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	DeletedFile bool   `json:"deleted_file"`
	RenamedFile bool   `json:"renamed_file"`
	Diff        string `json:"diff"` // raw unified diff (may be empty for binary)
}

// Path returns the path the file will have at HEAD after the MR is
// merged — i.e. the path used for inline discussion anchors. For new
// files this is NewPath; for deleted files the file doesn't exist
// at HEAD so the caller should skip inline commenting.
func (c ChangeFile) Path() string {
	if c.NewPath != "" {
		return c.NewPath
	}
	return c.OldPath
}

// FetchChanges fetches the per-file diff for a merge request.
//
// Uses the modern /diffs endpoint (the /changes endpoint is
// deprecated upstream). PerPage is 100 which covers the typical MR;
// larger MRs can paginate later.
//
// Diff is the raw unified diff text. Empty for binary files — the
// caller should detect that and skip those when building chunks for
// the LLM.
func (c *Client) FetchChanges(ctx context.Context, project string, iid int) ([]ChangeFile, error) {
	if err := validatePath(project); err != nil {
		return nil, err
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlab: merge request IID must be > 0, got %d", iid)
	}

	var result []ChangeFile
	op := "FetchChanges"
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		opts := &gl.ListMergeRequestDiffsOptions{
			ListOptions: gl.ListOptions{PerPage: 100},
		}
		diffs, resp, err := c.inner.MergeRequests.ListMergeRequestDiffs(project, int64(iid), opts)
		if err != nil {
			return classifyChangesError(op, c.baseURL, project, iid, resp, err)
		}
		result = make([]ChangeFile, 0, len(diffs))
		for _, d := range diffs {
			result = append(result, ChangeFile{
				OldPath:     d.OldPath,
				NewPath:     d.NewPath,
				NewFile:     d.NewFile,
				DeletedFile: d.DeletedFile,
				RenamedFile: d.RenamedFile,
				Diff:        d.Diff,
			})
		}
		c.logger.Debug("gitlab fetch ok",
			"op", op,
			"project", project,
			"iid", iid,
			"attempt", attempt,
			"files", len(result),
		)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// classifyChangesError is the changes-flavored sibling of
// classifyFetchError. Kept separate because the URL shape differs.
func classifyChangesError(op, baseURL, project string, iid int, resp *gl.Response, err error) error {
	method := http.MethodGet
	url := fmt.Sprintf("%s/projects/%s/merge_requests/%d/diffs", baseURL, project, iid)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	body := ""
	if resp != nil {
		body = readResponseBody(resp.Body)
	}
	return classifyAndWrap(method, url, status, body, err)
}

// readResponseBody drains and returns the upstream response body. We
// limit the read so a hostile / broken server can't OOM the process;
// GitLab error pages are typically <10 KiB.
//
// Takes an io.ReadCloser (the body of *gl.Response) so the function
// is unit-testable without dragging in the upstream client-go types.
func readResponseBody(body io.ReadCloser) string {
	if body == nil {
		return ""
	}
	defer func() { _ = body.Close() }()
	const bodyMax = 64 * 1024
	lr := io.LimitReader(body, bodyMax)
	b, _ := io.ReadAll(lr)
	_, _ = io.Copy(io.Discard, body) // drain the rest so the connection can be reused
	return string(b)
}
