package jellybridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/pdeslaur/hangar/internal/jellyfin"
	"github.com/pdeslaur/hangar/internal/jellyfin/jellyfintest"
)

// upgrade asks for a WebSocket at url and returns the status.
func upgrade(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestProxyJellyfin sends requests through the proxy to a real Jellyfin.
func TestProxyJellyfin(t *testing.T) {
	s := jellyfintest.Start(t)
	ctx := context.Background()
	c := &jellyfin.Client{URL: s.URL, Key: jellyfintest.Key, HTTP: http.DefaultClient}
	store, _ := openStore(t)
	store.now = time.Now

	device := jellyfin.Device{Client: "Jellyfin Web", Name: "Firefox", ID: "bridge-test-1", Version: "10.11.0"}
	login, err := c.Login(ctx, s.Users["alice"], device)
	if err != nil {
		t.Fatal(err)
	}
	jellyfinToken := login.AccessToken
	session, err := store.CreateSession(ctx, Session{
		Kind: Browser, UserID: s.Users["alice"], Username: "alice", Device: device, JellyfinToken: jellyfinToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.IssueToken(ctx, session.ID, 8*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(s.URL)
	proxy := httptest.NewServer(NewProxy(target, NewApps(store, nil)))
	defer proxy.Close()

	if status, name := jellyfintest.Me(t, proxy.URL, token); status != http.StatusOK || name != "alice" {
		t.Errorf("/Users/Me with the bridge token: %d %q", status, name)
	}
	if status, _ := jellyfintest.Me(t, proxy.URL, jellyfinToken); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me through the proxy with the Jellyfin token: %d", status)
	}
	if status, _ := jellyfintest.Me(t, s.URL, token); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me at Jellyfin with the bridge token: %d", status)
	}

	// The WebSocket gets its token in the URL; Jellyfin refuses it without.
	socket := proxy.URL + "/socket?deviceId=" + device.ID
	if status := upgrade(t, socket+"&api_key="+token); status != http.StatusSwitchingProtocols {
		t.Errorf("WebSocket with the bridge token: %d", status)
	}
	if status := upgrade(t, socket+"&api_key="+jellyfinToken); status == http.StatusSwitchingProtocols {
		t.Error("WebSocket with the Jellyfin token through the proxy: switched protocols")
	}
}
