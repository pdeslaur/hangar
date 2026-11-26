package jellyfin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// server is a Jellyfin container, its startup wizard done.
type server struct {
	// id is the container's.
	id string
	// dir is its /config, on the host.
	dir string
	// url is its base URL, which changes when it restarts.
	url string
}

// startJellyfin runs the linuxserver.io Jellyfin image named by
// JELLYFIN_IMAGE with docker, completes its startup wizard with the
// administrator "admin", and creates the users "alice" and "bob". Without
// JELLYFIN_IMAGE, the test is skipped.
func startJellyfin(t *testing.T) *server {
	t.Helper()
	image := os.Getenv("JELLYFIN_IMAGE")
	if image == "" {
		t.Skip("JELLYFIN_IMAGE unset, like lscr.io/linuxserver/jellyfin:latest")
	}
	dir := t.TempDir()
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
	s := &server{id: strings.TrimSpace(string(out)), dir: dir}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "100", s.id).CombinedOutput()
			t.Logf("jellyfin logs:\n%s", logs)
		}
		exec.Command("docker", "rm", "--force", s.id).Run()
	})
	s.wait(t)

	wizard := &Client{URL: s.url, HTTP: http.DefaultClient}
	ctx := context.Background()
	for _, step := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/Startup/Configuration", map[string]string{"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"}},
		{http.MethodGet, "/Startup/User", nil},
		{http.MethodPost, "/Startup/User", map[string]string{"Name": "admin", "Password": "admin-password"}},
		{http.MethodPost, "/Startup/Complete", nil},
	} {
		if err := wizard.do(ctx, step.method, step.path, "", step.body, nil); err != nil {
			t.Fatalf("startup wizard: %v", err)
		}
	}
	device := Device{Client: "test", Name: "test", ID: "test-admin", Version: "1"}
	var login struct{ AccessToken string }
	err = wizard.do(ctx, http.MethodPost, "/Users/AuthenticateByName", device.Header(""),
		map[string]string{"Username": "admin", "Pw": "admin-password"}, &login)
	if err != nil {
		t.Fatalf("logging in as admin: %v", err)
	}
	for _, name := range []string{"alice", "bob"} {
		err = wizard.do(ctx, http.MethodPost, "/Users/New", device.Header(login.AccessToken),
			map[string]string{"Name": name, "Password": name + "-password"}, nil)
		if err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}
	return s
}

// wait finds the container's port and waits for Jellyfin to answer on it.
func (s *server) wait(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "port", s.id, "8096/tcp").Output()
	if err != nil {
		t.Fatalf("finding jellyfin's port: %v", err)
	}
	s.url = "http://" + strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	deadline := time.Now().Add(2 * time.Minute)
	for {
		// While it starts, a setup server answers /System/Info/Public and
		// 503 to the rest, so wait on an anonymous path it doesn't serve.
		resp, err := http.Get(s.url + "/Branding/Configuration")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("jellyfin never answered: %v", err)
		}
		time.Sleep(time.Second)
	}
}

// docker runs a docker command on the container.
func (s *server) docker(t *testing.T, command string) {
	t.Helper()
	if out, err := exec.Command("docker", command, s.id).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v: %s", command, err, out)
	}
}

// me returns the status of GET /Users/Me with token, and the user's name.
func (s *server) me(t *testing.T, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.url+"/Users/Me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", `MediaBrowser Token="`+token+`"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var user struct{ Name string }
	json.NewDecoder(resp.Body).Decode(&user)
	return resp.StatusCode, user.Name
}

// TestJellyfin gives alice tokens from a real Jellyfin with an API key the
// test writes into its database, and ends them.
func TestJellyfin(t *testing.T) {
	s := startJellyfin(t)
	ctx := context.Background()

	// Jellyfin takes a key written while it runs, but SQLite's WAL locks
	// don't cross the file sharing of Docker on macOS: write it stopped.
	s.docker(t, "stop")
	const key = "0123456789abcdef0123456789abcdef"
	if err := EnsureAPIKey(ctx, filepath.Join(s.dir, "data", "data", "jellyfin.db"), "bridge", key); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	s.docker(t, "start")
	s.wait(t)
	c := &Client{URL: s.url, Key: key, HTTP: http.DefaultClient}

	var config map[string]any
	if err := c.do(ctx, http.MethodGet, "/System/Configuration", c.keyHeader(), nil, &config); err != nil {
		t.Fatalf("reading the configuration with the key: %v", err)
	}
	config["QuickConnectAvailable"] = true
	if err := c.do(ctx, http.MethodPost, "/System/Configuration", c.keyHeader(), config, nil); err != nil {
		t.Fatalf("enabling Quick Connect: %v", err)
	}

	alice, err := c.UserID(ctx, "ALICE")
	if err != nil {
		t.Fatalf("UserID: %v", err)
	}
	bob, err := c.UserID(ctx, "bob")
	if err != nil {
		t.Fatalf("UserID: %v", err)
	}
	if _, err := c.UserID(ctx, "carol"); err == nil {
		t.Error("UserID found carol")
	}

	device := Device{Client: "Jellyfin Web", Name: "Firefox", ID: "bridge-test-1", Version: "10.11.0"}
	first, err := c.Login(ctx, alice, device)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if status, name := s.me(t, first); status != http.StatusOK || name != "alice" {
		t.Fatalf("/Users/Me with alice's token: %d %q", status, name)
	}

	// Logging the same device in again ends its first token.
	second, err := c.Login(ctx, alice, device)
	if err != nil {
		t.Fatalf("Login again: %v", err)
	}
	if status, _ := s.me(t, first); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me with the first token after the second login: %d", status)
	}
	if status, name := s.me(t, second); status != http.StatusOK || name != "alice" {
		t.Errorf("/Users/Me with the second token: %d %q", status, name)
	}

	// bob on the same browser: his own token, which alice's logout keeps.
	bobs, err := c.Login(ctx, bob, device)
	if err != nil {
		t.Fatalf("Login bob: %v", err)
	}
	if status, name := s.me(t, second); status != http.StatusOK || name != "alice" {
		t.Errorf("/Users/Me with alice's token after bob's login: %d %q", status, name)
	}

	if err := c.Logout(ctx, second); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if status, _ := s.me(t, second); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me after Logout: %d", status)
	}
	if status, name := s.me(t, bobs); status != http.StatusOK || name != "bob" {
		t.Errorf("/Users/Me with bob's token after alice's Logout: %d %q", status, name)
	}
	if err := c.Logout(ctx, second); err != nil {
		t.Errorf("Logout again: %v", err)
	}
}
