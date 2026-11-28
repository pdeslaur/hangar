// Package jellyfintest runs a real Jellyfin for tests, with docker, set up
// with users and an API key.
package jellyfintest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pdeslaur/hangar/internal/jellyfin"
)

// Key is the API key Start writes into Jellyfin's database, named "jellyfin-sso".
const Key = "0123456789abcdef0123456789abcdef"

// Server is a Jellyfin container.
type Server struct {
	// URL is its base URL.
	URL string
	// Users are its users' IDs by name: the administrator "admin", and
	// "alice" and "bob".
	Users map[string]string

	// t is the test that started it, which its setup reports to.
	t *testing.T
	// id is the container's.
	id string
	// dir is its /config.
	dir string
}

// Start runs the linuxserver.io Jellyfin image named by JELLYFIN_IMAGE with
// docker, completes its startup wizard, creates its users, writes Key into
// its database and enables Quick Connect, for t alone. Without
// JELLYFIN_IMAGE, the test is skipped.
func Start(t *testing.T) *Server {
	t.Helper()
	s := run(t, image(t), t.TempDir())
	t.Cleanup(func() {
		s.logIfFailed(t)
		s.remove()
	})
	s.setUp()
	return s
}

// shared is the Server that Shared starts for the tests of a package, which
// Main removes once they ran.
var shared struct {
	once sync.Once
	// main is whether the package's tests run through Main.
	main bool
	// dir is the Server's /config, which outlives the test that started it.
	dir string
	s   *Server
	// ready is whether the Server is set up.
	ready bool
}

// Shared returns a Server, as Start does, shared by the tests of a package
// that runs them through Main: the first test to call it starts it. Tests
// sharing it name their devices apart, since Jellyfin keeps one token per
// device.
func Shared(t *testing.T) *Server {
	t.Helper()
	img := image(t)
	if !shared.main {
		t.Fatal("jellyfintest.Shared needs TestMain to call jellyfintest.Main")
	}
	t.Cleanup(func() {
		if shared.s != nil {
			shared.s.logIfFailed(t)
		}
	})
	shared.once.Do(func() {
		dir, err := os.MkdirTemp("", "jellyfintest")
		if err != nil {
			t.Fatal(err)
		}
		shared.dir = dir
		shared.s = run(t, img, dir)
		shared.s.setUp()
		shared.ready = true
	})
	if !shared.ready {
		t.Fatal("jellyfin failed to start in an earlier test")
	}
	return shared.s
}

// Main runs the tests of m, then removes the Server Shared started for them.
// A package whose tests call Shared calls it from its TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(jellyfintest.Main(m)) }
func Main(m *testing.M) int {
	shared.main = true
	code := m.Run()
	if shared.s != nil {
		shared.s.remove()
	}
	if shared.dir != "" {
		os.RemoveAll(shared.dir)
	}
	return code
}

// image returns the image named by JELLYFIN_IMAGE, skipping t without one.
func image(t *testing.T) string {
	t.Helper()
	image := os.Getenv("JELLYFIN_IMAGE")
	if image == "" {
		t.Skip("JELLYFIN_IMAGE unset, like lscr.io/linuxserver/jellyfin:latest")
	}
	return image
}

// run starts a container of image, its /config in dir, reporting to t.
func run(t *testing.T, image, dir string) *Server {
	t.Helper()
	out, err := exec.Command("docker", "run", "--detach",
		"--publish", "127.0.0.1::8096",
		"--volume", dir+":/config",
		// Its files are ours, so the test reaches its database.
		"--env", fmt.Sprintf("PUID=%d", os.Getuid()),
		"--env", fmt.Sprintf("PGID=%d", os.Getgid()),
		image).Output()
	if err != nil {
		t.Fatalf("starting jellyfin: %v", err)
	}
	return &Server{t: t, id: strings.TrimSpace(string(out)), dir: dir}
}

