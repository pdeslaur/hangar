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

//go:embed account.html
var accountPage []byte

//go:embed bridge.js
var bridgeScript []byte

// maxRequest bounds the body of a request to /sso/.
const maxRequest = 4 << 10

// maxDeviceID bounds a device ID: jellyfin-web's is a user agent and a time
// in base64.
const maxDeviceID = 1024

// browserDevice describes a browser's device to Jellyfin until jellyfin-web
// sends its own description.
var browserDevice = jellyfin.Device{Client: "Jellyfin Web", Name: "Web Browser", Version: "jellyfin-bridge"}

// SSO serves the bridge's pages, under /sso/, to the user the single
// sign-on in front of it names:
//
//   - GET /sso/login, a page that asks for a session for jellyfin-web's
//     device ID and stores its token where jellyfin-web reads it.
//   - GET /sso/bridge.js, the script signing jellyfin-web in and keeping
//     it signed in, which the Proxy adds to jellyfin-web.
//   - POST /sso/session, taking {"DeviceId": …} and answering a bridge
//     token for the user's session on that device, made if it has none:
//     {"AccessToken", "UserId", "ServerId", "ExpiresIn", "RefreshBefore"}.
//     Asking again refreshes it.
//   - GET /sso/, the account page: signing apps in, and the user's
//     sessions.
//   - POST /sso/quickconnect, taking {"Code": …}, signing in the app
//     showing that Quick Connect code.
//   - POST /sso/code, answering {"Username", "Code", "ExpiresIn"}: a
//     one-time code an app without Quick Connect takes as the password.
//   - GET /sso/sessions, the user's sessions, and POST /sso/revoke, taking
//     {"ID": …}, ending one.
//
// Only the single sign-on may reach these paths, and it has to set
// UserHeader on every request to them, removing a client's own.
type SSO struct {
	Sessions *Store
	Jellyfin *jellyfin.Client
	Apps     *Apps
	// ServerID is Jellyfin's, from /System/Info/Public.
	ServerID string
	// UserHeader is the request header naming the signed-in user.
	UserHeader string
	// Users maps a signed-in user's name to their Jellyfin username. A user
	// missing from it can't sign in.
	Users map[string]string
	// BrowserTTL is how long a browser's token lasts.
	BrowserTTL time.Duration
	// RefreshBefore is how long before its token expires a browser
	// refreshes it, at most half of BrowserTTL.
	RefreshBefore time.Duration

	// mu serializes making sessions: logging a device in to Jellyfin ends
	// its earlier token, which a concurrent session would hold.
	mu sync.Mutex
}

func (s *SSO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	get := func(contentType, cache string, body []byte) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", cache)
		w.Write(body)
	}
	post := func(serve func(http.ResponseWriter, *http.Request, user)) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// A JSON body can't come from another site's form, and a script
		// there can't send one without CORS allowing it, which nothing
		// here does.
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			http.Error(w, "the body has to be JSON", http.StatusUnsupportedMediaType)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			http.Error(w, "cross-site request", http.StatusForbidden)
			return
		}
		if u, ok := s.user(w, r); ok {
			serve(w, r, u)
		}
	}

	switch r.URL.Path {
	case "/sso/login":
		get("text/html; charset=utf-8", "no-store", loginPage)
	case "/sso/":
		get("text/html; charset=utf-8", "no-store", accountPage)
	case "/sso/bridge.js":
		get("text/javascript; charset=utf-8", "no-cache", bridgeScript)
	case "/sso/session":
		post(s.serveSession)
	case "/sso/quickconnect":
		post(s.serveQuickConnect)
	case "/sso/code":
		post(s.serveCode)
	case "/sso/revoke":
		post(s.serveRevoke)
	case "/sso/sessions":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if u, ok := s.user(w, r); ok {
			s.serveSessions(w, r, u)
		}
	default:
		http.NotFound(w, r)
	}
}

// user is the signed-in user's Jellyfin user.
type user struct {
	// name is the single sign-on's.
	name string
	// id and username are Jellyfin's.
	id       string
	username string
}

// user returns the signed-in user, or answers an error.
func (s *SSO) user(w http.ResponseWriter, r *http.Request) (user, bool) {
	ctx := r.Context()
	name := r.Header.Get(s.UserHeader)
	username, ok := s.Users[name]
	if name == "" || !ok {
		clog.WarnContext(ctx, "an unknown user", "user", name)
		http.Error(w, "no Jellyfin user for "+name, http.StatusForbidden)
		return user{}, false
	}
	id, err := s.Jellyfin.UserID(ctx, username)
	if err != nil {
		clog.ErrorContext(ctx, "finding a Jellyfin user", "user", name, "username", username, "error", err)
		http.Error(w, "Jellyfin unavailable", http.StatusBadGateway)
		return user{}, false
	}
	return user{name: name, id: id, username: username}, true
}

// decode decodes a request's JSON body into v, or answers an error.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest)).Decode(v); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

// reply answers v as JSON.
func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
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
	// RefreshBefore is how many seconds before it expires to refresh it.
	RefreshBefore int64
}

