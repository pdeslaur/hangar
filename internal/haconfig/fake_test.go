package haconfig

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub is a repository, served as the REST API GitHub client uses.
// Commits hold whole trees: path → content.
type fakeGitHub struct {
	t *testing.T

	mu       sync.Mutex
	trees    map[string]map[string]string
	commits  map[string]fakeCommit
	branches map[string]string
	checks   map[string]fakeCheck
	pulls    map[int]*fakePull
	nextPull int
	// headRequests counts the requests for a branch's head, 304s
	// included.
	headRequests int
}

type fakeCommit struct {
	tree, parent, message string
}

type fakeCheck struct {
	status, conclusion string
}

type fakePull struct {
	number     int
	head, base string
	open       bool
	merged     bool
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *GitHub) {
	f := &fakeGitHub{
		t:        t,
		trees:    map[string]map[string]string{},
		commits:  map[string]fakeCommit{},
		branches: map[string]string{},
		checks:   map[string]fakeCheck{},
		pulls:    map[int]*fakePull{},
		nextPull: 1,
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &GitHub{
		API:    srv.URL,
		Owner:  "owner",
		Repo:   "repo",
		Tokens: staticToken("token"),
		HTTP:   srv.Client(),
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// commit adds a commit of files onto branch, and returns its hash.
func (f *fakeGitHub) commit(branch string, files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commitLocked(branch, f.treeLocked(files), "commit")
}

func (f *fakeGitHub) treeLocked(files map[string]string) string {
	h := sha1.New()
	for _, p := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(h, "%s\x00%s\x00", p, files[p])
	}
	sha := hex.EncodeToString(h.Sum(nil))
	f.trees[sha] = maps.Clone(files)
	return sha
}

func (f *fakeGitHub) commitLocked(branch, tree, message string) string {
	parent := f.branches[branch]
	sha := hexSHA1(fmt.Sprintf("%s %s %s %d", tree, parent, message, len(f.commits)))
	f.commits[sha] = fakeCommit{tree: tree, parent: parent, message: message}
	if branch != "" {
		f.branches[branch] = sha
	}
	return sha
}

// files returns the files of the commit branch points at.
func (f *fakeGitHub) files(branch string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.trees[f.commits[f.branches[branch]].tree])
}

func (f *fakeGitHub) setChecks(sha, status, conclusion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks[sha] = fakeCheck{status, conclusion}
}

func (f *fakeGitHub) openPulls() []*fakePull {
	f.mu.Lock()
	defer f.mu.Unlock()
	var open []*fakePull
	for _, p := range f.pulls {
		if p.open {
			open = append(open, p)
		}
	}
	return open
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer token" {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/repos/owner/repo/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(path, "/")
	route := r.Method + " " + parts[0]
	if len(parts) > 1 && (parts[0] == "git" || parts[0] == "pulls") {
		route += "/" + parts[1]
	}
	switch {
	case route == "GET commits" && len(parts) == 2:
		f.headRequests++
		sha, ok := f.branches[parts[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		etag := `"` + sha + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		fmt.Fprint(w, sha)
	case route == "GET commits" && len(parts) == 3 && parts[2] == "check-runs":
		type run struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			App        struct {
				Slug string `json:"slug"`
			} `json:"app"`
		}
		// Another app's run, which doesn't count.
		other := run{Name: "build", Status: "completed", Conclusion: "failure"}
		other.App.Slug = "github-actions"
		runs := []run{other}
		if c, ok := f.checks[parts[1]]; ok {
			cg := run{Name: "Chainguard Checks (presubmit)", Status: c.status, Conclusion: c.conclusion}
			cg.App.Slug = "chainguard"
			runs = append(runs, cg)
		}
		writeJSON(w, map[string]any{"check_runs": runs})
	case route == "GET tarball":
		c, ok := f.commits[parts[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(tarball(f.t, "owner-repo-"+parts[1][:7], f.trees[c.tree]))
	case route == "GET git/commits":
		c, ok := f.commits[parts[2]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"tree": map[string]string{"sha": c.tree}})
	case route == "GET git/trees":
		files, ok := f.trees[parts[2]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var entries []TreeEntry
		dirs := map[string]bool{}
		for p, content := range files {
			if top, _, nested := strings.Cut(p, "/"); nested {
				if !dirs[top] {
					dirs[top] = true
					entries = append(entries, TreeEntry{Path: top, Type: "tree", SHA: "dir"})
				}
				continue
			}
			entries = append(entries, TreeEntry{Path: p, Type: "blob", SHA: blobSHA([]byte(content))})
		}
		writeJSON(w, map[string]any{"tree": entries})
	case route == "POST git/trees":
		var req struct {
			BaseTree string           `json:"base_tree"`
			Tree     []map[string]any `json:"tree"`
		}
		f.decode(r, &req)
		files := maps.Clone(f.trees[req.BaseTree])
		for _, e := range req.Tree {
			p := e["path"].(string)
			if content, ok := e["content"].(string); ok {
				files[p] = content
			} else if sha, ok := e["sha"]; ok && sha == nil {
				delete(files, p)
			} else {
				f.t.Errorf("tree entry %v: neither content nor a null sha", e)
			}
		}
		writeJSON(w, map[string]string{"sha": f.treeLocked(files)})
	case route == "POST git/commits":
		var req struct {
			Message string   `json:"message"`
			Tree    string   `json:"tree"`
			Parents []string `json:"parents"`
		}
		f.decode(r, &req)
		sha := hexSHA1(fmt.Sprintf("%s %v %s %d", req.Tree, req.Parents, req.Message, len(f.commits)))
		f.commits[sha] = fakeCommit{tree: req.Tree, parent: req.Parents[0], message: req.Message}
		writeJSON(w, map[string]string{"sha": sha})
	case route == "PATCH git/refs":
		branch := strings.TrimPrefix(path, "git/refs/heads/")
		if _, ok := f.branches[branch]; !ok {
			http.Error(w, `{"message":"Reference does not exist"}`, http.StatusUnprocessableEntity)
			return
		}
		var req struct {
			SHA string `json:"sha"`
		}
		f.decode(r, &req)
		f.branches[branch] = req.SHA
		writeJSON(w, map[string]any{})
	case route == "POST git/refs":
		var req struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		f.decode(r, &req)
		f.branches[strings.TrimPrefix(req.Ref, "refs/heads/")] = req.SHA
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{})
	case route == "GET pulls":
		head := strings.TrimPrefix(r.URL.Query().Get("head"), "owner:")
		var pulls []map[string]any
		for _, p := range f.pulls {
			if p.open && p.head == head && p.base == r.URL.Query().Get("base") {
				pulls = append(pulls, f.pullJSON(p))
			}
		}
		writeJSON(w, pulls)
	case route == "POST pulls":
		var req struct {
			Head, Base, Title, Body string
		}
		f.decode(r, &req)
		p := &fakePull{number: f.nextPull, head: req.Head, base: req.Base, open: true}
		f.pulls[p.number] = p
		f.nextPull++
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, f.pullJSON(p))
	case route == "PATCH pulls/"+parts[len(parts)-1] && len(parts) == 2:
		p := f.pull(parts[1])
		p.open = false
		writeJSON(w, f.pullJSON(p))
	case len(parts) == 3 && parts[0] == "pulls" && parts[2] == "merge" && r.Method == http.MethodPut:
		p := f.pull(parts[1])
		var req struct {
			MergeMethod string `json:"merge_method"`
			SHA         string `json:"sha"`
		}
		f.decode(r, &req)
		if req.MergeMethod != "squash" {
			f.t.Errorf("merge method %q, want squash", req.MergeMethod)
		}
		if head := f.branches[p.head]; req.SHA != head {
			http.Error(w, `{"message":"Head branch was modified"}`, http.StatusConflict)
			return
		}
		f.commitLocked(p.base, f.commits[f.branches[p.head]].tree, "squash")
		p.open, p.merged = false, true
		writeJSON(w, map[string]any{"merged": true})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) pull(number string) *fakePull {
	n, _ := strconv.Atoi(number)
	p, ok := f.pulls[n]
	if !ok {
		f.t.Fatalf("no pull request %s", number)
	}
	return p
}

func (f *fakeGitHub) pullJSON(p *fakePull) map[string]any {
	return map[string]any{"number": p.number, "head": map[string]string{"sha": f.branches[p.head]}}
}

func (f *fakeGitHub) decode(r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		f.t.Errorf("decoding %s %s: %v", r.Method, r.URL, err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// tarball returns files as GitHub's tarball of a commit: gzipped, under top,
// after a pax global header.
func tarball(t *testing.T, top string, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "sha"}}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top + "/", Mode: 0o755}))
	for _, p := range slices.Sorted(maps.Keys(files)) {
		must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + "/" + p, Mode: 0o644, Size: int64(len(files[p]))}))
		_, err := tw.Write([]byte(files[p]))
		must(err)
	}
	must(tw.Close())
	must(gz.Close())
	return buf.Bytes()
}

