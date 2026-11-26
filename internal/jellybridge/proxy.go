// Package jellybridge puts Jellyfin behind single sign-on: it hands clients
// tokens of its own, and forwards their requests to Jellyfin with the
// Jellyfin token of each one's session, which never leaves it.
package jellybridge

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"

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
type Proxy struct {
	sessions *Store
	proxy    *httputil.ReverseProxy
}

// NewProxy returns a Proxy to the Jellyfin at target, its sessions in
// sessions, sending requests with transport, or http.DefaultTransport if
// nil.
func NewProxy(target *url.URL, sessions *Store, transport http.RoundTripper) *Proxy {
	return &Proxy{
		sessions: sessions,
		proxy: &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = pr.In.Host
				for _, h := range forwardedHeaders {
					if v := pr.In.Header.Values(h); len(v) > 0 {
						pr.Out.Header[h] = v
					}
				}
			},
			ModifyResponse: addScript,
			Transport:      transport,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				clog.WarnContext(r.Context(), "forwarding to jellyfin", "error", err)
				w.WriteHeader(http.StatusBadGateway)
			},
		},
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r = r.Clone(ctx)
	if isWebPage(r) {
		// Uncompressed, for addScript to change.
		r.Header.Del("Accept-Encoding")
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
		r.Header.Set("Authorization", device.Header(session.JellyfinToken))
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