func (s *SSO) serveSession(w http.ResponseWriter, r *http.Request, u user) {
	ctx := r.Context()
	var req sessionRequest
	if !decode(w, r, &req) {
		return
	}
	if req.DeviceID == "" || len(req.DeviceID) > maxDeviceID {
		http.Error(w, "a device ID of 1 to 1024 bytes is required", http.StatusBadRequest)
		return
	}
	session, err := s.browserSession(ctx, u, req.DeviceID)
	if err != nil {
		clog.ErrorContext(ctx, "making a session", "user", u.name, "error", err)
		http.Error(w, "making a session failed", http.StatusBadGateway)
		return
	}
	token, err := s.Sessions.IssueToken(ctx, session.ID, s.BrowserTTL)
	if err != nil {
		clog.ErrorContext(ctx, "issuing a token", "session", session.ID, "error", err)
		http.Error(w, "issuing a token failed", http.StatusInternalServerError)
		return
	}
	clog.InfoContext(ctx, "signed in", "user", u.name, "username", u.username, "session", session.ID)
	reply(w, sessionResponse{
		AccessToken:   token,
		UserID:        u.id,
		ServerID:      s.ServerID,
		ExpiresIn:     int64(s.BrowserTTL / time.Second),
		RefreshBefore: int64(min(s.RefreshBefore, s.BrowserTTL/2) / time.Second),
	})
}

// browserSession returns the user's session on the browser deviceID,
// logging the browser in to Jellyfin if it has none.
func (s *SSO) browserSession(ctx context.Context, u user, deviceID string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.Sessions.FindSession(ctx, Browser, u.id, deviceID)
	if !errors.Is(err, errNoSession) {
		return session, err
	}
	device := browserDevice
	device.ID = deviceID
	result, err := s.Jellyfin.Login(ctx, u.id, device)
	if err != nil {
		return Session{}, err
	}
	return s.Sessions.CreateSession(ctx, Session{
		Kind:          Browser,
		UserID:        u.id,
		Username:      u.username,
		Device:        device,
		JellyfinToken: result.AccessToken,
	})
}

func (s *SSO) serveQuickConnect(w http.ResponseWriter, r *http.Request, u user) {
	ctx := r.Context()
	var req struct{ Code string }
	if !decode(w, r, &req) {
		return
	}
	err := s.Jellyfin.AuthorizeQuickConnect(ctx, req.Code, u.id)
	if se := (*jellyfin.StatusError)(nil); errors.As(err, &se) && se.Status == http.StatusNotFound {
		http.Error(w, "no app is showing that code", http.StatusNotFound)
		return
	}
	if err != nil {
		clog.ErrorContext(ctx, "approving a Quick Connect code", "user", u.name, "error", err)
		http.Error(w, "Jellyfin refused the code", http.StatusBadGateway)
		return
	}
	clog.InfoContext(ctx, "approved a Quick Connect code", "user", u.name, "username", u.username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *SSO) serveCode(w http.ResponseWriter, r *http.Request, u user) {
	code, expires := s.Apps.IssueCode(u.id, u.username)
	clog.InfoContext(r.Context(), "issued an app code", "user", u.name, "username", u.username)
	reply(w, struct {
		Username  string
		Code      string
		ExpiresIn int64
	}{u.username, code, int64(time.Until(expires) / time.Second)})
}

// sessionInfo is a session as GET /sso/sessions lists it.
type sessionInfo struct {
	ID       string
	Kind     Kind
	Client   string
	Device   string
	Created  time.Time
	LastUsed time.Time
}

func (s *SSO) serveSessions(w http.ResponseWriter, r *http.Request, u user) {
	ctx := r.Context()
	sessions, err := s.Sessions.UserSessions(ctx, u.id)
	if err != nil {
		clog.ErrorContext(ctx, "listing sessions", "user", u.name, "error", err)
		http.Error(w, "listing sessions failed", http.StatusInternalServerError)
		return
	}
	infos := []sessionInfo{}
	for _, session := range sessions {
		infos = append(infos, sessionInfo{
			ID:       session.ID,
			Kind:     session.Kind,
			Client:   session.Device.Client,
			Device:   session.Device.Name,
			Created:  session.Created,
			LastUsed: session.LastUsed,
		})
	}
	reply(w, infos)
}

func (s *SSO) serveRevoke(w http.ResponseWriter, r *http.Request, u user) {
	ctx := r.Context()
	var req struct{ ID string }
	if !decode(w, r, &req) {
		return
	}
	sessions, err := s.Sessions.UserSessions(ctx, u.id)
	if err != nil {
		clog.ErrorContext(ctx, "listing sessions", "user", u.name, "error", err)
		http.Error(w, "listing sessions failed", http.StatusInternalServerError)
		return
	}
	for _, session := range sessions {
		if session.ID != req.ID {
			continue
		}
		if err := EndSession(ctx, s.Sessions, s.Jellyfin, session); err != nil {
			clog.ErrorContext(ctx, "ending a session", "session", session.ID, "error", err)
			http.Error(w, "ending the session failed", http.StatusBadGateway)
			return
		}
		clog.InfoContext(ctx, "revoked a session", "user", u.name, "session", session.ID,
			"client", session.Device.Client, "device", session.Device.Name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "no such session", http.StatusNotFound)
}
