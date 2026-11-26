package jellyfin_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/pdeslaur/hangar/internal/jellyfin"
	"github.com/pdeslaur/hangar/internal/jellyfin/jellyfintest"
)

// TestJellyfin gives alice and bob tokens from a real Jellyfin with an API
// key written into its database, and ends them.
func TestJellyfin(t *testing.T) {
	s := jellyfintest.Start(t)
	ctx := context.Background()
	c := &jellyfin.Client{URL: s.URL, Key: jellyfintest.Key, HTTP: http.DefaultClient}

	alice, err := c.UserID(ctx, "ALICE")
	if err != nil || alice != s.Users["alice"] {
		t.Fatalf("UserID(ALICE) = %q, %v, want %q", alice, err, s.Users["alice"])
	}
	if _, err := c.UserID(ctx, "carol"); err == nil {
		t.Error("UserID found carol")
	}

	device := jellyfin.Device{Client: "Jellyfin Web", Name: "Firefox", ID: "bridge-test-1", Version: "10.11.0"}
	first, err := c.Login(ctx, alice, device)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if status, name := jellyfintest.Me(t, s.URL, first); status != http.StatusOK || name != "alice" {
		t.Fatalf("/Users/Me with alice's token: %d %q", status, name)
	}

	// Logging the same device in again ends its first token.
	second, err := c.Login(ctx, alice, device)
	if err != nil {
		t.Fatalf("Login again: %v", err)
	}
	if status, _ := jellyfintest.Me(t, s.URL, first); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me with the first token after the second login: %d", status)
	}
	if status, name := jellyfintest.Me(t, s.URL, second); status != http.StatusOK || name != "alice" {
		t.Errorf("/Users/Me with the second token: %d %q", status, name)
	}

	// bob on the same browser: his own token, which alice's logout keeps.
	bobs, err := c.Login(ctx, s.Users["bob"], device)
	if err != nil {
		t.Fatalf("Login bob: %v", err)
	}
	if status, name := jellyfintest.Me(t, s.URL, second); status != http.StatusOK || name != "alice" {
		t.Errorf("/Users/Me with alice's token after bob's login: %d %q", status, name)
	}

	if err := c.Logout(ctx, second); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if status, _ := jellyfintest.Me(t, s.URL, second); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me after Logout: %d", status)
	}
	if status, name := jellyfintest.Me(t, s.URL, bobs); status != http.StatusOK || name != "bob" {
		t.Errorf("/Users/Me with bob's token after alice's Logout: %d %q", status, name)
	}
	if err := c.Logout(ctx, second); err != nil {
		t.Errorf("Logout again: %v", err)
	}
}
