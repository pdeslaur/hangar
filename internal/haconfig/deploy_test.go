package haconfig

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func newDeployer(t *testing.T) (*Deployer, *fakeGitHub, *fakeHass) {
	t.Helper()
	dir := t.TempDir()
	gh, client := newFakeGitHub(t)
	fh, hass := newFakeHass(t, dir)
	d := &Deployer{
		Dir:           dir,
		Branch:        "main",
		GitHub:        client,
		Hass:          hass,
		Interval:      time.Second,
		ChecksTimeout: time.Hour,
	}
	if err := d.load(); err != nil {
		t.Fatal(err)
	}
	return d, gh, fh
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDeployAppliesCommitOnceChecksPass(t *testing.T) {
	d, gh, fh := newDeployer(t)
	ctx := context.Background()
	sha := gh.commit("main", map[string]string{
		"README.md":              "readme",
		"automations/porch.yaml": "- id: porch\n",
		"automations.yaml":       "ui",
	})

	// No Checks yet: nothing happens.
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.Dir, "automations")); err == nil {
		t.Fatal("deployed before its Checks ran")
	}

	gh.setChecks(sha, "in_progress", "")
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fh.calls().checked) != 0 {
		t.Fatal("deployed while its Checks ran")
	}

	gh.setChecks(sha, "completed", "success")
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(d.Dir, "automations", "porch.yaml")); got != "- id: porch\n" {
		t.Errorf("automations/porch.yaml = %q", got)
	}
	// Only the owned directories: the UI's file and the rest stay out.
	for _, name := range []string{"README.md", "automations.yaml"} {
		if _, err := os.Stat(filepath.Join(d.Dir, name)); err == nil {
			t.Errorf("%s deployed", name)
		}
	}
	calls := fh.calls()
	if len(calls.checked) != 1 || string(calls.checked[0][filepath.Join("automations", "porch.yaml")]) != "- id: porch\n" {
		t.Errorf("checked %v, want one check of the new files", calls.checked)
	}
	if want := [][]string{{ReloadAutomation}}; !slices.EqualFunc(calls.reloads, want, slices.Equal) {
		t.Errorf("reloads = %v, want %v", calls.reloads, want)
	}
	if want := []string{"deploy " + sha}; !slices.Equal(calls.succeed, want) {
		t.Errorf("succeeded = %v, want %v", calls.succeed, want)
	}

	// Applied: the next polls, answered 304, do nothing.
	before := gh.headRequests
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if gh.headRequests != before+1 || len(fh.calls().checked) != 1 {
		t.Error("polled again after applying")
	}

	// It remembers across restarts.
	d2 := &Deployer{Dir: d.Dir, Branch: "main", GitHub: d.GitHub, Hass: d.Hass, ChecksTimeout: time.Hour}
	if err := d2.load(); err != nil {
		t.Fatal(err)
	}
	if err := d2.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fh.calls().checked) != 1 {
		t.Error("applied again after a restart")
	}
}

func TestDeployReloadsWhatChanged(t *testing.T) {
	d, gh, fh := newDeployer(t)
	ctx := context.Background()
	apply := func(files map[string]string) {
		t.Helper()
		sha := gh.commit("main", files)
		gh.setChecks(sha, "completed", "success")
		if err := d.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}

	apply(map[string]string{"scripts/bed.yaml": "bed:\n", "scenes/movie.yaml": "- name: movie\n"})
	apply(map[string]string{"scripts/bed.yaml": "bed: {}\n", "scenes/movie.yaml": "- name: movie\n"})
	apply(map[string]string{"scripts/bed.yaml": "bed: {}\n", "scenes/movie.yaml": "- name: movie\n", "README.md": "only this"})
	apply(map[string]string{"scripts/bed.yaml": "bed: {}\n", "packages/lights.yaml": "light: []\n"})

	want := [][]string{
		{ReloadScene, ReloadScript},
		{ReloadScript},
		// The README changed nothing deployed.
		// A package can hold anything: everything, once. The scene's
		// removal rides along.
		{ReloadAll},
	}
	if got := fh.calls().reloads; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("reloads = %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(d.Dir, "scenes", "movie.yaml")); err == nil {
		t.Error("scenes/movie.yaml, removed from the repository, still deployed")
	}
}

