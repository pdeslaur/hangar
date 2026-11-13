// Package login exchanges SPIFFE JWT-SVIDs for Matrix access tokens, through
// tuwunel's JWT login.
package login

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
)

// deviceID is a device ID a caller may log in on: Matrix leaves them opaque,
// tuwunel generates ten capital letters.
var deviceID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// maxRequest bounds the request body.
const maxRequest = 4 << 10

// Request is the optional JSON body of a request.
type Request struct {
	// DeviceID is the device to log in on, such as the one a client keeps
	// end-to-end encryption keys for: logging in again on it adds an access
	// token, keeping its keys. Without it, tuwunel creates a device.
	DeviceID string `json:"device_id,omitempty"`
}

// maxResponse bounds how much of tuwunel's response it relays.
const maxResponse = 64 << 10

// Handler answers POST requests carrying a JWT-SVID as a bearer token, and
// optionally a Request, with the response of tuwunel's login endpoint for the
// Matrix user the SVID's SPIFFE ID maps to.
type Handler struct {
	// Bundles verifies JWT-SVIDs; the Workload API's keeps up with the
	// SPIRE server's key rotation.
	Bundles jwtbundle.Source
	// Audience is the aud a JWT-SVID needs.
	Audience string
	// Users maps SPIFFE IDs to the Matrix localparts they log in as. Other
	// SPIFFE IDs are refused.
	Users map[string]string
	// Signer signs the JWTs tuwunel exchanges.
	Signer *Signer
	// MatrixURL is tuwunel's client API base URL.
	MatrixURL string
	// Client calls tuwunel.
	Client *http.Client
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	svid, err := jwtsvid.ParseAndValidate(token, h.Bundles, []string{h.Audience})
	if err != nil {
		clog.WarnContext(ctx, "invalid JWT-SVID", "error", err)
		http.Error(w, "invalid JWT-SVID", http.StatusUnauthorized)
		return
	}
	id := svid.ID.String()
	localpart, ok := h.Users[id]
	if !ok {
		clog.WarnContext(ctx, "SPIFFE ID maps to no Matrix user", "spiffe_id", id)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// The body is read only once the caller is authenticated.
	var request Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if request.DeviceID != "" && !deviceID.MatchString(request.DeviceID) {
		http.Error(w, "invalid device_id", http.StatusBadRequest)
		return
	}

	jwt, err := h.Signer.Sign(localpart, time.Now())
	if err != nil {
		clog.ErrorContext(ctx, "signing JWT", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	login := map[string]string{
		"type":                        "org.matrix.login.jwt",
		"token":                       jwt,
		"initial_device_display_name": "tuwunel-svid-sts",
	}
	if request.DeviceID != "" {
		login["device_id"] = request.DeviceID
	}
	body, err := json.Marshal(login)
	if err != nil {
		clog.ErrorContext(ctx, "encoding login request", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(h.MatrixURL, "/")+"/_matrix/client/v3/login", bytes.NewReader(body))
	if err != nil {
		clog.ErrorContext(ctx, "building login request", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		clog.ErrorContext(ctx, "calling tuwunel", "error", err)
		http.Error(w, "tuwunel unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// tuwunel's response, an access token or a Matrix error, goes to the
	// caller as is.
	clog.InfoContext(ctx, "logged in", "spiffe_id", id, "localpart", localpart, "device_id", request.DeviceID, "status", resp.StatusCode)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, io.LimitReader(resp.Body, maxResponse)); err != nil {
		clog.WarnContext(ctx, "relaying tuwunel's response", "error", err)
	}
}
