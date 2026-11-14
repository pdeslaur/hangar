// Package oidcrelay authenticates an app's OIDC client with the workload's
// JWT-SVID in place of the client secret the app sends: it serves the
// provider's discovery document with its token endpoint replaced by its own,
// and forwards token requests there with a client assertion.
package oidcrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
)

const (
	// DiscoveryPath is where it serves the discovery document, the issuer's
	// own path for it.
	DiscoveryPath = "/.well-known/openid-configuration"
	// TokenPath is the token endpoint it puts in the discovery document.
	TokenPath = "/token"

	// clientAssertionType is RFC 7523's, a JWT authenticating the client.
	clientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	// svidTimeout bounds fetching a JWT-SVID, so that an agent that doesn't
	// answer fails the request rather than hangs it.
	svidTimeout = 5 * time.Second
)

// maxRequest bounds the body of a token request.
const maxRequest = 64 << 10

// maxResponse bounds how much of the provider's responses it reads.
const maxResponse = 1 << 20

// SVIDs fetches the workload's JWT-SVIDs, like a workloadapi.Client.
type SVIDs interface {
	FetchJWTSVID(ctx context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error)
}

// Handler serves GET DiscoveryPath, the issuer's discovery document with
// token_endpoint pointing at TokenPath on the host the app asked, and POST
// TokenPath, forwarding a token request to the issuer's token endpoint with
// a JWT-SVID as its client assertion. Everything else in the document, the
// issuer and its JWKS included, stays the issuer's, so ID tokens verify as
// the issuer's.
type Handler struct {
	// Issuer is the OpenID provider's issuer, without a trailing slash, and
	// the audience of the JWT-SVIDs.
	Issuer string
	// SVIDs fetches the JWT-SVIDs.
	SVIDs SVIDs
	// Client calls the issuer.
	Client *http.Client
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case DiscoveryPath:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.serveDiscovery(w, r)
	case TokenPath:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.serveToken(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	doc, _, err := h.discover(ctx)
	if err != nil {
		clog.ErrorContext(ctx, "fetching discovery document", "error", err)
		http.Error(w, "issuer unreachable", http.StatusBadGateway)
		return
	}
	doc["token_endpoint"], _ = json.Marshal("http://" + r.Host + TokenPath)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

// discover fetches the issuer's discovery document, and its token endpoint.
// It fetches it for each request, which only the app's start and its logins
// make, so that it never serves a stale one.
func (h *Handler) discover(ctx context.Context) (map[string]json.RawMessage, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Issuer+DiscoveryPath, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s: %s", req.URL, resp.Status)
	}
	var doc map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&doc); err != nil {
		return nil, "", fmt.Errorf("%s: %w", req.URL, err)
	}
	var token string
	if err := json.Unmarshal(doc["token_endpoint"], &token); err != nil || !strings.HasPrefix(token, "https://") {
		return nil, "", fmt.Errorf("%s: no https token_endpoint", req.URL)
	}
	return doc, token, nil
}

func (h *Handler) serveToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	form := url.Values{}
	for k, v := range r.PostForm {
		form[k] = v
	}
	// The app's own client authentication, replaced by the JWT-SVID's.
	form.Del("client_secret")
	form.Del("client_assertion")
	form.Del("client_assertion_type")
	// client_secret_basic: the client ID and secret, each form-encoded.
	if user, _, ok := r.BasicAuth(); ok {
		id, err := url.QueryUnescape(user)
		if err != nil {
			tokenError(w, http.StatusBadRequest, "invalid_request", "invalid Basic credentials")
			return
		}
		if c := form.Get("client_id"); c != "" && c != id {
			tokenError(w, http.StatusBadRequest, "invalid_request", "client_id differs from the Basic credentials'")
			return
		}
		form.Set("client_id", id)
	}
	client := form.Get("client_id")
	if client == "" {
		tokenError(w, http.StatusBadRequest, "invalid_request", "missing client_id")
		return
	}

	_, endpoint, err := h.discover(ctx)
	if err != nil {
		clog.ErrorContext(ctx, "fetching discovery document", "error", err)
		tokenError(w, http.StatusBadGateway, "temporarily_unavailable", "issuer unreachable")
		return
	}
	svidCtx, cancel := context.WithTimeout(ctx, svidTimeout)
	defer cancel()
	svid, err := h.SVIDs.FetchJWTSVID(svidCtx, jwtsvid.Params{Audience: h.Issuer})
	if err != nil {
		clog.ErrorContext(ctx, "fetching JWT-SVID", "error", err)
		tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "client assertion unavailable")
		return
	}
	form.Set("client_assertion_type", clientAssertionType)
	form.Set("client_assertion", svid.Marshal())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		clog.ErrorContext(ctx, "calling the token endpoint", "error", err)
		tokenError(w, http.StatusBadGateway, "temporarily_unavailable", "issuer unreachable")
		return
	}
	defer resp.Body.Close()
	clog.InfoContext(ctx, "token request", "client_id", client, "grant_type", form.Get("grant_type"), "spiffe_id", svid.ID.String(), "status", resp.StatusCode)
	for _, k := range []string{"Content-Type", "Cache-Control", "Pragma"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, maxResponse))
}

// tokenError answers an error response of RFC 6749's token endpoint.
func tokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}
