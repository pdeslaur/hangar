package main

import (
	"slices"
	"testing"

	"github.com/sethvargo/go-envconfig/v2"
	"maunium.net/go/mautrix/id"
)

func TestConfig(t *testing.T) {
	var cfg config
	err := envconfig.ProcessWith(t.Context(), &envconfig.Config{
		Target: &cfg,
		Lookuper: envconfig.MapLookuper(map[string]string{
			"HOMESERVER_URL": "https://matrix.example.org",
			"STS_URL":        "http://matrix:8080/",
			"STS_AUDIENCE":   "tuwunel-svid-sts",
			"ALLOWED_USERS":  "@phil:example.org,@other:example.org",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []id.UserID{"@phil:example.org", "@other:example.org"}; !slices.Equal(cfg.AllowedUsers, want) {
		t.Errorf("allowed users %v, want %v", cfg.AllowedUsers, want)
	}
	if cfg.Addr != "127.0.0.1:8090" {
		t.Errorf("addr %q, want loopback", cfg.Addr)
	}
	if cfg.InboundURL != "" {
		t.Errorf("inbound URL %q, want none", cfg.InboundURL)
	}
}
