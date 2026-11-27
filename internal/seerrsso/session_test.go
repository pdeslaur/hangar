package seerrsso

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// sessionCookies returns the session cookie and its limit that w set.
func sessionCookies(t *testing.T, w interface{ Result() *http.Response }) (session, limit *http.Cookie) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case sessionCookie:
			session = c
		case sessionLimitCookie:
			limit = c
		}
	}
	return session, limit
}

func TestSessionLimit(t *testing.T) {
	f := &fakes{seerrUsers: map[string]bool{"alice": true}, cookie: true}
	h := newHandler(t, f)
	h.SessionDuration = 8 * time.Hour
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	h.clock = func() time.Time { return now }

	w := browse(h, http.MethodGet, "/sso/login", "phil")
	session, limit := sessionCookies(t, w)
	if w.Code != http.StatusOK || session == nil || limit == nil {
		t.Fatalf("GET /sso/login: %d, session %v, limit %v", w.Code, session, limit)
	}
	if limit.MaxAge != 8*60*60 || !limit.Secure || !limit.HttpOnly || limit.Path != "/" {
		t.Errorf("limit cookie %v", limit)
	}

	// The Cookie header Seerr receives for a request with cookies.
	seerrGets := func(cookies ...*http.Cookie) string {
		t.Helper()
		other := &http.Cookie{Name: "XSRF-TOKEN", Value: "t0k"}
		if w := browse(h, http.MethodGet, "/api/v1/auth/me", "", append(cookies, other)...); w.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/auth/me: %d", w.Code)
		}
		return f.seen[len(f.seen)-1].Header.Get("Cookie")
	}
	const kept = "connect.sid=abc; XSRF-TOKEN=t0k"

	if got := seerrGets(session, limit); got != kept {
		t.Errorf("within the limit, Seerr got cookies %q", got)
	}
	now = now.Add(8*time.Hour - time.Second)
	if got := seerrGets(session, limit); got != kept {
		t.Errorf("a second before the limit, Seerr got cookies %q", got)
	}

	forged := *limit
	forged.Value = limit.Value[:strings.Index(limit.Value, ".")] + ".AAAA"
	other := *limit
	other.Name, other.Value = sessionCookie, "xyz"
	for _, tc := range []struct {
		name    string
		cookies []*http.Cookie
	}{
		{"no limit", []*http.Cookie{session}},
		{"a forged limit", []*http.Cookie{session, &forged}},
		{"another session's limit", []*http.Cookie{&other, limit}},
		{"a garbled limit", []*http.Cookie{session, {Name: sessionLimitCookie, Value: "garbled"}}},
	} {
		if got := seerrGets(tc.cookies...); got != "XSRF-TOKEN=t0k" {
			t.Errorf("%s: Seerr got cookies %q, want the session's dropped", tc.name, got)
		}
	}

	now = now.Add(time.Second)
	if got := seerrGets(session, limit); got != "XSRF-TOKEN=t0k" {
		t.Errorf("past the limit, Seerr got cookies %q, want the session's dropped", got)
	}
	// Signing in again makes a limit for the new session.
	w = browse(h, http.MethodGet, "/sso/login", "phil")
	session, limit = sessionCookies(t, w)
	if got := seerrGets(session, limit); got != kept {
		t.Errorf("signed in again, Seerr got cookies %q, want %q", got, kept)
	}
}

func TestSessionLimitSeerrSignIn(t *testing.T) {
	f := &fakes{}
	h := newHandler(t, f)
	h.SessionDuration = time.Hour

	// Seerr's own sign-in, at /login?local, gets a limit too.
	w := browse(h, http.MethodPost, "/api/v1/auth/local", "")
	session, limit := sessionCookies(t, w)
	if session == nil || session.Value != "s%3Alocal.sig" || limit == nil {
		t.Fatalf("POST /api/v1/auth/local: session %v, limit %v", session, limit)
	}
	browse(h, http.MethodGet, "/api/v1/auth/me", "", session, limit)
	if got := f.seen[len(f.seen)-1].Header.Get("Cookie"); got != "connect.sid=s%3Alocal.sig" {
		t.Errorf("Seerr got cookies %q", got)
	}
	// Other responses get none.
	if _, limit := sessionCookies(t, browse(h, http.MethodGet, "/api/v1/auth/me", "", session, limit)); limit != nil {
		t.Errorf("GET /api/v1/auth/me set %v", limit)
	}
}

func TestSessionUnlimited(t *testing.T) {
	f := &fakes{seerrUsers: map[string]bool{"alice": true}, cookie: true}
	h := newHandler(t, f)
	w := browse(h, http.MethodGet, "/sso/login", "phil")
	session, limit := sessionCookies(t, w)
	if session == nil || limit != nil {
		t.Fatalf("GET /sso/login: session %v, limit %v", session, limit)
	}
	browse(h, http.MethodGet, "/api/v1/auth/me", "", session)
	if got := f.seen[len(f.seen)-1].Header.Get("Cookie"); got != "connect.sid=abc" {
		t.Errorf("Seerr got cookies %q", got)
	}
}
