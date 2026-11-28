package main

import (
	"testing"
	"time"

	"github.com/sethvargo/go-envconfig/v2"
)

func TestConfig(t *testing.T) {
	var cfg config
	err := envconfig.ProcessWith(t.Context(), &envconfig.Config{
		Target: &cfg,
		Lookuper: envconfig.MapLookuper(map[string]string{
			"SEND_URL": "http://127.0.0.1:8090/send",
			"ROOM":     "#alerts:example.org",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9093" {
		t.Errorf("addr %q, want loopback on Alertmanager's port", cfg.Addr)
	}
	if cfg.StoreDir != "/var/lib/alert-relay" {
		t.Errorf("store dir %q", cfg.StoreDir)
	}
	if cfg.SweepInterval != time.Minute {
		t.Errorf("sweep interval %v, want a minute", cfg.SweepInterval)
	}
}
