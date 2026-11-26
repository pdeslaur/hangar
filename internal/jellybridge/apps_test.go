package jellybridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestAppCodes(t *testing.T) {
	s, c := openStore(t)
	a := NewApps(s, nil)
	a.now = c.now

	code, expires := a.IssueCode("u1", "Alice")
	if len(code) != appCodeDigits || !expires.Equal(c.t.Add(appCodeTTL)) {
		t.Fatalf("IssueCode = %q, %v", code, expires)
	}
	if _, ok := a.redeem("bob", code); ok {
		t.Error("bob redeemed alice's code")
	}
	got, ok := a.redeem("alice", code)
	if !ok || got.userID != "u1" {
		t.Fatalf("redeem = %+v, %v", got, ok)
	}
	if _, ok := a.redeem("alice", code); ok {
		t.Error("a code redeemed twice")
	}

	// A new code replaces the last.
	first, _ := a.IssueCode("u1", "Alice")
	second, _ := a.IssueCode("u1", "Alice")
	if _, ok := a.redeem("alice", first); ok && first != second {
		t.Error("a replaced code redeemed")
	}

	// Expired.
	code, _ = a.IssueCode("u1", "Alice")
	c.t = c.t.Add(appCodeTTL)
	if _, ok := a.redeem("alice", code); ok {
		t.Error("an expired code redeemed")
	}

	// Guessed: after maxCodeFailures wrong codes, the right one fails too.
	code, _ = a.IssueCode("u1", "Alice")
	for range maxCodeFailures {
		a.redeem("alice", "00000000")
	}
	if _, ok := a.redeem("alice", code); ok {
		t.Error("a code redeemed after too many wrong ones")
	}
	// A new code starts over.
	code, _ = a.IssueCode("u1", "Alice")
	if _, ok := a.redeem("alice", code); !ok {
		t.Error("a new code after too many wrong ones failed")
	}

	// An app sending its used code again, as Infuse does, isn't guessing.
	used := code
	code, _ = a.IssueCode("u1", "Alice")
	for range 2 * maxCodeFailures {
		a.redeem("alice", used)
	}
	if _, ok := a.redeem("alice", code); !ok {
		t.Error("a code voided by an app sending its used code")
	}
}

func TestProxyRefuses(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	s, _ := openStore(t)
	p := NewProxy(target, NewApps(s, nil))
	for _, path := range []string{
		"/Users/u1/Authenticate?pw=secret",
		"/users/U1/authenticate/",
		"/Users/ForgotPassword",
		"/USERS/FORGOTPASSWORD/PIN",
	} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if w.Code != http.StatusForbidden {
			t.Errorf("POST %s: %d", path, w.Code)
		}
	}
	// Jellyfin's password login is the bridge's, whatever the password.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/users/authenticatebyname", strings.NewReader(`{"Username": "alice", "Pw": "alice-password"}`))
	r.Header.Set("Authorization", jellyfin.Device{Client: "Infuse", Name: "Apple TV", ID: "tv1", Version: "8"}.Header(""))
	p.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /users/authenticatebyname: %d", w.Code)
	}
	if reached {
		t.Error("a refused request reached Jellyfin")
	}
}

// call sends a request to url with authorization and a JSON body, if not
// nil, returning the status and the response's body.
func call(t *testing.T, method, url, authorization string, body any) (int, []byte) {
	t.Helper()
	var in io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		in = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, in)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// sso sends a request to the SSO's path as phil, returning the recorder.
