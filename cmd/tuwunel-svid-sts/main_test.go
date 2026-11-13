package main

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sethvargo/go-envconfig"
)

func TestConfigUsers(t *testing.T) {
	var cfg config
	err := envconfig.ProcessWith(t.Context(), &envconfig.Config{
		Target: &cfg,
		Lookuper: envconfig.MapLookuper(map[string]string{
			"AUDIENCE":            "tuwunel-svid-sts",
			"TUWUNEL_CONFIG_FILE": "/run/tuwunel/jwt.toml",
			"USERS":               "spiffe://example.org/a=alice,spiffe://example.org/b=bob",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"spiffe://example.org/a": "alice",
		"spiffe://example.org/b": "bob",
	}
	if !maps.Equal(cfg.Users, want) {
		t.Errorf("users %v, want %v", cfg.Users, want)
	}
}

func TestConfigAddrDefaultsToLoopback(t *testing.T) {
	var cfg config
	err := envconfig.ProcessWith(t.Context(), &envconfig.Config{
		Target: &cfg,
		Lookuper: envconfig.MapLookuper(map[string]string{
			"AUDIENCE":            "tuwunel-svid-sts",
			"TUWUNEL_CONFIG_FILE": "/run/tuwunel/jwt.toml",
			"USERS":               "spiffe://example.org/a=alice",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "127.0.0.1:8080"; cfg.Addr != want {
		t.Errorf("addr %q, want %q", cfg.Addr, want)
	}
}

func TestTuwunelClientFollowsNoRedirect(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect to %s", r.URL)
	}))
	defer elsewhere.Close()
	tuwunel := httptest.NewServer(http.RedirectHandler(elsewhere.URL, http.StatusTemporaryRedirect))
	defer tuwunel.Close()

	resp, err := tuwunelClient().Post(tuwunel.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status %d, want %d", resp.StatusCode, http.StatusTemporaryRedirect)
	}
}
