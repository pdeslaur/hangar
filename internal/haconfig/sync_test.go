package haconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newSyncer(t *testing.T) (*Syncer, *fakeGitHub, *fakeHass) {
	t.Helper()
	dir := t.TempDir()
	gh, client := newFakeGitHub(t)
	fh, hass := newFakeHass(t, dir)
	return &Syncer{
		Dir:           dir,
		Branch:        "main",
		SyncBranch:    "ha-ui-sync",
		GitHub:        client,
		Hass:          hass,
		Debounce:      10 * time.Millisecond,
		Interval:      10 * time.Millisecond,
		ChecksTimeout: time.Hour,
	}, gh, fh
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncMergesUIEditsOnceChecksPass(t *testing.T) {
	s, gh, fh := newSyncer(t)
	ctx := context.Background()
	gh.commit("main", map[string]string{
		"automations.yaml":     "old\n",
		"scripts.yaml":         "{}\n",
		"automations/a.yaml":   "code\n",
		".github/whatever.yml": "x",
	})
	writeFile(t, filepath.Join(s.Dir, "automations.yaml"), "new\n")
	writeFile(t, filepath.Join(s.Dir, "scripts.yaml"), "{}\n")
	writeFile(t, filepath.Join(s.Dir, "scenes.yaml"), "[]\n")

	if err := s.Push(ctx); err != nil {
		t.Fatal(err)
	}
	pulls := gh.openPulls()
	if len(pulls) != 1 {
		t.Fatalf("%d open pull requests, want 1", len(pulls))
	}
	files := gh.files("ha-ui-sync")
	if files["automations.yaml"] != "new\n" || files["scenes.yaml"] != "[]\n" || files["scripts.yaml"] != "{}\n" {
		t.Errorf("sync branch's UI files = %q %q %q", files["automations.yaml"], files["scripts.yaml"], files["scenes.yaml"])
	}
	if files["automations/a.yaml"] != "code\n" {
		t.Error("sync branch lost the repository's own files")
	}

	// Pushing again, with nothing new, keeps the commit and its Checks.
	head := gh.files("ha-ui-sync")
	sha := s.pending.sha
	if err := s.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if s.pending.sha != sha || len(gh.openPulls()) != 1 || len(gh.files("ha-ui-sync")) != len(head) {
		t.Error("pushed a new commit without a new edit")
	}

	if retry, err := s.Settle(ctx); err != nil || retry {
		t.Fatalf("settle without Checks: retry %v, %v", retry, err)
	}
	if s.pending == nil {
		t.Fatal("gave up on the pull request while its Checks hadn't run")
	}

	gh.setChecks(sha, "completed", "success")
	if retry, err := s.Settle(ctx); err != nil || retry {
		t.Fatalf("settle: retry %v, %v", retry, err)
	}
	if !pulls[0].merged {
		t.Error("pull request not merged")
	}
	if got := gh.files("main")["automations.yaml"]; got != "new\n" {
		t.Errorf("main's automations.yaml = %q, want the UI's", got)
	}
	if calls := fh.calls(); len(calls.succeed) != 1 || calls.succeed[0] != "sync "+sha {
		t.Errorf("succeeded = %q", calls.succeed)
	}

	// Main now holds the UI's files: nothing to push.
	if err := s.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if len(gh.openPulls()) != 0 || s.pending != nil {
		t.Error("pushed again after the merge")
	}
}

func TestSyncClosesPullWhenUIMatchesBranch(t *testing.T) {
	s, gh, _ := newSyncer(t)
	ctx := context.Background()
	gh.commit("main", map[string]string{"automations.yaml": "old\n"})
	writeFile(t, filepath.Join(s.Dir, "automations.yaml"), "new\n")
	if err := s.Push(ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(s.Dir, "automations.yaml"), "old\n")
	if err := s.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if len(gh.openPulls()) != 0 || s.pending != nil {
		t.Error("left the pull request open after the edit was undone")
	}
}

func TestSyncDeletesRemovedUIFile(t *testing.T) {
	s, gh, _ := newSyncer(t)
	ctx := context.Background()
	gh.commit("main", map[string]string{"automations.yaml": "a\n", "scenes.yaml": "s\n"})
	writeFile(t, filepath.Join(s.Dir, "automations.yaml"), "a\n")
	if err := s.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := gh.files("ha-ui-sync")["scenes.yaml"]; ok {
		t.Error("scenes.yaml, gone from Home Assistant, still on the sync branch")
	}
}

func TestSyncReportsFailedChecks(t *testing.T) {
	s, gh, fh := newSyncer(t)
	ctx := context.Background()
	gh.commit("main", map[string]string{})
	writeFile(t, filepath.Join(s.Dir, "automations.yaml"), "broken\n")
	if err := s.Push(ctx); err != nil {
		t.Fatal(err)
	}
	gh.setChecks(s.pending.sha, "completed", "failure")
	if _, err := s.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := fh.calls(); len(calls.failed) != 1 || !strings.HasPrefix(calls.failed[0], "sync: Checks failed on pull request #1") {
		t.Errorf("failed = %q", calls.failed)
	}
	if s.pending != nil {
		t.Error("still waiting on a pull request whose Checks failed")
	}
	if len(gh.openPulls()) != 1 {
		t.Error("closed the pull request whose Checks failed, which shows why")
	}
}

func TestSyncRetriesFailedMerge(t *testing.T) {
	s, gh, fh := newSyncer(t)
	ctx := context.Background()
	gh.commit("main", map[string]string{})
	writeFile(t, filepath.Join(s.Dir, "automations.yaml"), "a\n")
	if err := s.Push(ctx); err != nil {
		t.Fatal(err)
	}
	stale := s.pending.sha
	gh.setChecks(stale, "completed", "success")
	// Something else moved the branch since: the merge is refused.
	gh.commit("ha-ui-sync", map[string]string{"automations.yaml": "other\n"})
	retry, err := s.Settle(ctx)
	if err == nil || !retry {
		t.Fatalf("settle: retry %v, %v; want a retry and an error", retry, err)
	}
	if calls := fh.calls(); len(calls.failed) != 1 || !strings.Contains(calls.failed[0], "Merging pull request #1 failed") {
		t.Errorf("failed = %q", calls.failed)
	}
	if err := s.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if s.pending == nil || s.pending.sha == stale {
		t.Error("pushing again didn't replace the refused commit")
	}
}

func TestSyncRunPushesAfterEdits(t *testing.T) {
	s, gh, _ := newSyncer(t)
	gh.commit("main", map[string]string{})
	writeFile(t, filepath.Join(s.Dir, "automations.yaml"), "before\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	waitFor := func(content string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for gh.files("ha-ui-sync")["automations.yaml"] != content {
			if time.Now().After(deadline) {
				t.Fatalf("%q never reached the sync branch", content)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// The edit made while it wasn't running.
	waitFor("before\n")

	// As Home Assistant writes: a temporary file renamed over the file.
	tmp := filepath.Join(s.Dir, "automations.yaml.tmp")
	writeFile(t, tmp, "- id: x\n")
	if err := os.Rename(tmp, filepath.Join(s.Dir, "automations.yaml")); err != nil {
		t.Fatal(err)
	}
	waitFor("- id: x\n")
}