func sso(t *testing.T, s *SSO, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(userHeader, "phil")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// TestAppsJellyfin signs apps in through the proxy against a real Jellyfin:
// one with Quick Connect approved on the bridge's page, one with a one-time
// code, and revokes one.
func TestAppsJellyfin(t *testing.T) {
	s := jellyfintest.Start(t)
	ctx := context.Background()
	store, _ := openStore(t)
	store.now = time.Now
	client := &jellyfin.Client{URL: s.URL, Key: jellyfintest.Key, HTTP: http.DefaultClient}
	apps := NewApps(store, client)
	pages := &SSO{
		Apps:       apps,
		ServerID:   "server1",
		UserHeader: userHeader,
		Users:      map[string]string{"phil": "alice"},
		BrowserTTL: 8 * time.Hour,
	}
	target, _ := url.Parse(s.URL)
	proxy := httptest.NewServer(NewProxy(target, apps))
	defer proxy.Close()

	// signedIn checks a login's result: a bridge token for alice.
	signedIn := func(what string, body []byte) string {
		t.Helper()
		var result struct {
			AccessToken string
			User        struct{ Name string }
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("%s: %v: %s", what, err, body)
		}
		if result.User.Name != "alice" {
			t.Errorf("%s: user %q", what, result.User.Name)
		}
		session, err := store.Lookup(ctx, result.AccessToken)
		if err != nil || session.Kind != App {
			t.Fatalf("%s: the token's session: %+v, %v", what, session, err)
		}
		if status, name := jellyfintest.Me(t, proxy.URL, result.AccessToken); status != http.StatusOK || name != "alice" {
			t.Errorf("%s: /Users/Me through the bridge: %d %q", what, status, name)
		}
		return result.AccessToken
	}

	// Quick Connect, approved on the bridge's page.
	tv := jellyfin.Device{Client: "Jellyfin Android TV", Name: "Living room", ID: "tv-1", Version: "0.18"}
	quickConnect := func() string {
		t.Helper()
		status, body := call(t, http.MethodPost, proxy.URL+"/QuickConnect/Initiate", tv.Header(""), nil)
		if status != http.StatusOK {
			t.Fatalf("Initiate: %d %s", status, body)
		}
		var request struct{ Code, Secret string }
		json.Unmarshal(body, &request)
		if w := sso(t, pages, http.MethodPost, "/sso/quickconnect", map[string]string{"Code": request.Code}); w.Code != http.StatusNoContent {
			t.Fatalf("POST /sso/quickconnect: %d %s", w.Code, w.Body)
		}
		status, body = call(t, http.MethodPost, proxy.URL+"/Users/AuthenticateWithQuickConnect", tv.Header(""), map[string]string{"Secret": request.Secret})
		if status != http.StatusOK {
			t.Fatalf("AuthenticateWithQuickConnect: %d %s", status, body)
		}
		return signedIn("Quick Connect", body)
	}
	first := quickConnect()
	if w := sso(t, pages, http.MethodPost, "/sso/quickconnect", map[string]string{"Code": "000000"}); w.Code != http.StatusNotFound {
		t.Errorf("POST /sso/quickconnect with no such code: %d", w.Code)
	}
	// Signing the same TV in again ends its first session.
	second := quickConnect()
	if status, _ := jellyfintest.Me(t, proxy.URL, first); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me with the TV's first token: %d", status)
	}

	// A one-time code, for an app without Quick Connect.
	infuse := jellyfin.Device{Client: "Infuse", Name: "Apple TV", ID: "appletv-1", Version: "8.5.11"}
	w := sso(t, pages, http.MethodPost, "/sso/code", nil)
	var code struct{ Username, Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &code); err != nil || code.Username != "alice" {
		t.Fatalf("POST /sso/code: %d %s", w.Code, w.Body)
	}
	login := func(password string) (int, []byte) {
		return call(t, http.MethodPost, proxy.URL+"/Users/AuthenticateByName", infuse.Header(""),
			map[string]string{"Username": "Alice", "Pw": password})
	}
	if status, _ := login("alice-password"); status != http.StatusUnauthorized {
		t.Errorf("logging in with Jellyfin's password: %d", status)
	}
	status, body := login(code.Code)
	if status != http.StatusOK {
		t.Fatalf("logging in with the code: %d %s", status, body)
	}
	infuseToken := signedIn("one-time code", body)
	if status, _ := login(code.Code); status != http.StatusUnauthorized {
		t.Errorf("logging in with the code again: %d", status)
	}

	// The sessions, and revoking Infuse's.
	w = sso(t, pages, http.MethodGet, "/sso/sessions", nil)
	var sessions []sessionInfo
	json.Unmarshal(w.Body.Bytes(), &sessions)
	var infuseID string
	for _, s := range sessions {
		if s.Client == "Infuse" {
			infuseID = s.ID
		}
	}
	if len(sessions) != 2 || infuseID == "" {
		t.Fatalf("GET /sso/sessions: %s", w.Body)
	}
	if w := sso(t, pages, http.MethodPost, "/sso/revoke", map[string]string{"ID": infuseID}); w.Code != http.StatusNoContent {
		t.Fatalf("POST /sso/revoke: %d %s", w.Code, w.Body)
	}
	if status, _ := jellyfintest.Me(t, proxy.URL, infuseToken); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me with Infuse's token after revoking it: %d", status)
	}
	if w := sso(t, pages, http.MethodPost, "/sso/revoke", map[string]string{"ID": infuseID}); w.Code != http.StatusNotFound {
		t.Errorf("POST /sso/revoke again: %d", w.Code)
	}

	// Signing out through the bridge ends the bridge session, not just
	// Jellyfin's token.
	if status, body := call(t, http.MethodPost, proxy.URL+"/Sessions/Logout", tv.Header(second), nil); status != http.StatusNoContent {
		t.Fatalf("POST /Sessions/Logout: %d %s", status, body)
	}
	if _, err := store.Lookup(ctx, second); err == nil {
		t.Error("the TV's session outlived its logout")
	}

	// A device deleted in Jellyfin: the next request finds its token dead,
	// and the bridge drops its session.
	third := quickConnect()
	if err := call2xx(t, http.MethodDelete, s.URL+"/Devices?id="+tv.ID, jellyfintest.Key); err != "" {
		t.Fatalf("deleting the TV in Jellyfin: %s", err)
	}
	if status, _ := jellyfintest.Me(t, proxy.URL, third); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me after deleting the TV: %d", status)
	}
	if _, err := store.Lookup(ctx, third); err == nil {
		t.Error("the TV's session outlived its device")
	}
}

// call2xx sends a request with Jellyfin's token or key, returning what went
// wrong, if it didn't answer 2xx.
func call2xx(t *testing.T, method, url, token string) string {
	t.Helper()
	status, body := call(t, method, url, `MediaBrowser Token="`+token+`"`, nil)
	if status < 200 || status > 299 {
		return fmt.Sprintf("%d %s", status, body)
	}
	return ""
}
