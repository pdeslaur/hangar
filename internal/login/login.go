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

// maxRequest bounds the body of a login request.
const maxRequest = 4 << 10

// Request is the optional JSON body of a login request.
type Request struct {
	// DeviceID is the device to log in on, such as the one a client keeps
	// end-to-end encryption keys for: logging in again on it adds an access
	// token, keeping its keys. Without it, tuwunel creates a device.
	DeviceID string `json:"device_id,omitempty"`
}

// CrossSigningKeysPath is the path replacing the caller's user's
// cross-signing keys.
const CrossSigningKeysPath = "/cross-signing-keys"

// maxCrossSigningKeys bounds the body of a CrossSigningKeys request: three
// keys, with their signatures.
const maxCrossSigningKeys = 16 << 10

// CrossSigningKeys is the JSON body of a request replacing the caller's
// user's cross-signing keys, which needs user-interactive auth once the user
// has some. The handler uploads them with the access token and, as auth, a
// JWT it signs for the user, which never leaves its pod.
type CrossSigningKeys struct {
	// AccessToken is the caller's: it needs to be its user's.
	AccessToken string `json:"access_token"`
	// MasterKey, SelfSigningKey and UserSigningKey are the keys, as in the
	// request uploading them.
	MasterKey      json.RawMessage `json:"master_key"`
	SelfSigningKey json.RawMessage `json:"self_signing_key,omitempty"`
	UserSigningKey json.RawMessage `json:"user_signing_key,omitempty"`
}

// maxResponse bounds how much of tuwunel's response it relays.
const maxResponse = 64 << 10

// Handler answers POST requests carrying a JWT-SVID as a bearer token, for
// the Matrix user the SVID's SPIFFE ID maps to. At /, with optionally a
// Request, it answers the response of tuwunel's login endpoint; at
// CrossSigningKeysPath, with CrossSigningKeys, the response of its endpoint
// uploading them.
type Handler struct {
	// Bundles verifies JWT-SVIDs; the Workload API's keeps up with the
	// SPIRE server's key rotation.
	Bundles jwtbundle.Source
	// Audience is the aud a JWT-SVID needs, as its only one.
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
	if r.URL.Path != "/" && r.URL.Path != CrossSigningKeysPath {
		http.NotFound(w, r)
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
	// An SVID for other audiences too could be replayed here by any of them.
	if len(svid.Audience) != 1 {
		clog.WarnContext(ctx, "JWT-SVID for other audiences too", "audience", svid.Audience)
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
	if r.URL.Path == CrossSigningKeysPath {
		h.serveCrossSigningKeys(w, r, id, localpart)
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
	resp, err := h.call(r, http.MethodPost, "/_matrix/client/v3/login", "", login)
	if err != nil {
		clog.ErrorContext(ctx, "calling tuwunel", "error", err)
		http.Error(w, "tuwunel unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	clog.InfoContext(ctx, "logged in", "spiffe_id", id, "localpart", localpart, "device_id", request.DeviceID, "status", resp.StatusCode)
	relay(w, r, resp)
}

// serveCrossSigningKeys replaces the cross-signing keys of localpart, the
// user id maps to, with the CrossSigningKeys of the request.
func (h *Handler) serveCrossSigningKeys(w http.ResponseWriter, r *http.Request, id, localpart string) {
	ctx := r.Context()
	var request CrossSigningKeys
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCrossSigningKeys))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&request); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if request.AccessToken == "" || len(request.MasterKey) == 0 {
		http.Error(w, "invalid request: access_token and master_key are required", http.StatusBadRequest)
		return
	}

	// tuwunel replaces the keys of the access token's user, but takes the
	// auth of the JWT's: without this, the caller could replace another
	// user's keys with its token.
	resp, err := h.call(r, http.MethodGet, "/_matrix/client/v3/account/whoami", request.AccessToken, nil)
	if err != nil {
		clog.ErrorContext(ctx, "calling tuwunel", "error", err)
		http.Error(w, "tuwunel unreachable", http.StatusBadGateway)
		return
	}
	var whoami struct {
		UserID string `json:"user_id"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&whoami)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil {
		clog.WarnContext(ctx, "access token rejected", "spiffe_id", id, "status", resp.StatusCode)
		http.Error(w, "invalid access token", http.StatusUnauthorized)
		return
	}
	if user, _, _ := strings.Cut(strings.TrimPrefix(whoami.UserID, "@"), ":"); user != localpart {
		clog.WarnContext(ctx, "access token of another user", "spiffe_id", id, "localpart", localpart, "user_id", whoami.UserID)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	jwt, err := h.Signer.Sign(localpart, time.Now())
	if err != nil {
		clog.ErrorContext(ctx, "signing JWT", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	upload := map[string]any{
		"master_key": request.MasterKey,
		"auth":       map[string]string{"type": "org.matrix.login.jwt", "token": jwt},
	}
	if len(request.SelfSigningKey) != 0 {
		upload["self_signing_key"] = request.SelfSigningKey
	}
	if len(request.UserSigningKey) != 0 {
		upload["user_signing_key"] = request.UserSigningKey
	}
	resp, err = h.call(r, http.MethodPost, "/_matrix/client/v3/keys/device_signing/upload", request.AccessToken, upload)
	if err != nil {
		clog.ErrorContext(ctx, "calling tuwunel", "error", err)
		http.Error(w, "tuwunel unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	clog.InfoContext(ctx, "replaced cross-signing keys", "spiffe_id", id, "localpart", localpart, "status", resp.StatusCode)
	relay(w, r, resp)
}

// call sends tuwunel a request at path, with token, if any, and body, if
// not nil, as JSON.
func (h *Handler) call(r *http.Request, method, path, token string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, strings.TrimSuffix(h.MatrixURL, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return h.Client.Do(req)
}

// relay answers tuwunel's response, such as an access token or a Matrix
// error, as is.
func relay(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, io.LimitReader(resp.Body, maxResponse)); err != nil {
		clog.WarnContext(r.Context(), "relaying tuwunel's response", "error", err)
	}
}
