// Package seerrsso signs Seerr's users in through single sign-on, with
// Seerr's own Jellyfin Quick Connect sign-in, approved by jellyfin-bridge.
//
// The Handler sits between the reverse proxy and Seerr, which it forwards
// everything to but its own paths. The reverse proxy signs users in on
// /sso/* and names them in UserHeader. On GET /sso/login, the Handler
// starts a Quick Connect sign-in in Seerr, has jellyfin-bridge approve its
// code for the user, finishes it, and hands the browser the session cookie
// Seerr set. Seerr's own login page sends the browser there.
package seerrsso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/chainguard-dev/clog"
)

// signedOutCookie marks a browser that signed out of Seerr: its next
// visit to Seerr's login page signs it out of the single sign-on too, at
// /sso/logout, rather than in again.
const signedOutCookie = "seerr-sso-signed-out"

// forwardedHeaders are the ones the reverse proxy sets, which Seerr needs
// to set its secure cookies and log clients' addresses, and which a
// ReverseProxy with Rewrite would drop.
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// Handler signs Seerr's users in, and forwards everything else to Seerr.
type Handler struct {
	// Seerr is Seerr's address, like http://127.0.0.1:5055.
	Seerr *url.URL
	// Bridge is jellyfin-bridge's internal address, like
	// http://jellyfin:8098.
	Bridge string
	// UserHeader is the request header naming the signed-in user, on
	// /sso/*.
	UserHeader string
	// Client calls Seerr and jellyfin-bridge.
	Client *http.Client

	once  sync.Once
	proxy *httputil.ReverseProxy
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.once.Do(func() {
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
			},
			ModifyResponse: markSignedOut,
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
func markSignedOut(resp *http.Response) error {
	r := resp.Request
	if r.Method == http.MethodPost && strings.EqualFold(r.URL.Path, "/api/v1/auth/logout") && resp.StatusCode == http.StatusOK {
		resp.Header.Add("Set-Cookie", (&http.Cookie{Name: signedOutCookie, Value: "1", Path: "/", MaxAge: 300, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}).String())
	}
	return nil
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

// call sends a JSON request, with r's forwarded headers, and decodes the
// JSON answer into out, if any, returning the response, its body closed.
func (h *Handler) call(ctx context.Context, r *http.Request, url string, in, out any, want int) (*http.Response, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Host = r.Host
	req.Header.Set("Content-Type", "application/json")
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

func (h *Handler) serveLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	name := r.Header.Get(h.UserHeader)
	if name == "" {
		http.Error(w, "not signed in", http.StatusForbidden)
		return
	}
	seerr := strings.TrimSuffix(h.Seerr.String(), "/") + "/api/v1/auth/jellyfin/quickconnect/"

	var started struct {
		Code   string `json:"code"`
		Secret string `json:"secret"`
	}
	if _, err := h.call(ctx, r, seerr+"initiate", struct{}{}, &started, http.StatusOK); err != nil {
		clog.ErrorContext(ctx, "starting a Quick Connect sign-in in seerr", "user", name, "error", err)
		http.Error(w, "Seerr couldn't start signing in.", http.StatusBadGateway)
		return
	}

	approve := map[string]string{"Code": started.Code, "User": name}
	if _, err := h.call(ctx, r, strings.TrimSuffix(h.Bridge, "/")+"/bridge/quickconnect", approve, nil, http.StatusNoContent); err != nil {
		if isStatus(err, http.StatusForbidden) {
			clog.WarnContext(ctx, "jellyfin-bridge knows no user", "user", name)
			http.Error(w, "No Jellyfin user for "+name+".", http.StatusForbidden)
			return
		}
		clog.ErrorContext(ctx, "approving seerr's Quick Connect code", "user", name, "error", err)
		http.Error(w, "Jellyfin didn't approve signing in.", http.StatusBadGateway)
		return
	}

	resp, err := h.call(ctx, r, seerr+"authenticate", map[string]string{"secret": started.Secret}, nil, http.StatusOK)
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
	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		// Seerr's session cookie is secure: Seerr sets it only for a
		// request it trusts the X-Forwarded-Proto of.
		clog.ErrorContext(ctx, "seerr set no session cookie: is its proxy support on?", "user", name)
		http.Error(w, "Seerr signed in without a session.", http.StatusBadGateway)
		return
	}
	for _, c := range cookies {
		w.Header().Add("Set-Cookie", c)
	}
	clog.InfoContext(ctx, "signed in to seerr", "user", name)
	http.Redirect(w, r, "/", http.StatusFound)
}
