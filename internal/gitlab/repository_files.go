package gitlab

import (
	"context"
	"fmt"
	"net/http"

	gl "gitlab.com/gitlab-org/api/client-go"
)

// TreeNode is the subset of *gitlab.TreeNode the reviewer needs.
// GitLab returns these from the repository tree API (a flat or
// recursive listing of files / directories in the project).
type TreeNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Mode string `json:"mode"`
}

// ListRepositoryTree returns the repository tree rooted at
// path, scoped to ref. Used by the skills loader to discover
// the skill files in a project's skills/ directory.
//
// path is the directory within the repository (e.g. "skills").
// Pass "" to list the repo root.
//
// ref is the branch / tag / SHA to read from. Pass "" to use
// the project's default branch.
//
// Each TreeNode.Type is "blob" (file), "tree" (directory),
// or "commit" (a submodule). The skills loader filters to blobs
// and only those whose Name ends in ".md".
//
// Pagination: we request per_page=100 which is well above any
// realistic skills directory size. For projects with more
// entries, callers can iterate via the returned slice + opt.Page
// (future enhancement).
func (c *Client) ListRepositoryTree(ctx context.Context, project, path, ref string) ([]TreeNode, error) {
	if err := validatePath(project); err != nil {
		return nil, err
	}

	var result []TreeNode
	op := "ListRepositoryTree"
	url := fmt.Sprintf("%s/projects/%s/repository/tree", c.baseURL, project)
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		opts := &gl.ListTreeOptions{
			ListOptions: gl.ListOptions{PerPage: 100},
		}
		if path != "" {
			p := path
			opts.Path = &p
		}
		if ref != "" {
			r := ref
			opts.Ref = &r
		}
		nodes, resp, err := c.inner.Repositories.ListTree(project, opts)
		if err != nil {
			return c.classify(http.MethodGet, url, resp, err)
		}
		result = make([]TreeNode, 0, len(nodes))
		for _, n := range nodes {
			result = append(result, TreeNode{
				ID:   n.ID,
				Name: n.Name,
				Path: n.Path,
				Type: n.Type,
				Mode: n.Mode,
			})
		}
		c.logger.Debug("gitlab tree ok",
			"op", op,
			"project", project,
			"path", path,
			"ref", ref,
			"attempt", attempt,
			"nodes", len(result),
		)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetRepositoryFileRaw returns the raw bytes of a single file
// from the project's repository at ref. Used by the skills
// loader to fetch the body of each skill file discovered via
// ListRepositoryTree.
//
// path is the full file path within the repository (e.g.
// "skills/go-review.md").
//
// ref is the branch / tag / SHA — same semantics as
// ListRepositoryTree.
func (c *Client) GetRepositoryFileRaw(ctx context.Context, project, path, ref string) ([]byte, error) {
	if err := validatePath(project); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("gitlab: file path is required")
	}

	var result []byte
	op := "GetRepositoryFileRaw"
	url := fmt.Sprintf("%s/projects/%s/repository/files/%s/raw",
		c.baseURL, project, path)
	err := doWithRetry(ctx, c.retry, op, func(ctx context.Context, attempt int) error {
		opts := &gl.GetRawFileOptions{}
		if ref != "" {
			r := ref
			opts.Ref = &r
		}
		data, resp, err := c.inner.RepositoryFiles.GetRawFile(project, path, opts)
		if err != nil {
			return c.classify(http.MethodGet, url, resp, err)
		}
		result = data
		c.logger.Debug("gitlab file ok",
			"op", op,
			"project", project,
			"path", path,
			"ref", ref,
			"attempt", attempt,
			"bytes", len(data),
		)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
