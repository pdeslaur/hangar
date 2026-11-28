package haconfig

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-github/v93/github"
)

// GitHub is the configuration's repository, through go-github. Its methods
// add what ha-config-sync needs beyond go-github's.
type GitHub struct {
	*github.Client
	Owner, Repo string
}

// Head returns the commit branch points at. With lastSHA, the commit it
// answered before, it reports changed false while the branch hasn't moved,
// which GitHub answers with a 304 that costs no rate limit.
func (g *GitHub) Head(ctx context.Context, branch, lastSHA string) (sha string, changed bool, err error) {
	sha, resp, err := g.Repositories.GetCommitSHA1(ctx, g.Owner, g.Repo, branch, lastSHA)
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return lastSHA, false, nil
	}
	if err != nil {
		return "", false, err
	}
	return sha, sha != lastSHA, nil
}

// ChecksState is the outcome of a commit's Chainguard Checks.
type ChecksState int

const (
	// ChecksPending is no Checks yet, or Checks still running.
	ChecksPending ChecksState = iota
	ChecksPassed
	ChecksFailed
)

func (s ChecksState) String() string {
	return [...]string{"pending", "passed", "failed"}[s]
}

// The Chainguard Checks summary: one check run per commit, named by its
// trigger, like "Chainguard Checks (presubmit)" on a pull request's commits
// and "Chainguard Checks (postsubmit)" on the default branch's.
const (
	checksApp    = "chainguard"
	checksPrefix = "Chainguard Checks"
)

// Checks returns the state of sha's Chainguard Checks: passed only when
// every summary check run succeeded.
func (g *GitHub) Checks(ctx context.Context, sha string) (ChecksState, error) {
	runs, _, err := g.Client.Checks.ListCheckRunsForRef(ctx, g.Owner, g.Repo, sha, &github.ListCheckRunsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	})
	if err != nil {
		return ChecksPending, err
	}
	state := ChecksPending
	for _, run := range runs.CheckRuns {
		if run.GetApp().GetSlug() != checksApp || !strings.HasPrefix(run.GetName(), checksPrefix) {
			continue
		}
		switch {
		case run.GetStatus() != "completed":
			return ChecksPending, nil
		case run.GetConclusion() != "success":
			return ChecksFailed, nil
		}
		state = ChecksPassed
	}
	return state, nil
}

// Tarball returns sha's tarball, a gzipped tar whose entries are all under
// one top directory.
func (g *GitHub) Tarball(ctx context.Context, sha string) (io.ReadCloser, error) {
	// A link to codeload, holding a token of its own.
	link, _, err := g.Repositories.GetArchiveLink(ctx, g.Owner, g.Repo, github.Tarball, &github.RepositoryContentGetOptions{Ref: sha}, 0)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.Client.Client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("downloading %s's tarball: %s", sha, resp.Status)
	}
	return resp.Body, nil
}

// SetBranch points branch at sha, creating it or moving it by force.
func (g *GitHub) SetBranch(ctx context.Context, branch, sha string) error {
	ref := "refs/heads/" + branch
	_, resp, err := g.Git.UpdateRef(ctx, g.Owner, g.Repo, ref, github.UpdateRef{SHA: sha, Force: new(true)})
	// GitHub answers 422 "Reference does not exist" for a missing branch.
	if resp == nil || (resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusNotFound) {
		return err
	}
	_, _, err = g.Git.CreateRef(ctx, g.Owner, g.Repo, github.CreateRef{Ref: ref, SHA: sha})
	return err
}

// Root returns the tree commit points at, and the git hashes of the files at
// its top, by path.
func (g *GitHub) Root(ctx context.Context, commit string) (tree string, blobs map[string]string, err error) {
	c, _, err := g.Git.GetCommit(ctx, g.Owner, g.Repo, commit)
	if err != nil {
		return "", nil, err
	}
	t, _, err := g.Git.GetTree(ctx, g.Owner, g.Repo, c.GetTree().GetSHA(), false)
	if err != nil {
		return "", nil, err
	}
	blobs = map[string]string{}
	for _, e := range t.Entries {
		if e.GetType() == "blob" {
			blobs[e.GetPath()] = e.GetSHA()
		}
	}
	return t.GetSHA(), blobs, nil
}
