// Package seerrsso signs Seerr's users in through single sign-on, with
// Seerr's own Jellyfin Quick Connect sign-in, approved by jellyfin-sso.
//
// The Handler sits between the reverse proxy and Seerr, which it forwards
// everything to but its own paths. The reverse proxy signs users in on
// /sso/* and names them in UserHeader. On GET /sso/login, the Handler
// starts a Quick Connect sign-in in Seerr, has jellyfin-sso approve its
// code for the user, finishes it, and hands the browser the session cookie
// Seerr set, its own calls carrying a CSRF token when Seerr's CSRF
// protection is on. Seerr's own login page sends the browser there. With a
// SessionDuration, the Handler ends Seerr's sessions sooner than Seerr.
package seerrsso

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chainguard-dev/clog"
)

// signedOutCookie marks a browser that signed out of Seerr: its next
// visit to Seerr's login page signs it out of the single sign-on too, at
// /sso/logout, rather than in again.
const signedOutCookie = "seerr-sso-signed-out"

const (
	// sessionCookie is Seerr's session cookie.
	sessionCookie = "connect.sid"
	// csrfCookie holds Seerr's CSRF secret, csrfTokenCookie a token made
	// from it, which a POST carries in csrfTokenHeader.
	csrfCookie      = "_csrf"
	csrfTokenCookie = "XSRF-TOKEN"
	csrfTokenHeader = "X-XSRF-TOKEN"
)

// forwardedHeaders are the ones the reverse proxy sets, which Seerr needs
// to set its secure cookies and log clients' addresses, and which a
// ReverseProxy with Rewrite would drop.
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// Handler signs Seerr's users in, and forwards everything else to Seerr.
type Handler struct {
	// Seerr is Seerr's address, like http://127.0.0.1:5055.
	Seerr *url.URL
	// JellyfinSSO is jellyfin-sso's internal address, like
	// http://jellyfin:8098.
	JellyfinSSO string
	// UserHeader is the request header naming the signed-in user, on
	// /sso/*.
	UserHeader string
	// Client calls Seerr and jellyfin-sso.
	Client *http.Client
	// SessionDuration, if positive, limits how long a Seerr session lasts,
	// from its sign-in, rather than Seerr's 30 days: past it, the Handler
	// leaves the session cookie out of the requests it forwards, and Seerr
	// sends the browser to sign in again.
	SessionDuration time.Duration

	once  sync.Once
	proxy *httputil.ReverseProxy
	// key signs sessionLimitCookie: random, so a restart signs everyone in
	// again.
	key []byte
	// clock is the time, time.Now unless set by a test.
	clock func() time.Time
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.once.Do(func() {
		h.key = make([]byte, 32)
		rand.Read(h.key)
		h.proxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(h.Seerr)
				pr.Out.Host = pr.In.Host
				pr.Out.Header.Del(h.UserHeader)
				for _, name := range forwardedHeaders {
					if v := pr.In.Header.Values(name); len(v) > 0 {
						pr.Out.Header[name] = v
					}
				}
				// The limit is the Handler's alone, and past it, the session
				// is as good as gone.
				drop := []string{sessionLimitCookie}
				if h.SessionDuration > 0 && !h.sessionValid(pr.In) {
					drop = append(drop, sessionCookie)
				}
				dropCookies(pr.Out.Header, drop...)
			},
			ModifyResponse: func(resp *http.Response) error {
				markSignedOut(resp)
				h.limitSessions(resp.Header)
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				clog.WarnContext(r.Context(), "forwarding to seerr", "error", err)
				w.WriteHeader(http.StatusBadGateway)
			},
		}
	})
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case get && r.URL.Path == "/sso/login":
		h.serveLogin(w, r)
	case get && r.URL.Path == "/sso/logout":
		// The reverse proxy signs out here, if it can.
		http.Redirect(w, r, "/", http.StatusFound)
	case get && r.URL.Path == "/login" && !r.URL.Query().Has("local"):
		// Seerr's login page, where it sends a browser without a session,
		// unless asked for its own with ?local, as for a user of Seerr's
		// alone.
		serveLoginPage(w, r)
	default:
		h.proxy.ServeHTTP(w, r)
	}
}

// serveLoginPage sends the browser to sign in through the single sign-on,
// or, once it signed out of Seerr, out of it.
func serveLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if c, err := r.Cookie(signedOutCookie); err == nil && c.Value == "1" {
		http.SetCookie(w, &http.Cookie{Name: signedOutCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/sso/logout", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/sso/login", http.StatusFound)
}

// markSignedOut marks the browser that signed out of Seerr with
// signedOutCookie.
func markSignedOut(resp *http.Response) {
	r := resp.Request
	if r.Method == http.MethodPost && strings.EqualFold(r.URL.Path, "/api/v1/auth/logout") && resp.StatusCode == http.StatusOK {
		resp.Header.Add("Set-Cookie", (&http.Cookie{Name: signedOutCookie, Value: "1", Path: "/", MaxAge: 300, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}).String())
	}
}

// statusError is a call answered with a status other than the one wanted.
type statusError struct {
	url    string
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s: %d %s", e.url, e.status, e.body)
}

// call sends a request, with r's forwarded headers and header, and the
// JSON of in, if any, and decodes the JSON answer into out, if any,
// returning the response, its body closed.
func (h *Handler) call(ctx context.Context, r *http.Request, method, url string, header http.Header, in, out any, want int) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Host = r.Host
	maps.Copy(req.Header, header)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for _, name := range forwardedHeaders {
		if v := r.Header.Values(name); len(v) > 0 {
			req.Header[name] = v
		}
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &statusError{url, resp.StatusCode, string(b)}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, fmt.Errorf("%s: %w", url, err)
		}
	}
	return resp, nil
}

