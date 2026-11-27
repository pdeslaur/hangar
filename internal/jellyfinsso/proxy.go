// Package jellyfinsso puts Jellyfin behind single sign-on: it hands clients
// tokens of its own, and forwards their requests to Jellyfin with the
// Jellyfin token of each one's session, which never leaves it.
package jellyfinsso

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/jellyfin"
)

// forwardedHeaders are the ones Caddy sets in front of jellyfin-sso, which a
// ReverseProxy with Rewrite would drop.
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// Proxy forwards requests to Jellyfin, swapping each jellyfin-sso token for its
// session's Jellyfin token.
//
// It removes every token from a request, wherever Jellyfin would read one,
// and sends Jellyfin the Jellyfin token of the session whose jellyfin-sso token
// it found, in an Authorization header, or none: a Jellyfin token sent to
// jellyfin-sso, an unknown or an expired one, reaches Jellyfin as no token at
// all. Jellyfin gets the device's ID from the session, and the rest of the
// device's description from the client when it gives one.
//
// It adds jellyfin-sso's script, /sso-static/sso.js, to jellyfin-web's
// page, and a link to its account page, My devices, to jellyfin-web's menu.
//
// Jellyfin's logins go through Apps: a password login is jellyfin-sso's, the
// result of a Quick Connect login gets a jellyfin-sso token in place of
// Jellyfin's, and the obsolete password login and password resets are
// refused.
type Proxy struct {
	apps  *Apps
	proxy *httputil.ReverseProxy
}

// NewProxy returns a Proxy to the Jellyfin at target, its sessions in
// apps.Sessions and its apps signed in by apps.
func NewProxy(target *url.URL, apps *Apps) *Proxy {
	p := &Proxy{apps: apps}
	p.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			for _, h := range forwardedHeaders {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
		},
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			clog.WarnContext(r.Context(), "forwarding to jellyfin", "error", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return p
}

// route is what jellyfin-sso does with a request to Jellyfin.
type route int

const (
	// forward sends it to Jellyfin.
	forward route = iota
	// webPage is jellyfin-web's page, to add the script to.
	webPage
	// webConfig is jellyfin-web's config.json, to add the menu link to.
	webConfig
	// passwordLogin is Jellyfin's password login, which Apps answers.
	passwordLogin
	// quickConnectLogin is Jellyfin's Quick Connect login, whose result
	// Apps adopts.
	quickConnectLogin
	// logout is Jellyfin's logout, which ends jellyfin-sso session.
	logout
	// refused never reaches Jellyfin.
	refused
)

// userAuthenticate is Jellyfin's obsolete password login, by user ID.
var userAuthenticate = regexp.MustCompile(`^/users/[^/]+/authenticate$`)

// routeOf returns the route of r, matching its path as ASP.NET does,
// without case or a trailing slash.
func routeOf(r *http.Request) route {
	path := strings.TrimSuffix(strings.ToLower(r.URL.Path), "/")
	switch {
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && (r.URL.Path == "/web/" || r.URL.Path == "/web/index.html"):
		return webPage
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/web/config.json":
		return webConfig
	case path == "/users/authenticatebyname":
		return passwordLogin
	case path == "/users/authenticatewithquickconnect":
		return quickConnectLogin
	case path == "/sessions/logout":
		return logout
	case path == "/users/forgotpassword", path == "/users/forgotpassword/pin", userAuthenticate.MatchString(path):
		return refused
	}
	return forward
}

// Context keys of a request to Jellyfin: its route, and the session it
// carries the token of.
type (
	routeKey   struct{}
	sessionKey struct{}
)

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rt := routeOf(r)
	ctx = context.WithValue(ctx, routeKey{}, rt)
	r = r.Clone(ctx)
	switch rt {
	case webPage, webConfig:
		// Whole and uncompressed, for addScript and addMenuLink to change: a
		// browser may hold a copy from before jellyfin-sso, which Jellyfin
		// would let it keep.
		for _, h := range []string{"Accept-Encoding", "If-None-Match", "If-Modified-Since"} {
			r.Header.Del(h)
		}
	case quickConnectLogin:
		// Uncompressed, for adoptLogin to change.
		r.Header.Del("Accept-Encoding")
	case refused:
		http.Error(w, "signing in is through single sign-on", http.StatusForbidden)
		return
	case passwordLogin:
		p.apps.ServePasswordLogin(w, r, takeCredentials(r).device)
		return
	}
	c := takeCredentials(r)
	session, err := p.apps.Sessions.Lookup(ctx, c.token)
	switch {
	case err == nil:
		if err := p.apps.Sessions.Touch(ctx, session); err != nil {
			clog.WarnContext(ctx, "recording a session's use", "session", session.ID, "error", err)
		}
		if rt == logout {
			p.serveLogout(w, r, session)
			return
		}
		device := session.Device
		device.Client = cmp.Or(c.device.Client, device.Client)
		device.Name = cmp.Or(c.device.Name, device.Name)
		device.Version = cmp.Or(c.device.Version, device.Version)
		r.Header.Set("Authorization", device.Header(session.JellyfinToken))
		r = r.WithContext(context.WithValue(ctx, sessionKey{}, session))
	case errors.Is(err, errNoSession):
		if c.token != "" {
			p.logRefused(ctx, r, c.token)
		}
		// Anonymous, still naming its device: Quick Connect needs it.
		if c.device != (jellyfin.Device{}) {
			r.Header.Set("Authorization", c.device.Header(""))
		}
	default:
		clog.ErrorContext(ctx, "looking up a session", "error", err)
		http.Error(w, "session store unavailable", http.StatusServiceUnavailable)
		return
	}
	p.proxy.ServeHTTP(w, r)
}

