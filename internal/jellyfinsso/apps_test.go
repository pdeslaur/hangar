package jellyfinsso

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	// Jellyfin's password login is jellyfin-sso's, whatever the password.
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

// sso sends a request to the SSO's path as phil, posting form, if any, as
// a form of the same site does.
func sso(t *testing.T, s *SSO, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set(userHeader, "phil")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// issuedCodeHTML finds the username and the one-time code on the account
// page.
var issuedCodeHTML = regexp.MustCompile(`Username: (\w+)</div>\s*<div class="code">(\d+)</div>`)

// sessionRowHTML finds the app and the ID of each session the account page
// lists.
var sessionRowHTML = regexp.MustCompile(`<td>([^<(]+?)(?: \(browser\))?</td>\s*<td>[^<]*</td>\s*<td>\s*<form method="post" action="/sso/revoke"[^>]*>\s*<input type="hidden" name="id" value="([^"]+)"`)

// TestAppsJellyfin signs apps in through the proxy against a real Jellyfin:
// with Quick Connect approved on jellyfin-sso's page and on jellyfin-web's,
// and with a one-time code, and revokes one.
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

	// signedIn checks a login's result: a jellyfin-sso token for alice.
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
			t.Errorf("%s: /Users/Me through jellyfin-sso: %d %q", what, status, name)
		}
		return result.AccessToken
	}

	// Quick Connect, approved on jellyfin-sso's page, or by approve.
	tv := jellyfin.Device{Client: "Jellyfin Android TV", Name: "Living room", ID: "tv-1", Version: "0.18"}
	onPage := func(code string) {
		t.Helper()
		if w := sso(t, pages, http.MethodPost, "/sso/quickconnect", url.Values{"code": {code}}); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Signed in.") {
			t.Fatalf("POST /sso/quickconnect: %d %s", w.Code, w.Body)
		}
	}
	quickConnectWith := func(approve func(code string)) string {
		t.Helper()
		status, body := call(t, http.MethodPost, proxy.URL+"/QuickConnect/Initiate", tv.Header(""), nil)
		if status != http.StatusOK {
			t.Fatalf("Initiate: %d %s", status, body)
		}
		var request struct{ Code, Secret string }
		json.Unmarshal(body, &request)
		approve(request.Code)
		status, body = call(t, http.MethodPost, proxy.URL+"/Users/AuthenticateWithQuickConnect", tv.Header(""), map[string]string{"Secret": request.Secret})
		if status != http.StatusOK {
			t.Fatalf("AuthenticateWithQuickConnect: %d %s", status, body)
		}
		return signedIn("Quick Connect", body)
	}
	quickConnect := func() string { return quickConnectWith(onPage) }
	first := quickConnect()
	if w := sso(t, pages, http.MethodPost, "/sso/quickconnect", url.Values{"code": {"000000"}}); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no app is showing that code") {
		t.Errorf("POST /sso/quickconnect with no such code: %d %s", w.Code, w.Body)
	}

	// Quick Connect approved on jellyfin-web's own page, through the proxy
	// with a browser's token.
	w := browse(t, pages, http.MethodGet, "/sso/login", "phil", nil, nil)
	m := storedSession.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || m == nil {
		t.Fatalf("GET /sso/login: %d %s", w.Code, w.Body)
	}
	var browser sessionResponse
	json.Unmarshal([]byte(m[1]), &browser)
	web := browserDevice
	web.ID = browser.DeviceID
	quickConnectWith(func(code string) {
		t.Helper()
		if err := call2xxAs(t, http.MethodPost, proxy.URL+"/QuickConnect/Authorize?code="+code, web.Header(browser.AccessToken)); err != "" {
			t.Fatalf("approving the code in jellyfin-web: %s", err)
		}
	})
	// Signing the same TV in again ends its first session.
	second := quickConnect()
	if status, _ := jellyfintest.Me(t, proxy.URL, first); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me with the TV's first token: %d", status)
	}

	// A one-time code, for an app without Quick Connect.
	infuse := jellyfin.Device{Client: "Infuse", Name: "Apple TV", ID: "appletv-1", Version: "8.5.11"}
	w = sso(t, pages, http.MethodPost, "/sso/code", nil)
	m = issuedCodeHTML.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || m == nil || m[1] != "alice" {
		t.Fatalf("POST /sso/code: %d %s", w.Code, w.Body)
	}
	code := struct{ Username, Code string }{m[1], m[2]}
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
	w = sso(t, pages, http.MethodGet, "/sso/", nil)
	listed := sessionRowHTML.FindAllStringSubmatch(w.Body.String(), -1)
	var infuseID string
	for _, row := range listed {
		if row[1] == "Infuse" {
			infuseID = row[2]
		}
	}
	// The TV, the browser and Infuse.
	if len(listed) != 3 || infuseID == "" {
		t.Fatalf("GET /sso/: %v in %s", listed, w.Body)
	}
	if w := sso(t, pages, http.MethodPost, "/sso/revoke", url.Values{"id": {infuseID}}); w.Code != http.StatusOK || strings.Contains(w.Body.String(), infuseID) {
		t.Fatalf("POST /sso/revoke: %d %s", w.Code, w.Body)
	}
	if status, _ := jellyfintest.Me(t, proxy.URL, infuseToken); status != http.StatusUnauthorized {
		t.Errorf("/Users/Me with Infuse's token after revoking it: %d", status)
	}
	if w := sso(t, pages, http.MethodPost, "/sso/revoke", url.Values{"id": {infuseID}}); w.Code != http.StatusNotFound {
		t.Errorf("POST /sso/revoke again: %d", w.Code)
	}

	// Signing out through jellyfin-sso ends jellyfin-sso session, not just
	// Jellyfin's token; an app's, without the cookie a browser's gets.
	logout := func(what string, device jellyfin.Device, token string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/Sessions/Logout", nil)
		req.Header.Set("Authorization", device.Header(token))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s: POST /Sessions/Logout: %d", what, resp.StatusCode)
		}
		if _, err := store.Lookup(ctx, token); err == nil {
			t.Errorf("%s: the session outlived its logout", what)
		}
		return resp
	}
	signedOut := func(resp *http.Response) bool {
		for _, c := range resp.Cookies() {
			if c.Name == signedOutCookie && c.Value == "1" && c.Path == "/" && !c.HttpOnly {
				return true
			}
		}
		return false
	}
	if resp := logout("the TV", tv, second); signedOut(resp) {
		t.Error("an app's logout set the signed-out cookie")
	}
	if resp := logout("the browser", web, browser.AccessToken); !signedOut(resp) {
		t.Errorf("a browser's logout set cookies %v", resp.Cookies())
	}

	// A device deleted in Jellyfin: the next request finds its token dead,
	// and jellyfin-sso drops its session.
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
	return call2xxAs(t, method, url, `MediaBrowser Token="`+token+`"`)
}

// call2xxAs sends a request with the Authorization header authorization,
// returning what went wrong, if it didn't answer 2xx.
func call2xxAs(t *testing.T, method, url, authorization string) string {
	t.Helper()
	status, body := call(t, method, url, authorization, nil)
	if status < 200 || status > 299 {
		return fmt.Sprintf("%d %s", status, body)
	}
	return ""
}