// isStatus reports whether err is a call answered with status.
func isStatus(err error, status int) bool {
	var e *statusError
	return errors.As(err, &e) && e.status == status
}

// csrf returns the headers Seerr's POSTs need when its CSRF protection is
// on, none otherwise. Seerr then sets a secret, in csrfCookie, and a token
// made from it, in csrfTokenCookie, on every response, and a POST has to
// carry both: the secret as its cookie, the token in csrfTokenHeader.
func (h *Handler) csrf(ctx context.Context, r *http.Request) (http.Header, error) {
	resp, err := h.call(ctx, r, http.MethodGet, h.seerrURL("/api/v1/status"), nil, nil, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	for _, c := range resp.Cookies() {
		switch c.Name {
		case csrfCookie:
			header.Set("Cookie", (&http.Cookie{Name: c.Name, Value: c.Value}).String())
		case csrfTokenCookie:
			// Express URL-encodes cookie values.
			token, err := url.QueryUnescape(c.Value)
			if err != nil {
				return nil, fmt.Errorf("%s cookie: %w", csrfTokenCookie, err)
			}
			header.Set(csrfTokenHeader, token)
		}
	}
	return header, nil
}

func (h *Handler) seerrURL(path string) string {
	return strings.TrimSuffix(h.Seerr.String(), "/") + path
}

func (h *Handler) serveLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	name := r.Header.Get(h.UserHeader)
	if name == "" {
		http.Error(w, "not signed in", http.StatusForbidden)
		return
	}
	quickConnect := h.seerrURL("/api/v1/auth/jellyfin/quickconnect/")

	csrf, err := h.csrf(ctx, r)
	if err != nil {
		clog.ErrorContext(ctx, "reaching seerr", "user", name, "error", err)
		http.Error(w, "Seerr couldn't start signing in.", http.StatusBadGateway)
		return
	}

	var started struct {
		Code   string `json:"code"`
		Secret string `json:"secret"`
	}
	if _, err := h.call(ctx, r, http.MethodPost, quickConnect+"initiate", csrf, struct{}{}, &started, http.StatusOK); err != nil {
		clog.ErrorContext(ctx, "starting a Quick Connect sign-in in seerr", "user", name, "error", err)
		http.Error(w, "Seerr couldn't start signing in.", http.StatusBadGateway)
		return
	}

	approve := map[string]string{"Code": started.Code, "User": name}
	if _, err := h.call(ctx, r, http.MethodPost, strings.TrimSuffix(h.JellyfinSSO, "/")+"/internal/quickconnect", nil, approve, nil, http.StatusNoContent); err != nil {
		if isStatus(err, http.StatusForbidden) {
			clog.WarnContext(ctx, "jellyfin-sso knows no user", "user", name)
			http.Error(w, "No Jellyfin user for "+name+".", http.StatusForbidden)
			return
		}
		clog.ErrorContext(ctx, "approving seerr's Quick Connect code", "user", name, "error", err)
		http.Error(w, "Jellyfin didn't approve signing in.", http.StatusBadGateway)
		return
	}

	resp, err := h.call(ctx, r, http.MethodPost, quickConnect+"authenticate", csrf, map[string]string{"secret": started.Secret}, nil, http.StatusOK)
	if isStatus(err, http.StatusForbidden) {
		clog.WarnContext(ctx, "seerr refused a user", "user", name, "error", err)
		http.Error(w, "Seerr has no user for "+name+": an admin imports them from Jellyfin, or enables new Jellyfin sign-ins.", http.StatusForbidden)
		return
	}
	if err != nil {
		clog.ErrorContext(ctx, "finishing a Quick Connect sign-in in seerr", "user", name, "error", err)
		http.Error(w, "Seerr couldn't finish signing in.", http.StatusBadGateway)
		return
	}
	// The session cookie alone: Seerr's CSRF cookies are seerr-sso's own.
	var session []string
	for _, line := range resp.Header.Values("Set-Cookie") {
		if c, err := http.ParseSetCookie(line); err == nil && c.Name == sessionCookie {
			session = append(session, line)
		}
	}
	if len(session) == 0 {
		// Seerr's session cookie is secure: Seerr sets it only for a
		// request it trusts the X-Forwarded-Proto of.
		clog.ErrorContext(ctx, "seerr set no session cookie: is its proxy support on?", "user", name)
		http.Error(w, "Seerr signed in without a session.", http.StatusBadGateway)
		return
	}
	for _, c := range session {
		w.Header().Add("Set-Cookie", c)
	}
	h.limitSessions(w.Header())
	clog.InfoContext(ctx, "signed in to seerr", "user", name)
	// A page sending the browser on, not a redirect: the browser came
	// through redirects from the identity provider, another site, and
	// would leave out a SameSite=Strict session cookie, Seerr's with its
	// CSRF protection on, on one more. A page's own navigation is
	// same-site.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, signedInPage)
}

// signedInPage sends the browser to Seerr's home page.
const signedInPage = `<!doctype html>
<meta charset="utf-8">
<meta http-equiv="refresh" content="0;url=/">
<title>Seerr</title>
<a href="/">Continue to Seerr</a>
`
