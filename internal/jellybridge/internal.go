package jellybridge

import (
	"net/http"
	"strings"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/jellyfin"
)

// Internal answers the requests of the services trusted to sign their
// users in, on a listener only they reach, like through an mTLS tunnel:
// POST /bridge/quickconnect approves a Quick Connect code for a user, by
// the single sign-on's name, {"Code": "123456", "User": "phil"}, mapped
// as on /sso/. The service, holding the code's secret, then logs in with
// it, through the Proxy, which hands it a bridge token.
//
// A service reaching it signs in as any user Users allows, so it trusts
// the service as much as the reverse proxy setting UserHeader.
func (s *SSO) Internal() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /bridge/quickconnect", s.serveInternalQuickConnect)
	return mux
}

// quickConnectRequest is a request of POST /bridge/quickconnect.
type quickConnectRequest struct {
	Code string
	User string
}

func (s *SSO) serveInternalQuickConnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req quickConnectRequest
	if !decode(w, r, &req) {
		return
	}
	u, ok := s.lookUp(w, r, req.User)
	if !ok {
		return
	}
	err := s.Apps.Jellyfin.AuthorizeQuickConnect(ctx, strings.TrimSpace(req.Code), u.id)
	switch {
	case jellyfin.IsStatus(err, http.StatusNotFound):
		http.Error(w, "no Quick Connect code "+req.Code, http.StatusNotFound)
	case err != nil:
		clog.ErrorContext(ctx, "approving a service's Quick Connect code", "user", u.name, "error", err)
		http.Error(w, "Jellyfin refused the code", http.StatusBadGateway)
	default:
		clog.InfoContext(ctx, "approved a service's Quick Connect code", "user", u.name, "username", u.username)
		w.WriteHeader(http.StatusNoContent)
	}
}
