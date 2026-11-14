package keybroker

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// ServicePrefix is the path a client's SPIFFE ID starts with.
const ServicePrefix = "/svc/"

// AuthorizeServices accepts the SPIFFE IDs of td whose path starts with
// ServicePrefix.
func AuthorizeServices(td spiffeid.TrustDomain) tlsconfig.Authorizer {
	return tlsconfig.AdaptMatcher(func(id spiffeid.ID) error {
		if !id.MemberOf(td) || !strings.HasPrefix(id.Path(), ServicePrefix) {
			return fmt.Errorf("unauthorized ID %q", id)
		}
		return nil
	})
}

// NewServer returns a server listening on addr with mTLS, as svids' SVID,
// for the services of td that bundles' authorities sign for. Each gets its
// own secrets: GET /v1/secrets/{name}?version=N&bytes=B returns the secret
// Derive gives for the SPIFFE ID in the client's certificate, raw.
func NewServer(addr string, master []byte, svids x509svid.Source, bundles x509bundle.Source, td spiffeid.TrustDomain) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/secrets/{name...}", func(w http.ResponseWriter, r *http.Request) {
		serveSecret(w, r, master)
	})
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		TLSConfig:         tlsconfig.MTLSServerConfig(svids, bundles, AuthorizeServices(td)),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

func serveSecret(w http.ResponseWriter, r *http.Request, master []byte) {
	ctx := r.Context()
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "no client certificate", http.StatusUnauthorized)
		return
	}
	id, err := x509svid.IDFromCert(r.TLS.PeerCertificates[0])
	if err != nil {
		http.Error(w, "no SPIFFE ID", http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	version, err := intParam(r, "version", 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	size, err := intParam(r, "bytes", 32)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key, err := Derive(master, id, name, version, size)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	clog.InfoContext(ctx, "derived", "id", id.String(), "name", name, "version", version, "bytes", size)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(key)
}

// intParam returns the query parameter key as an int, or def when it's
// missing.
func intParam(r *http.Request, key string, def int) (int, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s %q: not an integer", key, s)
	}
	return n, nil
}
