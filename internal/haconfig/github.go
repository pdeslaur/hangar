package haconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// TokenSource returns GitHub tokens, like OctoSTS.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// GitHub is the REST API of one repository, for what ha-config-sync needs.
type GitHub struct {
	// API is the REST API's URL, like https://api.github.com.
	API string
	// Owner and Repo name the repository.
	Owner, Repo string
	Tokens      TokenSource
	HTTP        *http.Client
}

// ErrNotFound is a 404.
var ErrNotFound = errors.New("not found")

// statusError is a response other than the one expected.
type statusError struct {
	method, path string
	status       int
	body         []byte
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GitHub %s %s: %d: %s", e.method, e.path, e.status, bytes.TrimSpace(e.body))
}

func (e *statusError) Is(target error) bool {
	return target == ErrNotFound && e.status == http.StatusNotFound
}

// Head returns the commit branch points at. With etag, the ETag of an
// earlier answer, it reports changed false when the branch hasn't moved,
// which costs no rate limit.
func (g *GitHub) Head(ctx context.Context, branch, etag string) (sha, newETag string, changed bool, err error) {
	req, err := g.request(ctx, http.MethodGet, g.repoPath("commits", branch), nil)
	if err != nil {
		return "", "", false, err
	}
	// Answers the SHA alone, as text.
	req.Header.Set("Accept", "application/vnd.github.sha")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return "", "", false, err
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		return "", etag, false, nil
	case http.StatusOK:
		return strings.TrimSpace(string(b)), resp.Header.Get("ETag"), true, nil
	}
	return "", "", false, &statusError{req.Method, req.URL.Path, resp.StatusCode, b}
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
	var resp struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			App        struct {
				Slug string `json:"slug"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	path := g.repoPath("commits", sha, "check-runs") + "?per_page=100"
	if err := g.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return ChecksPending, err
	}
	state := ChecksPending
	for _, run := range resp.CheckRuns {
		if run.App.Slug != checksApp || !strings.HasPrefix(run.Name, checksPrefix) {
			continue
		}
		switch {
		case run.Status != "completed":
			return ChecksPending, nil
		case run.Conclusion != "success":
			return ChecksFailed, nil
		}
		state = ChecksPassed
	}
	return state, nil
}

// Tarball returns sha's tarball, a gzipped tar whose entries are all under
// one top directory.
func (g *GitHub) Tarball(ctx context.Context, sha string) (io.ReadCloser, error) {
	req, err := g.request(ctx, http.MethodGet, g.repoPath("tarball", sha), nil)
	if err != nil {
		return nil, err
	}
	// GitHub redirects to codeload with a token of its own in the URL; the
	// client drops Authorization on the way, as the redirect changes host.
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &statusError{req.Method, req.URL.Path, resp.StatusCode, b}
	}
	return resp.Body, nil
}

// TreeEntry is a file of a commit's top directory.
type TreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	// SHA is the blob's git hash.
	SHA string `json:"sha"`
}

// Tree returns the tree commit points at, and the entries of its top
// directory.
func (g *GitHub) Tree(ctx context.Context, commit string) (tree string, entries []TreeEntry, err error) {
	var c struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath("git", "commits", commit), nil, &c); err != nil {
		return "", nil, err
	}
	var t struct {
		Tree []TreeEntry `json:"tree"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath("git", "trees", c.Tree.SHA), nil, &t); err != nil {
		return "", nil, err
	}
	return c.Tree.SHA, t.Tree, nil
}

// FileChange is a file to write, or with Delete, to remove, in a new tree.
type FileChange struct {
	Path    string
	Content string
	Delete  bool
}