// scriptTag adds jellyfin-sso's script to jellyfin-web.
const scriptTag = `<script src="/sso-static/sso.js"></script>`

// maxWebPage bounds the size of jellyfin-web's page that addScript reads.
const maxWebPage = 1 << 20

// addScript adds scriptTag to jellyfin-web's page, before its </head>. The
// page it changes is uncached, so that no browser keeps a copy
// without the script. A page it can't change goes on as it is: jellyfin-web
// works without the script, until its token expires.
func addScript(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || resp.Request.Method != http.MethodGet {
		return nil
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxWebPage+1))
	if err != nil {
		return err
	}
	head := bytes.Index(page, []byte("</head>"))
	if len(page) > maxWebPage || head < 0 {
		clog.WarnContext(resp.Request.Context(), "can't add jellyfin-sso's script to jellyfin-web's page", "size", len(page))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(page), resp.Body), resp.Body}
		return nil
	}
	resp.Body.Close()
	setBody(resp, slices.Concat(page[:head], []byte(scriptTag), page[head:]))
	uncached(resp)
	return nil
}

// setBody replaces the body of resp with b.
func setBody(resp *http.Response, b []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(b))
	resp.ContentLength = int64(len(b))
	resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
}

// maxLoginResponse bounds the size of a login's result that adoptLogin
// reads.
const maxLoginResponse = 1 << 20

// modifyResponse changes Jellyfin's responses on the routes that need it.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	if session, ok := resp.Request.Context().Value(sessionKey{}).(Session); ok && resp.StatusCode == http.StatusUnauthorized {
		p.checkSession(resp.Request.Context(), session)
	}
	switch resp.Request.Context().Value(routeKey{}) {
	case webPage:
		return addScript(resp)
	case webConfig:
		return addMenuLink(resp)
	case quickConnectLogin:
		return p.adoptLogin(resp)
	}
	return nil
}

// adoptLogin gives an app a jellyfin-sso token in place of the Jellyfin token of
// a Quick Connect login's result. A result it can't adopt fails the
// request, rather than reaching the app with Jellyfin's token.
func (p *Proxy) adoptLogin(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		return fmt.Errorf("a login's result encoded as %s", enc)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLoginResponse))
	resp.Body.Close()
	if err != nil {
		return err
	}
	result, err := jellyfin.ParseAuthenticationResult(raw)
	if err != nil {
		return err
	}
	out, err := p.apps.Adopt(resp.Request.Context(), result)
	if err != nil {
		return err
	}
	setBody(resp, out)
	resp.Header.Set("Cache-Control", "no-store")
	return nil
}

// signedOutCookie tells sso.js that jellyfin-web signed out, rather
// than lost its token: it then signs out of the single sign-on too, at
// /sso/logout.
const signedOutCookie = "jellyfin-sso-signed-out"

// serveLogout answers Jellyfin's logout, with a jellyfin-sso token, by ending its
// session: forwarded, it would end the session's Jellyfin token and leave
// the session holding it. A browser's gets signedOutCookie. A session
// another request ended first, as jellyfin-web logging out twice at once
// does, is logged out once.
func (p *Proxy) serveLogout(w http.ResponseWriter, r *http.Request, session Session) {
	ctx := r.Context()
	err := EndSession(ctx, p.apps.Sessions, p.apps.Jellyfin, session)
	if err != nil && !errors.Is(err, errNoSession) {
		clog.ErrorContext(ctx, "ending a session", "session", session.ID, "error", err)
		http.Error(w, "ending the session failed", http.StatusBadGateway)
		return
	}
	if session.Kind == Browser {
		http.SetCookie(w, &http.Cookie{Name: signedOutCookie, Value: "1", Path: "/", MaxAge: 300, Secure: true, SameSite: http.SameSiteLaxMode})
	}
	if err == nil {
		clog.InfoContext(ctx, "logged out", "session", session.ID, "username", session.Username,
			"client", session.Device.Client, "device", session.Device.Name)
	}
	w.WriteHeader(http.StatusNoContent)
}

