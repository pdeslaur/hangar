package haconfig

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/chainguard-dev/clog"
)

// Owned are the directories the repository owns, by the reload a change to
// each needs.
var Owned = map[string]string{
	"automations": ReloadAutomation,
	"scripts":     ReloadScript,
	"scenes":      ReloadScene,
	"packages":    ReloadAll,
}

// StateDir is the directory, under the configuration directory, holding
// what ha-config-sync keeps: the integration's socket, its state, and the
// directories of a deploy.
const StateDir = ".config_sync"

// Limits on a commit's files, which are YAML.
const (
	maxFile  = 1 << 20
	maxFiles = 64 << 20
)

// Deployer deploys the commits of a branch whose Checks passed to Home
// Assistant's configuration directory.
type Deployer struct {
	// Dir is Home Assistant's configuration directory.
	Dir    string
	Branch string
	GitHub *GitHub
	Hass   *Hass
	// Interval is how often it looks for a new commit.
	Interval time.Duration
	// ChecksTimeout is how long it waits for a commit's Checks before
	// skipping it.
	ChecksTimeout time.Duration

	// head is the branch's commit it last saw.
	head  string
	state deployState
	// pending is the commit waiting for its Checks, and since when.
	pending      string
	pendingSince time.Time
}

// deployState is what Deployer remembers across restarts.
type deployState struct {
	// Applied is the last commit deployed.
	Applied string `json:"applied"`
	// Skipped is the last commit refused, by its Checks or Home Assistant's,
	// which it won't try again.
	Skipped string `json:"skipped"`
}

