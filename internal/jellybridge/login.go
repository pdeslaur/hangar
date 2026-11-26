package jellybridge

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/jellyfin"
)

//go:embed login.html
var loginPage []byte

//go:embed bridge.js
var bridgeScript []byte

// maxSessionRequest bounds the body of a session request.
const maxSessionRequest = 4 << 10

// maxDeviceID bounds a device ID: jellyfin-web's is a user agent and a time
// in base64.
const maxDeviceID = 1024

// browserDevice describes a browser's device to Jellyfin until jellyfin-web
// sends its own description.
var browserDevice = jellyfin.Device{Client: "Jellyfin Web", Name: "Web Browser", Version: "jellyfin-bridge"}

// Login signs browsers in to Jellyfin as the user the single sign-on in
// front of it names, serving:
//
//   - GET /sso/login, a page that asks for a session for jellyfin-web's
//     device ID and stores its token where jellyfin-web reads it.
//   - GET /sso/bridge.js, the script signing jellyfin-web in and keeping
//     it signed in, which the Proxy adds to jellyfin-web.
//   - POST /sso/session, taking {"DeviceId": …} and answering a bridge
//     token for the user's session on that device, made if it has none:
//     {"AccessToken", "UserId", "ServerId", "ExpiresIn"}. Asking again
//     refreshes it.
//
// Only the single sign-on may reach these paths, and it has to set
// UserHeader on every request to them, removing a client's own.
type Login struct {
	Sessions *Store
	Jellyfin *jellyfin.Client
	// ServerID is Jellyfin's, from /System/Info/Public.
	ServerID string
	// UserHeader is the request header naming the signed-in user.
	UserHeader string
	// Users maps a signed-in user's name to their Jellyfin username. A user
	// missing from it can't sign in.
	Users map[string]string
	// BrowserTTL is how long a browser's token lasts.
	BrowserTTL time.Duration

	// mu serializes making sessions: logging a device in to Jellyfin ends
	// its earlier token, which a concurrent session would hold.
	mu sync.Mutex
}

func (l *Login) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/sso/login":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(loginPage)
	case "/sso/bridge.js":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(bridgeScript)
	case "/sso/session":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		l.serveSession(w, r)
	default:
		http.NotFound(w, r)
	}
}

// sessionRequest is the body of POST /sso/session.
type sessionRequest struct {
	DeviceID string `json:"DeviceId"`
}

// sessionResponse is the answer to POST /sso/session, named as
// jellyfin-web's credentials name them.
type sessionResponse struct {
	AccessToken string
	UserID      string `json:"UserId"`
	ServerID    string `json:"ServerId"`
	// ExpiresIn is how many seconds the token lasts.
	ExpiresIn int64
}

func (l *Login) serveSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// A JSON body can't come from another site's form, and a script there
	// can't send one without CORS allowing it, which nothing here does.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "the body has to be JSON", http.StatusUnsupportedMediaType)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		http.Error(w, "cross-site request", http.StatusForbidden)
		return
	}
	var req sessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionRequest)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.DeviceID == "" || len(req.DeviceID) > maxDeviceID {
		http.Error(w, "a device ID of 1 to 1024 bytes is required", http.StatusBadRequest)
		return
	}

	user := r.Header.Get(l.UserHeader)
	username, ok := l.Users[user]
	if user == "" || !ok {
		clog.WarnContext(ctx, "signing in an unknown user", "user", user)
		http.Error(w, "no Jellyfin user for "+user, http.StatusForbidden)
		return
	}
	userID, err := l.Jellyfin.UserID(ctx, username)
	if err != nil {
		clog.ErrorContext(ctx, "finding a Jellyfin user", "user", user, "username", username, "error", err)
		http.Error(w, "Jellyfin unavailable", http.StatusBadGateway)
		return
	}
	session, err := l.browserSession(ctx, userID, username, req.DeviceID)
	if err != nil {
		clog.ErrorContext(ctx, "making a session", "user", user, "error", err)
		http.Error(w, "making a session failed", http.StatusBadGateway)
		return
	}
	token, err := l.Sessions.IssueToken(ctx, session.ID, l.BrowserTTL)
	if err != nil {
		clog.ErrorContext(ctx, "issuing a token", "session", session.ID, "error", err)
		http.Error(w, "issuing a token failed", http.StatusInternalServerError)
		return
	}
	clog.InfoContext(ctx, "signed in", "user", user, "username", username, "session", session.ID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(sessionResponse{
		AccessToken: token,
		UserID:      userID,
		ServerID:    l.ServerID,
		ExpiresIn:   int64(l.BrowserTTL / time.Second),
	})
}

// browserSession returns the user's session on the browser deviceID,
// logging the browser in to Jellyfin if it has none.
func (l *Login) browserSession(ctx context.Context, userID, username, deviceID string) (Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	session, err := l.Sessions.FindSession(ctx, Browser, userID, deviceID)
	if !errors.Is(err, errNoSession) {
		return session, err
	}
	device := browserDevice
	device.ID = deviceID
	token, err := l.Jellyfin.Login(ctx, userID, device)
	if err != nil {
		return Session{}, err
	}
	return l.Sessions.CreateSession(ctx, Session{
		Kind:          Browser,
		UserID:        userID,
		Username:      username,
		Device:        device,
		JellyfinToken: token,
	})
}