// fakeHass is the config_sync integration, on a Unix socket.
type fakeHass struct {
	mu sync.Mutex
	// errors is what checks answer.
	errors string
	// checked holds what the owned directories held at each check.
	checked []map[string][]byte
	reloads [][]string
	failed  []string
	succeed []string
	dir     string
}

func newFakeHass(t *testing.T, dir string) (*fakeHass, *Hass) {
	// t.TempDir's paths can exceed a socket path's 104 bytes on macOS.
	sockDir, err := os.MkdirTemp("", "hass")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "api.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHass{dir: dir}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(h.serve))
	srv.Listener = lis
	srv.Start()
	t.Cleanup(srv.Close)
	return h, NewHass(socket)
}

func (h *fakeHass) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var body map[string]any
	if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&body)
	}
	switch r.URL.Path {
	case "/version":
		writeJSON(w, map[string]string{"version": "2026.11.0"})
	case "/check":
		files := map[string][]byte{}
		for name := range Owned {
			tree, _ := readTree(filepath.Join(h.dir, name))
			for p, data := range tree {
				files[filepath.Join(name, p)] = data
			}
		}
		h.checked = append(h.checked, files)
		writeJSON(w, map[string]string{"errors": h.errors})
	case "/reload":
		var domains []string
		for _, d := range body["domains"].([]any) {
			domains = append(domains, d.(string))
		}
		h.reloads = append(h.reloads, domains)
		writeJSON(w, map[string]any{})
	case "/failed":
		h.failed = append(h.failed, body["kind"].(string)+": "+body["message"].(string))
		writeJSON(w, map[string]any{})
	case "/succeeded":
		h.succeed = append(h.succeed, body["kind"].(string)+" "+body["sha"].(string))
		writeJSON(w, map[string]any{})
	default:
		http.NotFound(w, r)
	}
}

// hassCalls is what fakeHass was asked.
type hassCalls struct {
	checked []map[string][]byte
	reloads [][]string
	failed  []string
	succeed []string
}

func (h *fakeHass) calls() hassCalls {
	h.mu.Lock()
	defer h.mu.Unlock()
	return hassCalls{
		checked: slices.Clone(h.checked),
		reloads: slices.Clone(h.reloads),
		failed:  slices.Clone(h.failed),
		succeed: slices.Clone(h.succeed),
	}
}

func (h *fakeHass) setErrors(errors string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errors = errors
}

func hexSHA1(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
