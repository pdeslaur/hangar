// Package jellybridge puts Jellyfin behind single sign-on: it hands clients
// tokens of its own, and forwards their requests to Jellyfin with the
// Jellyfin token of each one's session, which never leaves it.
package jellybridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/jellyfin"
)

// forwardedHeaders are the ones Caddy sets in front of the bridge, which a
// ReverseProxy with Rewrite would drop.
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// Proxy forwards requests to Jellyfin, swapping each bridge token for its
// session's Jellyfin token.
//
// It removes every token from a request, wherever Jellyfin would read one,
// and sends Jellyfin the Jellyfin token of the session whose bridge token
// it found, in an Authorization header, or none: a Jellyfin token sent to
// the bridge, an unknown or an expired one, reaches Jellyfin as no token at
// all. Jellyfin gets the device's ID from the session, and the rest of the
// device's description from the client when it gives one.
//
// It adds the bridge's script, /sso/bridge.js, to jellyfin-web's page.
//
// Jellyfin's logins go through Apps: a password login is the bridge's, the
// result of a Quick Connect login gets a bridge token in place of
// Jellyfin's, and the obsolete password login and password resets are
// refused.
type Proxy struct {
	sessions *Store
	apps     *Apps
	proxy    *httputil.ReverseProxy
}

// NewProxy returns a Proxy to the Jellyfin at target, its sessions in
// sessions and its apps signed in by apps, sending requests with transport,
// or http.DefaultTransport if nil.
func NewProxy(target *url.URL, sessions *Store, apps *Apps, transport http.RoundTripper) *Proxy {
	p := &Proxy{sessions: sessions, apps: apps}
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
		Transport:      transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			clog.WarnContext(r.Context(), "forwarding to jellyfin", "error", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return p
}

// route is what the bridge does with a request to Jellyfin.
type route int

const (
	// forward sends it to Jellyfin.
	forward route = iota
	// webPage is jellyfin-web's page, to add the script to.
	webPage
	// passwordLogin is Jellyfin's password login, which Apps answers.
	passwordLogin
	// quickConnectLogin is Jellyfin's Quick Connect login, whose result
	// Apps adopts.
	quickConnectLogin
	// logout is Jellyfin's logout, which ends the bridge session.
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
	case isWebPage(r):
		return webPage
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

// sessionKey is the context key of the session a request to Jellyfin
// carries the token of.
type sessionKey struct{}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r = r.Clone(ctx)
	rt := routeOf(r)
	switch rt {
	case webPage:
		// Whole and uncompressed, for addScript to change: a browser may hold
		// a copy from before the bridge, which Jellyfin would let it keep.
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
	session, err := p.sessions.Lookup(ctx, c.token)
	switch {
	case err == nil:
		if err := p.sessions.Touch(ctx, session.ID); err != nil {
			clog.WarnContext(ctx, "recording a session's use", "session", session.ID, "error", err)
		}
		device := session.Device
		if c.device.Client != "" {
			device.Client = c.device.Client
		}
		if c.device.Name != "" {
			device.Name = c.device.Name
		}
		if c.device.Version != "" {
			device.Version = c.device.Version
		}
		if rt == logout {
			p.serveLogout(w, r, session)
			return
		}
		r.Header.Set("Authorization", device.Header(session.JellyfinToken))
		r = r.WithContext(context.WithValue(ctx, sessionKey{}, session))
	case errors.Is(err, errNoSession):
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

// scriptTag adds the bridge's script to jellyfin-web.
const scriptTag = `<script src="/sso/bridge.js"></script>`

// maxWebPage bounds the size of jellyfin-web's page that addScript reads.
const maxWebPage = 1 << 20

// isWebPage reports whether r asks for jellyfin-web's page.
func isWebPage(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(r.URL.Path == "/web/" || r.URL.Path == "/web/index.html")
}

// addScript adds scriptTag to jellyfin-web's page, before its </head>. The
// page it changes loses its validators, so that no browser keeps a copy
// without the script. A page it can't change goes on as it is: jellyfin-web
// works without the script, until its token expires.
func addScript(resp *http.Response) error {
	if !isWebPage(resp.Request) || resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Content-Encoding") != "" || resp.Request.Method != http.MethodGet {
		return nil
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxWebPage+1))
	if err != nil {
		return err
	}
	head := bytes.Index(page, []byte("</head>"))
	if len(page) > maxWebPage || head < 0 {
		clog.WarnContext(resp.Request.Context(), "can't add the bridge's script to jellyfin-web's page", "size", len(page))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(page), resp.Body), resp.Body}
		return nil
	}
	resp.Body.Close()
	page = slices.Concat(page[:head], []byte(scriptTag), page[head:])
	resp.Body = io.NopCloser(bytes.NewReader(page))
	resp.ContentLength = int64(len(page))
	resp.Header.Set("Content-Length", fmt.Sprint(len(page)))
	resp.Header.Del("Etag")
	resp.Header.Del("Last-Modified")
	return nil
}

// maxLoginResponse bounds the size of a login's result that adoptLogin
// reads.
const maxLoginResponse = 1 << 20

// modifyResponse changes Jellyfin's responses on the routes that need it.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	if session, ok := resp.Request.Context().Value(sessionKey{}).(Session); ok && resp.StatusCode == http.StatusUnauthorized {
		p.checkSession(resp.Request.Context(), session)
	}
	switch routeOf(resp.Request) {
	case webPage:
		return addScript(resp)
	case quickConnectLogin:
		return p.adoptLogin(resp)
	}
	return nil
}

// adoptLogin gives an app a bridge token in place of the Jellyfin token of
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
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", fmt.Sprint(len(out)))
	resp.Header.Set("Cache-Control", "no-store")
	return nil
}

// serveLogout answers Jellyfin's logout, with a bridge token, by ending its
// session: forwarded, it would end the session's Jellyfin token and leave
// the session holding it.
func (p *Proxy) serveLogout(w http.ResponseWriter, r *http.Request, session Session) {
	ctx := r.Context()
	if err := EndSession(ctx, p.sessions, p.apps.Jellyfin, session); err != nil {
		clog.ErrorContext(ctx, "ending a session", "session", session.ID, "error", err)
		http.Error(w, "ending the session failed", http.StatusBadGateway)
		return
	}
	clog.InfoContext(ctx, "logged out", "session", session.ID, "username", session.Username,
		"client", session.Device.Client, "device", session.Device.Name)
	w.WriteHeader(http.StatusNoContent)
}

// checkSession deletes session if Jellyfin no longer takes its token, which
// Jellyfin answering 401 to a request with it suggests: an administrator
// deleted its device, say. Its client then signs in again, rather than
// holding a bridge token that works nowhere.
func (p *Proxy) checkSession(ctx context.Context, session Session) {
	valid, err := p.apps.Jellyfin.Valid(ctx, session.JellyfinToken)
	if err != nil || valid {
		return
	}
	if err := p.sessions.DeleteSession(ctx, session.ID); err != nil {
		clog.WarnContext(ctx, "deleting a session Jellyfin no longer takes", "session", session.ID, "error", err)
		return
	}
	clog.WarnContext(ctx, "deleted a session Jellyfin no longer takes", "session", session.ID,
		"username", session.Username, "client", session.Device.Client, "device", session.Device.Name)
}
