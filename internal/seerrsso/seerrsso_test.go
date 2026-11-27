package seerrsso

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const userHeader = "X-SSO-User"

// fakes is a fake Seerr and jellyfin-sso, where a Quick Connect code is
// approved for alice alone.
type fakes struct {
	mu sync.Mutex
	// approved is whether jellyfin-sso approved Seerr's code.
	approved bool
	// seerrUsers are the users Seerr knows.
	seerrUsers map[string]bool
	// cookie is whether Seerr sets its session cookie.
	cookie bool
	// csrf is whether Seerr's CSRF protection is on.
	csrf bool
	// user is who Seerr signed in.
	user string
	// seen are the requests Seerr received, with their headers.
	seen []*http.Request
}

func (f *fakes) handler(t *testing.T) (*url.URL, string) {
	t.Helper()
	seerr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seen = append(f.seen, r)
		if f.csrf {
			// Like csurf: the secret in _csrf, the token, URL-encoded by
			// Express, in XSRF-TOKEN, on every response; a POST carries
			// both.
			if r.Method == http.MethodPost {
				secret, err := r.Cookie("_csrf")
				if err != nil || secret.Value != "s%2Fcr3t" || r.Header.Get("X-XSRF-TOKEN") != "t0k/en" {
					http.Error(w, `{"message":"invalid csrf token"}`, http.StatusForbidden)
					return
				}
			} else {
				w.Header().Add("Set-Cookie", "_csrf=s%2Fcr3t; Path=/; HttpOnly; Secure; SameSite=Strict")
			}
			w.Header().Add("Set-Cookie", "XSRF-TOKEN=t0k%2Fen; Path=/; Secure; SameSite=Strict")
		}
		switch r.URL.Path {
		case "/api/v1/auth/jellyfin/quickconnect/initiate":
			io.WriteString(w, `{"code":"123456","secret":"s3cret"}`)
		case "/api/v1/auth/jellyfin/quickconnect/authenticate":
			var body struct{ Secret string }
			json.NewDecoder(r.Body).Decode(&body)
			if body.Secret != "s3cret" || !f.approved {
				http.Error(w, `{"message":"InvalidCredentials"}`, http.StatusUnauthorized)
				return
			}
			if !f.seerrUsers["alice"] {
				http.Error(w, `{"message":"Access denied."}`, http.StatusForbidden)
				return
			}
			f.user = "alice"
			if f.cookie {
				w.Header().Add("Set-Cookie", "connect.sid=abc; Path=/; HttpOnly; Secure")
			}
			io.WriteString(w, `{"id":1}`)
		case "/api/v1/auth/logout":
			io.WriteString(w, `{"status":"ok"}`)
		case "/api/v1/auth/local":
			// Seerr's own sign-in, which signs anyone in.
			w.Header().Add("Set-Cookie", "connect.sid=s%3Alocal.sig; Path=/; HttpOnly; Secure")
			io.WriteString(w, `{"id":2}`)
		default:
			io.WriteString(w, "seerr "+r.URL.Path)
		}
	}))
	t.Cleanup(seerr.Close)
	jsso := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct{ Code, User string }
		if r.Method != http.MethodPost || r.URL.Path != "/internal/quickconnect" || json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.User != "phil" {
			http.Error(w, "no Jellyfin user", http.StatusForbidden)
			return
		}
		if body.Code != "123456" {
			http.NotFound(w, r)
			return
		}
		f.approved = true
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(jsso.Close)
	u, _ := url.Parse(seerr.URL)
	return u, jsso.URL
}

func newHandler(t *testing.T, f *fakes) *Handler {
	seerr, jsso := f.handler(t)
	return &Handler{Seerr: seerr, JellyfinSSO: jsso, UserHeader: userHeader, Client: http.DefaultClient}
}

