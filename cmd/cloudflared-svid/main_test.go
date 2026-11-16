package main

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pdeslaur/hangar/cloudflarests"
	"github.com/pdeslaur/hangar/internal/workloadtest"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const id = "spiffe://example.org/svc/app"

func broker(t *testing.T, url string, api *workloadtest.API) *cloudflarests.Client {
	t.Helper()
	svids, err := workloadapi.New(t.Context(), workloadapi.WithAddr(api.Addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svids.Close() })
	return &cloudflarests.Client{URL: url, SVIDs: svids, HTTP: cloudflarests.NewHTTPClient()}
}

func testConfig(t *testing.T) *config {
	dir := t.TempDir()
	return &config{
		Profile:           id + ":tunnel",
		CredentialsFile:   filepath.Join(dir, "credentials.json"),
		CloudflaredConfig: "/etc/cloudflared/config.yml",
		Cloudflared:       "/usr/local/bin/cloudflared",
	}
}

func TestPrepareRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error": "temporarily_unavailable", "error_description": "Cloudflare: down"}`)
			return
		}
		io.WriteString(w, `{"tunnel": {"account_tag": "account", "tunnel_id": "tunnel", "tunnel_secret": "c2VjcmV0"}}`)
	}))
	defer srv.Close()
	api := workloadtest.Serve(t, id)
	// An agent that hasn't attested the workload, then a broker that can't
	// reach Cloudflare.
	api.Unready(1)
	cfg := testConfig(t)

	argv, err := prepare(t.Context(), cfg, broker(t, srv.URL, api), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("called the broker %d times, want 2", got)
	}
	want := []string{
		"/usr/local/bin/cloudflared", "tunnel", "--no-autoupdate", "--config", "/etc/cloudflared/config.yml",
		"run", "--credentials-file", cfg.CredentialsFile, "tunnel",
	}
	if !slices.Equal(argv, want) {
		t.Errorf("argv %q, want %q", argv, want)
	}

	info, err := os.Stat(cfg.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("credentials file mode %v, want 0600", mode)
	}
	b, err := os.ReadFile(cfg.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	var creds map[string]string
	if err := json.Unmarshal(b, &creds); err != nil {
		t.Fatal(err)
	}
	wantCreds := map[string]string{"AccountTag": "account", "TunnelID": "tunnel", "TunnelSecret": "c2VjcmV0"}
	if !maps.Equal(creds, wantCreds) {
		t.Errorf("credentials %v, want %v", creds, wantCreds)
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.CredentialsFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("left %d files beside the credentials, want none", len(entries)-1)
	}
}

func TestPrepareStopsWhenRefused(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error": "invalid_request", "error_description": "no profile matches the token"}`)
	}))
	defer srv.Close()
	cfg := testConfig(t)

	if _, err := prepare(t.Context(), cfg, broker(t, srv.URL, workloadtest.Serve(t, id)), time.Millisecond); err == nil {
		t.Fatal("prepared, want the broker's refusal")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("called the broker %d times, want 1", got)
	}
	if _, err := os.Stat(cfg.CredentialsFile); !os.IsNotExist(err) {
		t.Errorf("credentials file: %v, want none", err)
	}
}
