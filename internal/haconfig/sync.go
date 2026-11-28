package haconfig

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/fsnotify/fsnotify"
	"github.com/google/go-github/v93/github"
)

// UIFiles are the files Home Assistant's UI editors write, which the
// repository receives from Home Assistant.
var UIFiles = []string{"automations.yaml", "scripts.yaml", "scenes.yaml"}

// Syncer carries the UI files' edits to the repository, as a pull request
// from one branch that it merges once its Checks passed.
type Syncer struct {
	// Dir is Home Assistant's configuration directory.
	Dir string
	// Branch is the branch the pull request goes into.
	Branch string
	// SyncBranch is the pull request's branch, which it moves by force.
	SyncBranch string
	GitHub     *GitHub
	Hass       *Hass
	// Debounce is how long the UI files stay unwritten before it syncs:
	// the UI saves in bursts.
	Debounce time.Duration
	// Interval is how often it looks at the pull request's Checks.
	Interval time.Duration
	// ChecksTimeout is how long it waits for the pull request's Checks
	// before giving up on it until the next edit.
	ChecksTimeout time.Duration

	// pending is the pull request waiting for its Checks.
	pending *pendingPull
}

type pendingPull struct {
	number int
	sha    string
	since  time.Time
}

// Run syncs until ctx is done: once as it starts, for edits made while it
// wasn't running, then after each burst of writes.
func (s *Syncer) Run(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	// The directory, not the files: Home Assistant replaces a file with a
	// rename, which a watch on the file would lose.
	if err := w.Add(s.Dir); err != nil {
		return err
	}
	push := time.NewTimer(0)
	defer push.Stop()
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-w.Events:
			if filepath.Dir(ev.Name) == filepath.Clean(s.Dir) && slices.Contains(UIFiles, filepath.Base(ev.Name)) {
				push.Reset(s.Debounce)
			}
		case err := <-w.Errors:
			clog.WarnContext(ctx, "watching", "error", err)
		case <-push.C:
			if err := s.Push(ctx); err != nil {
				clog.WarnContext(ctx, "syncing", "error", err)
				push.Reset(s.Interval)
			}
		case <-ticker.C:
			retry, err := s.Settle(ctx)
			if err != nil {
				clog.WarnContext(ctx, "syncing", "error", err)
			}
			if retry {
				push.Reset(s.Debounce)
			}
		}
	}
}

// Push compares the UI files with the branch's and, when they differ,
// commits them onto it in the sync branch and opens its pull request. When
// they're the same, it closes the pull request, if one is open.
func (s *Syncer) Push(ctx context.Context) error {
	g := s.GitHub
	base, _, err := g.Head(ctx, s.Branch, "")
	if err != nil {
		return err
	}
	commit, _, err := g.Git.GetCommit(ctx, g.Owner, g.Repo, base)
	if err != nil {
		return err
	}
	tree, _, err := g.Git.GetTree(ctx, g.Owner, g.Repo, commit.GetTree().GetSHA(), false)
	if err != nil {
		return err
	}
	changes, err := s.changes(tree.Entries)
	if err != nil {
		return err
	}
	pulls, _, err := g.PullRequests.List(ctx, g.Owner, g.Repo, &github.PullRequestListOptions{
		State: "open",
		Head:  g.Owner + ":" + s.SyncBranch,
		Base:  s.Branch,
	})
	if err != nil {
		return err
	}
	var pull *github.PullRequest
	if len(pulls) > 0 {
		pull = pulls[0]
	}
	if len(changes) == 0 {
		s.pending = nil
		if pull == nil {
			return nil
		}
		clog.InfoContext(ctx, "closing the pull request, the UI files are the branch's", "pull", pull.GetNumber())
		_, _, err := g.PullRequests.Edit(ctx, g.Owner, g.Repo, pull.GetNumber(), &github.PullRequest{State: new("closed")})
		return err
	}

	newTree, _, err := g.Git.CreateTree(ctx, g.Owner, g.Repo, tree.GetSHA(), changes)
	if err != nil {
		return err
	}
	head := ""
	if pull != nil {
		// A tree is its content's hash: the pull request already holds
		// these files on this branch, as after a restart.
		c, _, err := g.Git.GetCommit(ctx, g.Owner, g.Repo, pull.GetHead().GetSHA())
		if err == nil && c.GetTree().GetSHA() == newTree.GetSHA() {
			head = c.GetSHA()
		}
	}
	if head == "" {
		var paths []string
		for _, c := range changes {
			paths = append(paths, c.GetPath())
		}
		created, _, err := g.Git.CreateCommit(ctx, g.Owner, g.Repo, github.Commit{
			Message: new("Sync the UI's edits from Home Assistant\n\n" + strings.Join(paths, "\n") + "\n"),
			Tree:    &github.Tree{SHA: newTree.SHA},
			Parents: []*github.Commit{{SHA: &base}},
		}, nil)
		if err != nil {
			return err
		}
		head = created.GetSHA()
		if err := g.SetBranch(ctx, s.SyncBranch, head); err != nil {
			return err
		}
	}
	if pull == nil {
		pull, _, err = g.PullRequests.Create(ctx, g.Owner, g.Repo, github.CreatePullRequest{
			Title: new("Sync the UI's edits from Home Assistant"),
			Head:  s.SyncBranch,
			Base:  s.Branch,
			Body:  new("Home Assistant's UI edited these files. ha-config-sync merges this pull request once its Checks pass, and replaces its commit with each new edit."),
		})
		if err != nil {
			return err
		}
	}
	if s.pending == nil || s.pending.sha != head {
		s.pending = &pendingPull{number: pull.GetNumber(), sha: head, since: time.Now()}
	}
	clog.InfoContext(ctx, "pushed the UI's edits", "pull", pull.GetNumber(), "sha", head)
	return nil
}

