package login

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tuwunel is a tuwunel container.
type tuwunel struct {
	// url is its client API's base URL.
	url string
	// id is the container's.
	id string
	// config is the config file it reads, on the host.
	config string
}

// startTuwunel runs the tuwunel image named by TUWUNEL_IMAGE with docker,
// reading the config file signer writes. It creates the user "bot". Without
// TUWUNEL_IMAGE, the test is skipped.
func startTuwunel(t *testing.T, signer *Signer) tuwunel {
	t.Helper()
	image := os.Getenv("TUWUNEL_IMAGE")
	if image == "" {
		t.Skip("TUWUNEL_IMAGE unset, like ghcr.io/matrix-construct/tuwunel:latest")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "jwt.toml")
	if err := signer.WriteTuwunelConfig(config); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "run", "--detach", "--rm",
		"--publish", "127.0.0.1::6167",
		"--volume", dir+":/etc/sts:ro",
		"--env", "TUWUNEL_CONFIG=/etc/sts/jwt.toml",
		"--env", "TUWUNEL_SERVER_NAME=example.org",
		"--env", "TUWUNEL_DATABASE_PATH=/tmp/db",
		"--env", "TUWUNEL_ADDRESS=0.0.0.0",
		"--env", "TUWUNEL_PORT=6167",
		image, "--execute", "users create-user bot").Output()
	if err != nil {
		t.Fatalf("starting tuwunel: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Logf("tuwunel logs:\n%s", logs)
		}
		exec.Command("docker", "rm", "--force", id).Run()
	})
	out, err = exec.Command("docker", "port", id, "6167/tcp").Output()
	if err != nil {
		t.Fatalf("finding tuwunel's port: %v", err)
	}
	url := "http://" + strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])

	// tuwunel listens, then runs --execute: wait for the user to exist.
	deadline := time.Now().Add(time.Minute)
	for {
		resp, err := http.Get(url + "/_matrix/client/v3/profile/@bot:example.org")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return tuwunel{url: url, id: id, config: config}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("tuwunel never served bot's profile: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// login asks h for a session with svid, returning tuwunel's status and
// response.
func login(t *testing.T, h http.Handler, svid string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer "+svid)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body, err)
	}
	return rec.Code, body
}

// call sends a request with an access token to tuwunel, returning its status.
func call(t *testing.T, method, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestTuwunel checks the config file and logins against a real tuwunel.
func TestTuwunel(t *testing.T) {
	signer, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	url := startTuwunel(t, signer).url
	ca := newAuthority(t)
	const ghost = "spiffe://example.org/ghost"
	h := &Handler{
		Bundles:   ca.bundle,
		Audience:  testAudience,
		Users:     map[string]string{bot: "bot", ghost: "ghost"},
		Signer:    signer,
		MatrixURL: url,
		Client:    http.DefaultClient,
	}
	later := time.Now().Add(5 * time.Minute)

	status, first := login(t, h, ca.svid(t, bot, testAudience, later))
	if status != http.StatusOK {
		t.Fatalf("logging in as bot: %d %v", status, first)
	}
	if first["user_id"] != "@bot:example.org" || first["device_id"] != DeviceID {
		t.Errorf("session %v, want @bot:example.org on %s", first, DeviceID)
	}
	if _, ok := first["expires_in_ms"]; !ok {
		t.Errorf("session %v doesn't expire", first)
	}
	if _, ok := first["refresh_token"]; ok {
		t.Errorf("session %v has a refresh token", first)
	}
	whoami := url + "/_matrix/client/v3/account/whoami"
	token, _ := first["access_token"].(string)
	if got := call(t, http.MethodGet, whoami, token); got != http.StatusOK {
		t.Errorf("whoami with the access token: %d", got)
	}

	// Logging in again on the same device adds an access token to it,
	// unlike the spec says, leaving the first valid until the caller logs
	// it out.
	status, second := login(t, h, ca.svid(t, bot, testAudience, later))
	if status != http.StatusOK || second["device_id"] != DeviceID {
		t.Fatalf("logging in as bot again: %d %v", status, second)
	}
	if got := call(t, http.MethodGet, whoami, second["access_token"].(string)); got != http.StatusOK {
		t.Errorf("whoami with the second access token: %d", got)
	}
	if got := call(t, http.MethodGet, whoami, token); got != http.StatusOK {
		t.Errorf("whoami with the first access token: %d", got)
	}

	// Logging out deletes the device, ending every access token on it.
	if got := call(t, http.MethodPost, url+"/_matrix/client/v3/logout", token); got != http.StatusOK {
		t.Errorf("logging out the first access token: %d", got)
	}
	for i, token := range []string{token, second["access_token"].(string)} {
		if got := call(t, http.MethodGet, whoami, token); got != http.StatusUnauthorized {
			t.Errorf("whoami with access token %d after logging out: %d, want %d", i+1, got, http.StatusUnauthorized)
		}
	}
	status, third := login(t, h, ca.svid(t, bot, testAudience, later))
	if status != http.StatusOK {
		t.Fatalf("logging in as bot after logging out: %d %v", status, third)
	}
	if got := call(t, http.MethodGet, whoami, third["access_token"].(string)); got != http.StatusOK {
		t.Errorf("whoami after logging in again: %d", got)
	}

	// tuwunel creates no user.
	status, body := login(t, h, ca.svid(t, ghost, testAudience, later))
	if status == http.StatusOK || body["errcode"] != "M_NOT_FOUND" {
		t.Errorf("logging in as ghost: %d %v, want M_NOT_FOUND", status, body)
	}
}

// TestTuwunelReload checks that tuwunel trusts the key of the config file it
// reads again on SIGUSR1, without restarting, and no longer the one before.
func TestTuwunelReload(t *testing.T) {
	before, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	tw := startTuwunel(t, before)
	ca := newAuthority(t)
	handler := func(signer *Signer) http.Handler {
		return &Handler{
			Bundles:   ca.bundle,
			Audience:  testAudience,
			Users:     map[string]string{bot: "bot"},
			Signer:    signer,
			MatrixURL: tw.url,
			Client:    http.DefaultClient,
		}
	}
	later := time.Now().Add(5 * time.Minute)
	if status, body := login(t, handler(before), ca.svid(t, bot, testAudience, later)); status != http.StatusOK {
		t.Fatalf("logging in before reloading: %d %v", status, body)
	}

	after, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	if err := after.WriteTuwunelConfig(tw.config); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "kill", "--signal", "USR1", tw.id).CombinedOutput(); err != nil {
		t.Fatalf("signaling tuwunel: %v: %s", err, out)
	}

	// tuwunel reloads in the background: wait for the key to be trusted.
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, body := login(t, handler(after), ca.svid(t, bot, testAudience, later))
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("logging in with the reloaded key: %d %v", status, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if status, body := login(t, handler(before), ca.svid(t, bot, testAudience, later)); status == http.StatusOK {
		t.Errorf("logging in with the key before reloading: %d %v, want an error", status, body)
	}
}