// logRefused logs a request whose token found no session, which reaches
// Jellyfin without one, and why: a jellyfin-sso token past its expiry, as a
// browser holding a stale one sends, of a session unused too long, or a
// token jellyfin-sso doesn't know.
func (p *Proxy) logRefused(ctx context.Context, r *http.Request, tok string) {
	session, expires, err := p.apps.Sessions.RefusedToken(ctx, tok)
	switch {
	case errors.Is(err, errNoSession):
		clog.WarnContext(ctx, "refused an unknown token", "method", r.Method, "path", r.URL.Path)
	case err != nil:
		clog.WarnContext(ctx, "looking up a refused token", "error", err)
	default:
		reason := "its session is idle"
		if now := p.apps.Sessions.now(); expires != nil && !expires.After(now) {
			reason = "expired " + now.Sub(*expires).Round(time.Second).String() + " ago"
		}
		clog.WarnContext(ctx, "refused a jellyfin-sso token", "reason", reason, "method", r.Method, "path", r.URL.Path,
			"session", session.ID, "username", session.Username, "client", session.Device.Client, "device", session.Device.Name)
	}
}

// checkSession deletes session if Jellyfin no longer takes its token, which
// Jellyfin answering 401 to a request with it suggests: an administrator
// deleted its device, say. Its client then signs in again, rather than
// holding a jellyfin-sso token that works nowhere.
func (p *Proxy) checkSession(ctx context.Context, session Session) {
	valid, err := p.apps.Jellyfin.Valid(ctx, session.JellyfinToken)
	if err != nil || valid {
		return
	}
	err = p.apps.Sessions.DeleteSession(ctx, session.ID)
	if errors.Is(err, errNoSession) {
		return
	}
	if err != nil {
		clog.WarnContext(ctx, "deleting a session Jellyfin no longer takes", "session", session.ID, "error", err)
		return
	}
	clog.WarnContext(ctx, "deleted a session Jellyfin no longer takes", "session", session.ID,
		"username", session.Username, "client", session.Device.Client, "device", session.Device.Name)
}

// menuLink is the link addMenuLink adds to jellyfin-web's menu, as its
// config.json names one.
var menuLink = map[string]string{"name": "My devices", "icon": "devices", "url": "/sso/"}

// maxWebConfig bounds the size of jellyfin-web's config.json that
// addMenuLink reads.
const maxWebConfig = 1 << 20

// addMenuLink adds menuLink to the menuLinks of jellyfin-web's config.json.
// Like addScript, the config it changes is uncached, and a config
// it can't change goes on as it is.
func addMenuLink(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || resp.Request.Method != http.MethodGet {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWebConfig+1))
	if err != nil {
		return err
	}
	if len(raw) > maxWebConfig {
		clog.WarnContext(resp.Request.Context(), "can't add jellyfin-sso's link to jellyfin-web's config", "size", len(raw))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(raw), resp.Body), resp.Body}
		return nil
	}
	resp.Body.Close()
	var config map[string]json.RawMessage
	var links []json.RawMessage
	err = json.Unmarshal(raw, &config)
	if err == nil && config["menuLinks"] != nil {
		err = json.Unmarshal(config["menuLinks"], &links)
	}
	if err != nil {
		clog.WarnContext(resp.Request.Context(), "can't add jellyfin-sso's link to jellyfin-web's config", "error", err)
		setBody(resp, raw)
		return nil
	}
	link, _ := json.Marshal(menuLink)
	config["menuLinks"], _ = json.Marshal(append(links, link))
	out, err := json.Marshal(config)
	if err != nil {
		return err
	}
	setBody(resp, out)
	uncached(resp)
	return nil
}

// uncached keeps browsers from using resp, a response jellyfin-sso changed,
// without asking again: Jellyfin sends its web files with validators and no
// Cache-Control, which browsers keep for a while, at a guess, without asking.
func uncached(resp *http.Response) {
	resp.Header.Del("Etag")
	resp.Header.Del("Last-Modified")
	resp.Header.Set("Cache-Control", "no-cache")
}
