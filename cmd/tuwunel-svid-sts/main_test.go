package main

import (
	"maps"
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