// CreateTree creates a tree of base with changes, and returns its hash.
func (g *GitHub) CreateTree(ctx context.Context, base string, changes []FileChange) (string, error) {
	var entries []map[string]any
	for _, c := range changes {
		e := map[string]any{"path": c.Path, "mode": "100644", "type": "blob"}
		if c.Delete {
			// A null sha, without content, removes the path.
			e["sha"] = nil
		} else {
			e["content"] = c.Content
		}
		entries = append(entries, e)
	}
	var resp struct {
		SHA string `json:"sha"`
	}
	err := g.do(ctx, http.MethodPost, g.repoPath("git", "trees"), map[string]any{
		"base_tree": base,
		"tree":      entries,
	}, &resp)
	return resp.SHA, err
}

// CreateCommit creates a commit of tree on parent, and returns its hash.
func (g *GitHub) CreateCommit(ctx context.Context, message, tree, parent string) (string, error) {
	var resp struct {
		SHA string `json:"sha"`
	}
	err := g.do(ctx, http.MethodPost, g.repoPath("git", "commits"), map[string]any{
		"message": message,
		"tree":    tree,
		"parents": []string{parent},
	}, &resp)
	return resp.SHA, err
}

// SetBranch points branch at sha, creating it or moving it by force.
func (g *GitHub) SetBranch(ctx context.Context, branch, sha string) error {
	err := g.do(ctx, http.MethodPatch, g.repoPath("git", "refs", "heads", branch), map[string]any{
		"sha":   sha,
		"force": true,
	}, nil)
	var se *statusError
	// GitHub answers 422 "Reference does not exist" for a missing branch.
	if !errors.As(err, &se) || (se.status != http.StatusUnprocessableEntity && se.status != http.StatusNotFound) {
		return err
	}
	return g.do(ctx, http.MethodPost, g.repoPath("git", "refs"), map[string]any{
		"ref": "refs/heads/" + branch,
		"sha": sha,
	}, nil)
}

// Pull is an open pull request.
type Pull struct {
	Number int `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// OpenPull returns the open pull request of head into base, if there is
// one; ok is false when there's none.
func (g *GitHub) OpenPull(ctx context.Context, head, base string) (pull Pull, ok bool, err error) {
	q := url.Values{"state": {"open"}, "head": {g.Owner + ":" + head}, "base": {base}}
	var pulls []Pull
	if err := g.do(ctx, http.MethodGet, g.repoPath("pulls")+"?"+q.Encode(), nil, &pulls); err != nil {
		return Pull{}, false, err
	}
	if len(pulls) == 0 {
		return Pull{}, false, nil
	}
	return pulls[0], true, nil
}

// CreatePull opens a pull request of head into base, and returns its number.
func (g *GitHub) CreatePull(ctx context.Context, head, base, title, body string) (int, error) {
	var pull Pull
	err := g.do(ctx, http.MethodPost, g.repoPath("pulls"), map[string]any{
		"head":  head,
		"base":  base,
		"title": title,
		"body":  body,
	}, &pull)
	return pull.Number, err
}

// ClosePull closes a pull request without merging it.
func (g *GitHub) ClosePull(ctx context.Context, number int) error {
	return g.do(ctx, http.MethodPatch, g.repoPath("pulls", fmt.Sprint(number)), map[string]any{"state": "closed"}, nil)
}

// MergePull squash-merges a pull request, only while its head is sha.
func (g *GitHub) MergePull(ctx context.Context, number int, sha string) error {
	return g.do(ctx, http.MethodPut, g.repoPath("pulls", fmt.Sprint(number), "merge"), map[string]any{
		"merge_method": "squash",
		"sha":          sha,
	}, nil)
}

func (g *GitHub) repoPath(elem ...string) string {
	parts := []string{"repos", g.Owner, g.Repo}
	for _, e := range elem {
		parts = append(parts, url.PathEscape(e))
	}
	return "/" + strings.Join(parts, "/")
}

func (g *GitHub) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	token, err := g.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, g.API+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

// maxJSON bounds the JSON responses it reads.
const maxJSON = 4 << 20

func (g *GitHub) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := g.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxJSON))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &statusError{method, req.URL.Path, resp.StatusCode, b}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