// browse sends a request through h, from the reverse proxy, as user.
func browse(h http.Handler, method, path, user string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://seerr.example.com"+path, nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if user != "" {
		r.Header.Set(userHeader, user)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLogin(t *testing.T) {
	for _, csrf := range []bool{false, true} {
		t.Run(fmt.Sprintf("csrf %t", csrf), func(t *testing.T) {
			f := &fakes{seerrUsers: map[string]bool{"alice": true}, cookie: true, csrf: csrf}
			h := newHandler(t, f)
			w := browse(h, http.MethodGet, "/sso/login", "phil")
			// A page sending the browser on, for Seerr's SameSite=Strict
			// session cookie.
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `<meta http-equiv="refresh" content="0;url=/">`) {
				t.Fatalf("GET /sso/login: %d: %s", w.Code, w.Body)
			}
			// Seerr's session cookie alone, not its CSRF ones.
			if got := w.Header().Values("Set-Cookie"); len(got) != 1 || !strings.HasPrefix(got[0], "connect.sid=abc;") {
				t.Errorf("cookies %q, want Seerr's session", got)
			}
			if f.user != "alice" {
				t.Errorf("Seerr signed in %q", f.user)
			}
			// Seerr trusts the proxy's headers, for its secure cookie and its
			// logs.
			for _, r := range f.seen {
				if r.Header.Get("X-Forwarded-Proto") != "https" || r.Header.Get("X-Forwarded-For") != "203.0.113.7" || r.Host != "seerr.example.com" {
					t.Errorf("%s reached Seerr with %q, %q, host %q", r.URL.Path, r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-For"), r.Host)
				}
			}
		})
	}
}

func TestLoginFails(t *testing.T) {
	for _, tc := range []struct {
		name, user string
		seerrUsers map[string]bool
		cookie     bool
		want       int
	}{
		{"no user", "", map[string]bool{"alice": true}, true, http.StatusForbidden},
		{"a user jellyfin-sso doesn't know", "mallory", map[string]bool{"alice": true}, true, http.StatusForbidden},
		{"a user Seerr doesn't know", "phil", nil, true, http.StatusForbidden},
		{"no session cookie", "phil", map[string]bool{"alice": true}, false, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakes{seerrUsers: tc.seerrUsers, cookie: tc.cookie}
			h := newHandler(t, f)
			w := browse(h, http.MethodGet, "/sso/login", tc.user)
			if w.Code != tc.want || w.Header().Get("Set-Cookie") != "" {
				t.Errorf("status %d, want %d, cookies %q: %s", w.Code, tc.want, w.Header().Values("Set-Cookie"), w.Body)
			}
		})
	}
	// No session cookie, among Seerr's CSRF ones.
	h := newHandler(t, &fakes{seerrUsers: map[string]bool{"alice": true}, csrf: true})
	if w := browse(h, http.MethodGet, "/sso/login", "phil"); w.Code != http.StatusBadGateway || w.Header().Get("Set-Cookie") != "" {
		t.Errorf("no session cookie, CSRF on: status %d, cookies %q", w.Code, w.Header().Values("Set-Cookie"))
	}
	// Seerr unreachable.
	h = &Handler{Seerr: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}, JellyfinSSO: "http://127.0.0.1:1", UserHeader: userHeader, Client: http.DefaultClient}
	if w := browse(h, http.MethodGet, "/sso/login", "phil"); w.Code != http.StatusBadGateway {
		t.Errorf("Seerr down: status %d", w.Code)
	}
}

func TestLoginPageAndLogout(t *testing.T) {
	f := &fakes{}
	h := newHandler(t, f)
	if w := browse(h, http.MethodGet, "/login", ""); w.Code != http.StatusFound || w.Header().Get("Location") != "/sso/login" {
		t.Errorf("GET /login: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if w := browse(h, http.MethodGet, "/login?local", ""); w.Code != http.StatusOK || w.Body.String() != "seerr /login" {
		t.Errorf("GET /login?local: %d %s", w.Code, w.Body)
	}

	// Signing out of Seerr signs out of the single sign-on, once.
	logout := browse(h, http.MethodPost, "/api/v1/auth/logout", "")
	var signedOut *http.Cookie
	for _, c := range logout.Result().Cookies() {
		if c.Name == signedOutCookie {
			signedOut = c
		}
	}
	if logout.Code != http.StatusOK || signedOut == nil || signedOut.Value != "1" || !signedOut.Secure || !signedOut.HttpOnly {
		t.Fatalf("POST /api/v1/auth/logout: %d, cookie %v", logout.Code, signedOut)
	}
	w := browse(h, http.MethodGet, "/login", "", signedOut)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/sso/logout" {
		t.Errorf("GET /login, signed out: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].Name != signedOutCookie || c[0].MaxAge >= 0 {
		t.Errorf("GET /login, signed out: cookies %v, want %s cleared", c, signedOutCookie)
	}
	if w := browse(h, http.MethodGet, "/sso/logout", ""); w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Errorf("GET /sso/logout: %d to %q", w.Code, w.Header().Get("Location"))
	}
}

func TestProxy(t *testing.T) {
	f := &fakes{}
	h := newHandler(t, f)
	w := browse(h, http.MethodGet, "/api/v1/settings/public", "phil")
	if w.Code != http.StatusOK || w.Body.String() != "seerr /api/v1/settings/public" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r := f.seen[len(f.seen)-1]
	if r.Header.Get(userHeader) != "" || r.Header.Get("X-Forwarded-Proto") != "https" || r.Host != "seerr.example.com" {
		t.Errorf("Seerr received user %q, proto %q, host %q", r.Header.Get(userHeader), r.Header.Get("X-Forwarded-Proto"), r.Host)
	}
	// Another path than logout sets no cookie.
	if w := browse(h, http.MethodPost, "/api/v1/auth/me", ""); len(w.Result().Cookies()) != 0 {
		t.Errorf("cookies %v", w.Result().Cookies())
	}
}
