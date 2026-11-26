package jellybridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pdeslaur/hangar/internal/jellyfin"
	"github.com/pdeslaur/hangar/internal/jellyfin/jellyfintest"
)

const userHeader = "X-Bridge-User"

// postSession sends POST /sso/session for deviceID as user, with
// headers, returning the response.
func postSession(t *testing.T, h http.Handler, user, deviceID string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(sessionRequest{DeviceID: deviceID})
	r := httptest.NewRequest(http.MethodPost, "/sso/session", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if user != "" {
		r.Header.Set(userHeader, user)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// fakeUsers is a Jellyfin knowing the user alice alone, and nothing else.
func fakeUsers(t *testing.T) *jellyfin.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `[{"Name": "alice", "Id": "u1"}]`)
	}))
	t.Cleanup(srv.Close)
	return &jellyfin.Client{URL: srv.URL, HTTP: srv.Client()}
}

func TestLoginRefuses(t *testing.T) {
	store, _ := openStore(t)
	l := &SSO{
		Sessions:   store,
		Jellyfin:   fakeUsers(t),
		UserHeader: userHeader,
		Users:      map[string]string{"phil": "alice"},
		BrowserTTL: 8 * time.Hour,
	}
	for _, tc := range []struct {
		name     string
		user     string
		deviceID string
		headers  map[string]string
		want     int
	}{
		{"no user", "", "dev1", nil, http.StatusForbidden},
		{"an unknown user", "mallory", "dev1", nil, http.StatusForbidden},
		{"no device", "phil", "", nil, http.StatusBadRequest},
		{"a device ID too long", "phil", strings.Repeat("x", maxDeviceID+1), nil, http.StatusBadRequest},
		{"a form", "phil", "dev1", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		{"another site", "phil", "dev1", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := postSession(t, l, tc.user, tc.deviceID, tc.headers); w.Code != tc.want {
				t.Errorf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}

	r := httptest.NewRequest(http.MethodGet, "/sso/login", nil)
	w := httptest.NewRecorder()
	l.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "jellyfinBridge.signIn") {
		t.Errorf("GET /sso/login: %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/sso/bridge.js", nil)
	w = httptest.NewRecorder()
	l.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "jellyfin_credentials") {
		t.Errorf("GET /sso/bridge.js: %d", w.Code)
	}
}

// TestLoginJellyfin signs a browser in against a real Jellyfin, refreshes
// its token, and uses both through the proxy.
func TestLoginJellyfin(t *testing.T) {
	s := jellyfintest.Start(t)
	ctx := context.Background()
	store, _ := openStore(t)
	store.now = time.Now
	client := &jellyfin.Client{URL: s.URL, Key: jellyfintest.Key, HTTP: http.DefaultClient}
	apps := NewApps(store, client)
	l := &SSO{
		Sessions:      store,
		Jellyfin:      client,
		Apps:          apps,
		ServerID:      "server1",
		UserHeader:    userHeader,
		Users:         map[string]string{"phil": "Alice"},
		BrowserTTL:    8 * time.Hour,
		RefreshBefore: time.Hour,
	}
	target, _ := url.Parse(s.URL)
	proxy := httptest.NewServer(NewProxy(target, store, apps, nil))
	defer proxy.Close()

	signIn := func(deviceID string) sessionResponse {
		t.Helper()
		w := postSession(t, l, "phil", deviceID, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("POST /sso/session: %d %s", w.Code, w.Body)
		}
		var resp sessionResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.UserID != s.Users["alice"] || resp.ServerID != "server1" || resp.ExpiresIn != 8*3600 || resp.RefreshBefore != 3600 {
			t.Errorf("session %+v, want alice's on server1, for 8 hours, refreshed an hour before", resp)
		}
		return resp
	}

	first := signIn("browser-1")
	if status, name := jellyfintest.Me(t, proxy.URL, first.AccessToken); status != http.StatusOK || name != "alice" {
		t.Fatalf("/Users/Me with the first token: %d %q", status, name)
	}

	// A refresh: a second token on the same session, the first still good.
	second := signIn("browser-1")
	if second.AccessToken == first.AccessToken {
		t.Error("a refresh gave the same token")
	}
	for _, token := range []string{first.AccessToken, second.AccessToken} {
		if status, name := jellyfintest.Me(t, proxy.URL, token); status != http.StatusOK || name != "alice" {
			t.Errorf("/Users/Me after the refresh: %d %q", status, name)
		}
	}
	var sessions int
	if err := store.db.Raw(`SELECT count(*) FROM sessions`).Row().Scan(&sessions); err != nil || sessions != 1 {
		t.Errorf("sessions after a refresh: %d, %v", sessions, err)
	}

	// Another browser: its own session, the first one's still good.
	other := signIn("browser-2")
	for _, token := range []string{second.AccessToken, other.AccessToken} {
		if status, name := jellyfintest.Me(t, proxy.URL, token); status != http.StatusOK || name != "alice" {
			t.Errorf("/Users/Me with two browsers: %d %q", status, name)
		}
	}
	if session, err := store.Lookup(ctx, other.AccessToken); err != nil || session.Device.ID != "browser-2" {
		t.Errorf("the second browser's session: %+v, %v", session, err)
	}
}