// Settle merges the pending pull request once its Checks passed, or gives up
// on it when they failed or took too long. It reports retry when the merge
// failed, which pushing again, onto the branch as it is now, can fix.
func (s *Syncer) Settle(ctx context.Context) (retry bool, err error) {
	p := s.pending
	if p == nil {
		return false, nil
	}
	checks, err := s.GitHub.Checks(ctx, p.sha)
	if err != nil {
		return false, err
	}
	switch checks {
	case ChecksPending:
		if time.Since(p.since) < s.ChecksTimeout {
			return false, nil
		}
		s.pending = nil
		return false, s.failed(ctx, p.sha, fmt.Sprintf("No Checks passed on pull request #%d within %s.", p.number, s.ChecksTimeout))
	case ChecksFailed:
		s.pending = nil
		return false, s.failed(ctx, p.sha, fmt.Sprintf("Checks failed on pull request #%d.", p.number))
	}
	s.pending = nil
	g := s.GitHub
	_, _, err = g.PullRequests.Merge(ctx, g.Owner, g.Repo, p.number, "", &github.PullRequestOptions{SHA: p.sha, MergeMethod: "squash"})
	if err != nil {
		return true, errors.Join(err, s.failed(ctx, p.sha, fmt.Sprintf("Merging pull request #%d failed: %v", p.number, err)))
	}
	clog.InfoContext(ctx, "merged the UI's edits", "pull", p.number, "sha", p.sha)
	if err := s.Hass.Succeeded(ctx, KindSync, p.sha); err != nil {
		clog.WarnContext(ctx, "notifying", "error", err)
	}
	return false, nil
}

func (s *Syncer) failed(ctx context.Context, sha, message string) error {
	clog.WarnContext(ctx, "sync failed", "sha", sha, "reason", message)
	return s.Hass.Failed(ctx, KindSync, sha, message)
}

// changes returns the UI files that differ from entries, the branch's top
// directory.
func (s *Syncer) changes(entries []*github.TreeEntry) ([]*github.TreeEntry, error) {
	remote := map[string]string{}
	for _, e := range entries {
		if e.GetType() == "blob" {
			remote[e.GetPath()] = e.GetSHA()
		}
	}
	var changes []*github.TreeEntry
	for _, name := range UIFiles {
		data, err := os.ReadFile(filepath.Join(s.Dir, name))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if _, ok := remote[name]; ok {
				// No sha and no content: GitHub removes the file.
				changes = append(changes, &github.TreeEntry{Path: new(name), Mode: new("100644"), Type: new("blob")})
			}
		case err != nil:
			return nil, err
		case blobSHA(data) != remote[name]:
			changes = append(changes, &github.TreeEntry{Path: new(name), Mode: new("100644"), Type: new("blob"), Content: new(string(data))})
		}
	}
	return changes, nil
}

// blobSHA is data's git hash as a blob.
func blobSHA(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