// Run deploys until ctx is done.
func (d *Deployer) Run(ctx context.Context) error {
	if err := d.load(); err != nil {
		return err
	}
	ticker := time.NewTicker(d.Interval)
	defer ticker.Stop()
	for {
		if err := d.Poll(ctx); err != nil {
			clog.WarnContext(ctx, "deploying", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Poll looks for a new commit on the branch, and deploys it once its Checks
// passed.
func (d *Deployer) Poll(ctx context.Context) error {
	sha, changed, err := d.GitHub.Head(ctx, d.Branch, d.head)
	if err != nil {
		return err
	}
	if changed {
		d.head = sha
		if sha != d.pending {
			d.pending, d.pendingSince = sha, time.Now()
		}
	}
	if d.pending == "" || d.pending == d.state.Applied || d.pending == d.state.Skipped {
		return nil
	}
	sha = d.pending

	checks, err := d.GitHub.Checks(ctx, sha)
	if err != nil {
		return err
	}
	switch checks {
	case ChecksPending:
		if time.Since(d.pendingSince) < d.ChecksTimeout {
			return nil
		}
		return d.skip(ctx, sha, fmt.Sprintf("No Checks passed on %s within %s.", sha, d.ChecksTimeout))
	case ChecksFailed:
		return d.skip(ctx, sha, fmt.Sprintf("Checks failed on %s.", sha))
	}
	return d.Apply(ctx, sha)
}

// Apply deploys sha: it replaces the owned directories that changed, has
// Home Assistant check the configuration, and reloads what changed, or puts
// the directories back when the check fails.
func (d *Deployer) Apply(ctx context.Context, sha string) error {
	// Before touching anything: a check it can't ask for would leave the
	// files unchecked.
	version, err := d.Hass.Version(ctx)
	if err != nil {
		return fmt.Errorf("home assistant isn't serving config_sync yet: %w", err)
	}
	staging := filepath.Join(d.Dir, StateDir, "staging")
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := d.extract(ctx, sha, staging); err != nil {
		return err
	}

	var changed []string
	for _, name := range slices.Sorted(maps.Keys(Owned)) {
		same, err := sameTree(filepath.Join(d.Dir, name), filepath.Join(staging, name))
		if err != nil {
			return err
		}
		if !same {
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		clog.InfoContext(ctx, "nothing to deploy", "sha", sha)
		return d.applied(ctx, sha)
	}

	previous := filepath.Join(d.Dir, StateDir, "previous")
	if moved, err := swap(d.Dir, staging, previous, changed); err != nil {
		return errors.Join(err, restore(d.Dir, previous, moved))
	}
	errs, err := d.Hass.Check(ctx)
	if err != nil {
		return errors.Join(fmt.Errorf("checking %s: %w", sha, err), restore(d.Dir, previous, changed))
	}
	if errs != "" {
		if err := restore(d.Dir, previous, changed); err != nil {
			return err
		}
		return d.skip(ctx, sha, fmt.Sprintf("Home Assistant %s refused %s, which was rolled back:\n\n%s", version, sha, errs))
	}

	reloads := reloadsOf(changed)
	clog.InfoContext(ctx, "deployed", "sha", sha, "changed", changed, "reloads", reloads)
	if err := d.Hass.Reload(ctx, reloads); err != nil {
		// The files are valid and stay: the next reload or restart picks
		// them up.
		if err := d.Hass.Failed(ctx, KindDeploy, sha, fmt.Sprintf("Deployed %s, but reloading failed: %v", sha, err)); err != nil {
			clog.WarnContext(ctx, "notifying", "error", err)
		}
		d.state.Applied = sha
		return errors.Join(err, d.save())
	}
	return d.applied(ctx, sha)
}

func (d *Deployer) applied(ctx context.Context, sha string) error {
	if err := d.Hass.Succeeded(ctx, KindDeploy, sha); err != nil {
		clog.WarnContext(ctx, "notifying", "error", err)
	}
	d.state.Applied = sha
	return d.save()
}

func (d *Deployer) skip(ctx context.Context, sha, message string) error {
	clog.WarnContext(ctx, "skipping", "sha", sha, "reason", message)
	if err := d.Hass.Failed(ctx, KindDeploy, sha, message); err != nil {
		clog.WarnContext(ctx, "notifying", "error", err)
	}
	d.state.Skipped = sha
	return d.save()
}

// reloadsOf returns the reloads the changed directories need: all of them
// at once when packages changed.
func reloadsOf(changed []string) []string {
	var reloads []string
	for _, name := range changed {
		if Owned[name] == ReloadAll {
			return []string{ReloadAll}
		}
		reloads = append(reloads, Owned[name])
	}
	return reloads
}

// extract writes the owned directories of sha's tarball into dir, each
// directory present even when the commit has none.
func (d *Deployer) extract(ctx context.Context, sha, dir string) error {
	for name := range Owned {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			return err
		}
	}
	body, err := d.GitHub.Tarball(ctx, sha)
	if err != nil {
		return err
	}
	defer body.Close()
	return extractOwned(body, dir)
}

// extractOwned writes the regular files of a gzipped tarball, under its top
// directory, that are in an owned directory, into dir.
func extractOwned(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// Symbolic links and the rest have no place in the configuration.
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// Under the top directory, like owner-repo-0123abc/automations/x.yaml.
		_, name, ok := strings.Cut(h.Name, "/")
		if !ok || !filepath.IsLocal(name) {
			continue
		}
		top, _, _ := strings.Cut(name, "/")
		if _, owned := Owned[top]; !owned || top == name {
			continue
		}
		if h.Size > maxFile {
			return fmt.Errorf("%s: %d bytes, more than %d", name, h.Size, maxFile)
		}
		if total += h.Size; total > maxFiles {
			return fmt.Errorf("the owned directories hold more than %d bytes", maxFiles)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxFile))
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
}

// sameTree reports whether directories a and b hold the same files, a
// missing directory holding none.
func sameTree(a, b string) (bool, error) {
	fa, err := readTree(a)
	if err != nil {
		return false, err
	}
	fb, err := readTree(b)
	if err != nil {
		return false, err
	}
	return maps.EqualFunc(fa, fb, bytes.Equal), nil
}

func readTree(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == dir {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	return files, err
}

// swap moves each of names from dir into previous, and from staging into
// dir, with renames on the same filesystem. It returns the names it moved
// out of dir, which restore puts back, even when it fails.
func swap(dir, staging, previous string, names []string) (moved []string, err error) {
	if err := os.RemoveAll(previous); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(previous, 0o755); err != nil {
		return nil, err
	}
	for _, name := range names {
		err := os.Rename(filepath.Join(dir, name), filepath.Join(previous, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return moved, err
		}
		moved = append(moved, name)
		if err := os.Rename(filepath.Join(staging, name), filepath.Join(dir, name)); err != nil {
			return moved, err
		}
	}
	return moved, nil
}

// restore puts each of names back from previous into dir, as swap found
// them: a name previous lacks wasn't in dir.
func restore(dir, previous string, names []string) error {
	var errs []error
	for _, name := range names {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		err := os.Rename(filepath.Join(previous, name), filepath.Join(dir, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (d *Deployer) statePath() string {
	return filepath.Join(d.Dir, StateDir, "state.json")
}

func (d *Deployer) load() error {
	b, err := os.ReadFile(d.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &d.state)
}

func (d *Deployer) save() error {
	b, err := json.Marshal(d.state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.statePath()), 0o700); err != nil {
		return err
	}
	tmp := d.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.statePath())
}
