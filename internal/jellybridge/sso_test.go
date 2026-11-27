package jellybridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pdeslaur/hangar/internal/jellyfin"
	"github.com/pdeslaur/hangar/internal/jellyfin/jellyfintest"
)

const userHeader = "X-Bridge-User"

// browse sends a request from a browser signed in to the single sign-on as
// user, with the cookies and headers given, returning the response.
func browse(t *testing.T, h http.Handler, method, path, user string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if user != "" {
		r.Header.Set(userHeader, user)
	}
	for _, c := range cookies {
		r.AddCookie(c)
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

func TestSSORefuses(t *testing.T) {
	store, _ := openStore(t)
	l := &SSO{
		Apps:       NewApps(store, fakeUsers(t)),
		UserHeader: userHeader,
		Users:      map[string]string{"phil": "alice"},
		BrowserTTL: 8 * time.Hour,
	}
	for _, tc := range []struct {
		name, method, path, user string
		headers                  map[string]string
		want                     int
	}{
		{"no user", http.MethodGet, "/sso/login", "", nil, http.StatusForbidden},
		{"an unknown user", http.MethodGet, "/sso/login", "mallory", nil, http.StatusForbidden},
		{"no user's account page", http.MethodGet, "/sso/", "", nil, http.StatusForbidden},
		{"no user's session", http.MethodPost, "/sso/session", "", nil, http.StatusForbidden},
		{"another site", http.MethodPost, "/sso/session", "phil", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"another site's form", http.MethodPost, "/sso/code", "phil", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"another origin", http.MethodPost, "/sso/revoke", "phil", map[string]string{"Sec-Fetch-Site": "", "Origin": "https://evil.example"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := browse(t, l, tc.method, tc.path, tc.user, nil, tc.headers); w.Code != tc.want {
				t.Errorf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}

	// Without Users, a name Jellyfin doesn't know.
	unmapped := &SSO{Apps: l.Apps, UserHeader: userHeader, BrowserTTL: 8 * time.Hour}
	if w := browse(t, unmapped, http.MethodGet, "/sso/login", "mallory", nil, nil); w.Code != http.StatusForbidden {
		t.Errorf("an unknown user without Users: status %d, want %d: %s", w.Code, http.StatusForbidden, w.Body)
	}
}

func TestSSOPages(t *testing.T) {
	store, _ := openStore(t)
	l := &SSO{
		Apps:       NewApps(store, fakeUsers(t)),
		UserHeader: userHeader,
		Users:      map[string]string{"phil": "alice"},
		BrowserTTL: 8 * time.Hour,
	}
	account := browse(t, l, http.MethodGet, "/sso/", "phil", nil, nil)
	for _, want := range []string{"<title>My devices</title>", `src="/sso-static/unpoly.min.js"`, `action="/sso/quickconnect"`, "None."} {
		if account.Code != http.StatusOK || !strings.Contains(account.Body.String(), want) {
			t.Errorf("GET /sso/: %d, no %s in %s", account.Code, want, account.Body)
		}
	}
	if cc := account.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("GET /sso/: Cache-Control %q", cc)
	}

	logout := browse(t, l, http.MethodGet, "/sso/logout", "", nil, nil)
	if logout.Code != http.StatusFound || logout.Header().Get("Location") != "/web/" {
		t.Errorf("GET /sso/logout: %d to %q", logout.Code, logout.Header().Get("Location"))
	}

	for _, tc := range []struct{ name, has string }{
		{"bridge.js", "jellyfin_credentials"},
		{"style.css", "--accent"},
		{"unpoly.min.js", "Unpoly 3.15.0"},
		{"unpoly.min.css", "up-"},
	} {
		w := browse(t, l, http.MethodGet, "/sso-static/"+tc.name, "", nil, nil)
		etag := w.Header().Get("ETag")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tc.has) || w.Header().Get("Cache-Control") != "no-cache" || etag == "" {
			t.Errorf("GET /sso-static/%s: %d, Cache-Control %q, ETag %q", tc.name, w.Code, w.Header().Get("Cache-Control"), etag)
		}
		if w := browse(t, l, http.MethodGet, "/sso-static/"+tc.name, "", nil, map[string]string{"If-None-Match": etag}); w.Code != http.StatusNotModified {
			t.Errorf("GET /sso-static/%s, current: %d", tc.name, w.Code)
		}
	}
	for _, path := range []string{"/sso-static/", "/sso-static/missing.js", "/sso-static/../sso/", "/sso-static/account.html", "/sso/bridge.js", "/sso/style.css"} {
		if w := browse(t, l, http.MethodGet, path, "phil", nil, nil); w.Code == http.StatusOK {
			t.Errorf("GET %s: %d", path, w.Code)
		}
	}
}

// storedSession is the session login.html hands bridge.js.
var storedSession = regexp.MustCompile(`jellyfinBridge\.store\((\{.*?\})\);`)

// TestLoginJellyfin signs browsers in against a real Jellyfin, refreshes
// their tokens, and uses them through the proxy.
func TestLoginJellyfin(t *testing.T) {
	s := jellyfintest.Start(t)
	ctx := context.Background()
	store, _ := openStore(t)
	store.now = time.Now
	client := &jellyfin.Client{URL: s.URL, Key: jellyfintest.Key, HTTP: http.DefaultClient}
	apps := NewApps(store, client)
	l := &SSO{
		Apps:          apps,
		ServerID:      "server1",
		UserHeader:    userHeader,
		Users:         map[string]string{"phil": "Alice"},
		BrowserTTL:    8 * time.Hour,
		RefreshBefore: time.Hour,
	}
	target, _ := url.Parse(s.URL)
	proxy := httptest.NewServer(NewProxy(target, apps))
	defer proxy.Close()

	// check checks a session from the bridge, and returns the device cookie
	// it came with.
	check := func(what string, w *httptest.ResponseRecorder, session sessionResponse) *http.Cookie {
		t.Helper()
		if session.UserID != s.Users["alice"] || session.ServerID != "server1" || session.ExpiresIn != 8*3600 || session.RefreshBefore != 3600 {
			t.Errorf("%s: session %+v, want alice's on server1, for 8 hours, refreshed an hour before", what, session)
		}
		var device *http.Cookie
		for _, c := range w.Result().Cookies() {
			if c.Name == deviceCookie {
				device = c
			}
		}
		if device == nil || device.Value != session.DeviceID || !device.HttpOnly || !device.Secure || device.Path != "/sso/" {
			t.Fatalf("%s: device cookie %+v, for device %q", what, device, session.DeviceID)
		}
		if status, name := jellyfintest.Me(t, proxy.URL, session.AccessToken); status != http.StatusOK || name != "alice" {
			t.Errorf("%s: /Users/Me with its token: %d %q", what, status, name)
		}
		return device
	}
	// login opens /sso/login as user, with the cookies.
	login := func(h http.Handler, user string, cookies ...*http.Cookie) (sessionResponse, *http.Cookie) {
		t.Helper()
		w := browse(t, h, http.MethodGet, "/sso/login", user, cookies, nil)
		m := storedSession.FindStringSubmatch(w.Body.String())
		if w.Code != http.StatusOK || m == nil {
			t.Fatalf("GET /sso/login: %d %s", w.Code, w.Body)
		}
		var session sessionResponse
		if err := json.Unmarshal([]byte(m[1]), &session); err != nil {
			t.Fatalf("the page's session: %v: %s", err, m[1])
		}
		return session, check("GET /sso/login", w, session)
	}

	first, device := login(l, "phil")

	// A refresh: a second token on the same session, the first still good.
	w := browse(t, l, http.MethodPost, "/sso/session", "phil", []*http.Cookie{device}, nil)
	var second sessionResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &second) != nil {
		t.Fatalf("POST /sso/session: %d %s", w.Code, w.Body)
	}
	check("POST /sso/session", w, second)
	if second.AccessToken == first.AccessToken || second.DeviceID != first.DeviceID {
		t.Errorf("a refresh gave %+v after %+v", second, first)
	}
	// Signing in again once the token expired: the same session again.
	third, _ := login(l, "phil", device)
	for _, token := range []string{first.AccessToken, second.AccessToken, third.AccessToken} {
		if status, name := jellyfintest.Me(t, proxy.URL, token); status != http.StatusOK || name != "alice" {
			t.Errorf("/Users/Me after the refresh: %d %q", status, name)
		}
	}
	var sessions int
	if err := store.db.Raw(`SELECT count(*) FROM sessions`).Row().Scan(&sessions); err != nil || sessions != 1 {
		t.Errorf("sessions after refreshing: %d, %v", sessions, err)
	}

	// Another browser: its own session, the first one's still good.
	other, _ := login(l, "phil")
	if other.DeviceID == first.DeviceID {
		t.Error("two browsers got the same device")
	}
	if status, name := jellyfintest.Me(t, proxy.URL, third.AccessToken); status != http.StatusOK || name != "alice" {
		t.Errorf("/Users/Me with two browsers: %d %q", status, name)
	}
	if session, err := store.Lookup(ctx, other.AccessToken); err != nil || session.Device.ID != other.DeviceID {
		t.Errorf("the second browser's session: %+v, %v", session, err)
	}

	// A device cookie too long for a device ID: a device of its own.
	long := &http.Cookie{Name: deviceCookie, Value: strings.Repeat("x", maxDeviceID+1)}
	if session, _ := login(l, "phil", long); session.DeviceID == long.Value {
		t.Error("a device ID too long was used")
	}

	// Without Users, the signed-in name is the Jellyfin username.
	unmapped := &SSO{Apps: apps, ServerID: "server1", UserHeader: userHeader, BrowserTTL: 8 * time.Hour, RefreshBefore: time.Hour}
	login(unmapped, "alice")
}