// logIfFailed logs the container's last lines if t failed.
func (s *Server) logIfFailed(t *testing.T) {
	if t.Failed() {
		logs, _ := exec.Command("docker", "logs", "--tail", "100", s.id).CombinedOutput()
		t.Logf("jellyfin logs:\n%s", logs)
	}
}

// remove removes the container.
func (s *Server) remove() {
	exec.Command("docker", "rm", "--force", s.id).Run()
}

// setUp completes Jellyfin's startup wizard, creates its users, writes Key
// into its database and enables Quick Connect.
func (s *Server) setUp() {
	s.t.Helper()
	s.wait()

	s.call(http.MethodPost, "/Startup/Configuration", "", map[string]string{"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"}, nil)
	s.call(http.MethodGet, "/Startup/User", "", nil, nil)
	s.call(http.MethodPost, "/Startup/User", "", map[string]string{"Name": "admin", "Password": "admin-password"}, nil)
	s.call(http.MethodPost, "/Startup/Complete", "", nil, nil)
	device := jellyfin.Device{Client: "jellyfintest", Name: "jellyfintest", ID: "jellyfintest", Version: "1"}
	var login struct{ AccessToken string }
	s.call(http.MethodPost, "/Users/AuthenticateByName", device.Header(""), map[string]string{"Username": "admin", "Pw": "admin-password"}, &login)
	for _, name := range []string{"alice", "bob"} {
		s.call(http.MethodPost, "/Users/New", device.Header(login.AccessToken), map[string]string{"Name": name, "Password": name + "-password"}, nil)
	}

	// Jellyfin takes a key written while it runs, but SQLite's WAL locks
	// don't cross the file sharing of Docker on macOS: write it stopped.
	s.docker("stop")
	if err := jellyfin.EnsureAPIKey(context.Background(), filepath.Join(s.dir, "data", "data", "jellyfin.db"), "jellyfin-sso", Key); err != nil {
		s.t.Fatalf("EnsureAPIKey: %v", err)
	}
	s.docker("start")
	s.wait()

	key := jellyfin.TokenHeader(Key)
	var config map[string]any
	s.call(http.MethodGet, "/System/Configuration", key, nil, &config)
	config["QuickConnectAvailable"] = true
	s.call(http.MethodPost, "/System/Configuration", key, config, nil)

	var users []struct {
		Name string
		ID   string `json:"Id"`
	}
	s.call(http.MethodGet, "/Users", key, nil, &users)
	s.Users = map[string]string{}
	for _, u := range users {
		s.Users[u.Name] = u.ID
	}
}

// wait finds the container's port, which changes when it restarts, and
// waits for Jellyfin to answer on it.
func (s *Server) wait() {
	s.t.Helper()
	out, err := exec.Command("docker", "port", s.id, "8096/tcp").Output()
	if err != nil {
		s.t.Fatalf("finding jellyfin's port: %v", err)
	}
	s.URL = "http://" + strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := s.client().Ready(context.Background())
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("jellyfin never answered: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func (s *Server) docker(command string) {
	s.t.Helper()
	if out, err := exec.Command("docker", command, s.id).CombinedOutput(); err != nil {
		s.t.Fatalf("docker %s: %v: %s", command, err, out)
	}
}

func (s *Server) client() *jellyfin.Client {
	return &jellyfin.Client{URL: s.URL, HTTP: http.DefaultClient}
}

// call sends a request to Jellyfin, failing the test unless it answers 2xx,
// and decodes its JSON response into out, if not nil.
func (s *Server) call(method, path, authorization string, in, out any) {
	s.t.Helper()
	if err := s.client().Do(context.Background(), method, path, authorization, in, out); err != nil {
		s.t.Fatal(err)
	}
}

// Me returns the status of GET /Users/Me at base, Jellyfin's URL or a proxy
// to it, with token in the Authorization header, and the user's name.
func Me(t *testing.T, base, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/Users/Me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", jellyfin.TokenHeader(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var user struct{ Name string }
	json.NewDecoder(resp.Body).Decode(&user)
	return resp.StatusCode, user.Name
}