func TestDeployRollsBackRefusedConfig(t *testing.T) {
	d, gh, fh := newDeployer(t)
	ctx := context.Background()
	good := gh.commit("main", map[string]string{"automations/a.yaml": "good\n"})
	gh.setChecks(good, "completed", "success")
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}

	bad := gh.commit("main", map[string]string{"automations/a.yaml": "bad\n", "automations/b.yaml": "new\n"})
	gh.setChecks(bad, "completed", "success")
	fh.setErrors("Invalid config for 'automation'")
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}

	calls := fh.calls()
	if got := string(calls.checked[1][filepath.Join("automations", "a.yaml")]); got != "bad\n" {
		t.Errorf("checked a.yaml = %q, want the refused commit's", got)
	}
	if got := readFile(t, filepath.Join(d.Dir, "automations", "a.yaml")); got != "good\n" {
		t.Errorf("a.yaml = %q after the rollback, want good", got)
	}
	if _, err := os.Stat(filepath.Join(d.Dir, "automations", "b.yaml")); err == nil {
		t.Error("b.yaml left after the rollback")
	}
	if len(calls.reloads) != 1 {
		t.Errorf("reloads = %v, want only the good commit's", calls.reloads)
	}
	if len(calls.failed) != 1 || !strings.Contains(calls.failed[0], "Invalid config for 'automation'") || !strings.HasPrefix(calls.failed[0], "deploy: ") {
		t.Errorf("failed = %q, want the deploy's errors", calls.failed)
	}

	// Skipped: it isn't tried again.
	fh.setErrors("")
	d.etag = ""
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fh.calls().checked) != 2 {
		t.Error("tried the refused commit again")
	}
}

func TestDeploySkipsFailedChecks(t *testing.T) {
	d, gh, fh := newDeployer(t)
	ctx := context.Background()
	sha := gh.commit("main", map[string]string{"automations/a.yaml": "a\n"})
	gh.setChecks(sha, "completed", "failure")
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	calls := fh.calls()
	if len(calls.checked) != 0 {
		t.Error("deployed a commit whose Checks failed")
	}
	if len(calls.failed) != 1 || !strings.Contains(calls.failed[0], "Checks failed") {
		t.Errorf("failed = %q", calls.failed)
	}
	if d.state.Skipped != sha {
		t.Errorf("skipped = %q, want %q", d.state.Skipped, sha)
	}
}

func TestDeploySkipsCommitWithoutChecks(t *testing.T) {
	d, gh, fh := newDeployer(t)
	ctx := context.Background()
	d.ChecksTimeout = time.Millisecond
	gh.commit("main", map[string]string{"automations/a.yaml": "a\n"})
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := d.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := fh.calls(); len(calls.failed) != 1 || !strings.Contains(calls.failed[0], "No Checks passed") {
		t.Errorf("failed = %q", calls.failed)
	}
}

func TestExtractOwnedKeepsOnlyOwnedRegularFiles(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "top/automations/a.yaml"}, "a")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "top/packages/nested/p.yaml"}, "p")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "top/configuration.yaml"}, "not owned")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "top/automations/../../escape.yaml"}, "escape")
	add(&tar.Header{Typeflag: tar.TypeSymlink, Name: "top/scripts/link.yaml", Linkname: "/etc/passwd"}, "")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "top/automations"}, "a file named like a directory")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := extractOwned(&buf, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	got, err := readTree(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		filepath.Join("automations", "a.yaml"):        "a",
		filepath.Join("packages", "nested", "p.yaml"): "p",
	}
	if len(got) != len(want) {
		t.Errorf("extracted %d files, want %d: %v", len(got), len(want), got)
	}
	for p, content := range want {
		if string(got[p]) != content {
			t.Errorf("%s = %q, want %q", p, got[p], content)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.yaml")); err == nil {
		t.Error("extracted outside its directory")
	}
}

func TestRestoreAfterPartialSwap(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"automations", "scripts"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "x.yaml"), []byte("live "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	staging := filepath.Join(dir, "staging")
	previous := filepath.Join(dir, "previous")
	// Staging holds automations only: swapping scripts fails, after
	// automations moved.
	if err := os.MkdirAll(filepath.Join(staging, "automations"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved, err := swap(dir, staging, previous, []string{"automations", "scripts"})
	if err == nil {
		t.Fatal("swap succeeded without a staged scripts")
	}
	if err := restore(dir, previous, moved); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"automations", "scripts"} {
		if got := readFile(t, filepath.Join(dir, name, "x.yaml")); got != "live "+name {
			t.Errorf("%s/x.yaml = %q after restore", name, got)
		}
	}
}
