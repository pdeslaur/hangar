package jellyfinsso

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/jellyfin"
	"github.com/pdeslaur/hangar/internal/jellyfinsso/web"
)

// maxRequest bounds the body of a request to /sso/.
const maxRequest = 4 << 10

// maxDeviceID bounds a device ID from deviceCookie.
const maxDeviceID = 1024

// deviceCookie holds a browser's device ID, so that signing in again, once
// its token has expired, finds its session rather than making another.
const deviceCookie = "jellyfin-sso-device"

// deviceCookieAge is how long deviceCookie lasts unused: browsers keep a
// cookie 400 days at most.
const deviceCookieAge = 400 * 24 * time.Hour

// browserDevice describes a browser's device to Jellyfin until jellyfin-web
// sends its own description.
var browserDevice = jellyfin.Device{Client: "Jellyfin Web", Name: "Web Browser", Version: "jellyfin-sso"}

// SSO serves jellyfin-sso's pages, under /sso/, to the user the single
// sign-on in front of it names:
//
//   - GET /sso/login, a page signing jellyfin-web in: it stores a jellyfin-sso
//     token for the user's session on the browser where jellyfin-web reads
//     its credentials, with sso.js, then opens jellyfin-web.
//   - POST /sso/session, answering a jellyfin-sso token for the same session as
//     JSON, which sso.js asks for to refresh it: {"AccessToken",
//     "UserId", "ServerId", "DeviceId", "ExpiresIn", "RefreshBefore"}.
//   - GET /sso/, the account page, "My devices": signing apps in, and the
//     user's sessions. Its forms post to POST /sso/quickconnect, signing in
//     the app showing a Quick Connect code; POST /sso/code, giving a
//     one-time code an app without Quick Connect takes as the password;
//     and POST /sso/revoke, ending a session. Each answers the page, whose
//     parts Unpoly swaps in.
//   - GET /sso/logout, sending the browser back to jellyfin-web, which
//     signs in again: sso.js opens it once jellyfin-web signs out, for
//     the single sign-on to sign out too, and it reaches jellyfin-sso only
//     when the single sign-on doesn't.
//
// The browser's device ID is in deviceCookie, set on its first sign-in.
//
// Only the single sign-on may reach these paths, and it has to set
// UserHeader on every request to them, removing a client's own. POSTs from
// another site are refused.
//
// It also serves their static files, under /sso-static/, without a user:
// sso.js, the script keeping jellyfin-web signed in, which the Proxy
// adds to jellyfin-web and which loads before anyone has signed in;
// style.css; and Unpoly. So the sign-on covers /sso/* whole, and nothing
// under /sso-static/.
type SSO struct {
	// Apps signs apps in, and holds the sessions and the Jellyfin client.
	Apps *Apps
	// ServerID is Jellyfin's, from /System/Info/Public.
	ServerID string
	// UserHeader is the request header naming the signed-in user.
	UserHeader string
	// Users maps a signed-in user's name to their Jellyfin username. A user
	// missing from it can't sign in. Without it, a signed-in user's name is
	// their Jellyfin username, and only the single sign-on decides who signs
	// in.
	Users map[string]string
	// BrowserTTL is how long a browser's token lasts.
	BrowserTTL time.Duration
	// RefreshBefore is how long before its token expires a browser
	// refreshes it, at most half of BrowserTTL.
	RefreshBefore time.Duration

	// mu serializes making sessions: logging a device in to Jellyfin ends
	// its earlier token, which a concurrent session would hold.
	mu sync.Mutex

	once    sync.Once
	handler http.Handler
}

func (s *SSO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.once.Do(func() {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /sso/{$}", s.signedIn(s.serveAccount))
		mux.HandleFunc("GET /sso/login", s.signedIn(s.serveLogin))
		mux.HandleFunc("GET /sso/logout", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/web/", http.StatusFound)
		})
		mux.HandleFunc("GET /sso-static/{name}", serveStatic)
		mux.HandleFunc("POST /sso/session", s.signedIn(s.serveSession))
		mux.HandleFunc("POST /sso/quickconnect", s.signedIn(s.serveQuickConnect))
		mux.HandleFunc("POST /sso/code", s.signedIn(s.serveCode))
		mux.HandleFunc("POST /sso/revoke", s.signedIn(s.serveRevoke))
		s.handler = http.NewCrossOriginProtection().Handler(mux)
	})
	s.handler.ServeHTTP(w, r)
}

// staticFile is a file of web.Static, with its ETag.
type staticFile struct {
	content []byte
	etag    string
}

// staticFiles are web.Static's files, by name.
var staticFiles = func() map[string]staticFile {
	files := map[string]staticFile{}
	entries, err := fs.ReadDir(web.Static, ".")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		content, err := fs.ReadFile(web.Static, e.Name())
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(content)
		files[e.Name()] = staticFile{content, `"` + hex.EncodeToString(sum[:12]) + `"`}
	}
	return files
}()

// serveStatic serves a file of web.Static, which browsers check is current
// with its ETag before each use.
func serveStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	f, ok := staticFiles[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", f.etag)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.content))
}

// signedIn serves a request with the signed-in user, or answers an error.
func (s *SSO) signedIn(serve func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
		if u, ok := s.user(w, r); ok {
			serve(w, r, u)
		}
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
	return s.lookUp(w, r, r.Header.Get(s.UserHeader))
}

// lookUp returns the Jellyfin user of the single sign-on's user name, or
// answers an error.
func (s *SSO) lookUp(w http.ResponseWriter, r *http.Request, name string) (user, bool) {
	ctx := r.Context()
	username, ok := name, true
	if len(s.Users) > 0 {
		username, ok = s.Users[name]
	}
	var id string
	var err error
	if name != "" && ok {
		id, err = s.Apps.Jellyfin.UserID(ctx, username)
	}
	if name == "" || !ok || errors.Is(err, jellyfin.ErrNoUser) {
		clog.WarnContext(ctx, "an unknown user", "user", name)
		http.Error(w, "no Jellyfin user for "+name, http.StatusForbidden)
		return user{}, false
	}
	if err != nil {
		clog.ErrorContext(ctx, "finding a Jellyfin user", "user", name, "username", username, "error", err)
		http.Error(w, "Jellyfin unavailable", http.StatusBadGateway)
		return user{}, false
	}
	return user{name: name, id: id, username: username}, true
}

// render answers the template name, executed with data, with status.
func render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	var page bytes.Buffer
	if err := web.Templates.ExecuteTemplate(&page, name, data); err != nil {
		clog.ErrorContext(r.Context(), "rendering a page", "page", name, "error", err)
		http.Error(w, "rendering the page failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(page.Bytes())
}

// sessionResponse is a browser's jellyfin-sso token, named as jellyfin-web's
// credentials name them.
type sessionResponse struct {
	AccessToken string
	UserID      string `json:"UserId"`
	ServerID    string `json:"ServerId"`
	// DeviceID is the browser's, which jellyfin-web is to send.
	DeviceID string `json:"DeviceId"`
	// ExpiresIn is how many seconds the token lasts.
	ExpiresIn int64
	// RefreshBefore is how many seconds before it expires to refresh it.
	RefreshBefore int64
}

// signIn returns a jellyfin-sso token for the user's session on the browser
// deviceCookie names, setting a new one if it names none. The session is
// made if it doesn't exist.
func (s *SSO) signIn(w http.ResponseWriter, r *http.Request, u user) (sessionResponse, error) {
	ctx := r.Context()
	var deviceID string
	if c, err := r.Cookie(deviceCookie); err == nil && c.Value != "" && len(c.Value) <= maxDeviceID {
		deviceID = c.Value
	} else {
		deviceID = rand.Text()
	}
	// Set again, to last deviceCookieAge from its last use.
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookie,
		Value:    deviceID,
		Path:     "/sso/",
		MaxAge:   int(deviceCookieAge / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	session, err := s.browserSession(ctx, u, deviceID)
	if err != nil {
		return sessionResponse{}, fmt.Errorf("making a session: %w", err)
	}
	token, err := s.Apps.Sessions.IssueToken(ctx, session.ID, s.BrowserTTL)
	if err != nil {
		return sessionResponse{}, fmt.Errorf("issuing a token for session %s: %w", session.ID, err)
	}
	clog.InfoContext(ctx, "signed in", "user", u.name, "username", u.username, "session", session.ID)
	return sessionResponse{
		AccessToken:   token,
		UserID:        u.id,
		ServerID:      s.ServerID,
		DeviceID:      deviceID,
		ExpiresIn:     int64(s.BrowserTTL / time.Second),
		RefreshBefore: int64(min(s.RefreshBefore, s.BrowserTTL/2) / time.Second),
	}, nil
}

// browserSession returns the user's session on the browser deviceID,
// logging the browser in to Jellyfin if it has none.
func (s *SSO) browserSession(ctx context.Context, u user, deviceID string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.Apps.Sessions.FindSession(ctx, Browser, u.id, deviceID)
	if !errors.Is(err, errNoSession) {
		return session, err
	}
	device := browserDevice
	device.ID = deviceID
	result, err := s.Apps.Jellyfin.Login(ctx, u.id, device)
	if err != nil {
		return Session{}, err
	}
	return s.Apps.Sessions.CreateSession(ctx, Session{
		Kind:          Browser,
		UserID:        u.id,
		Username:      u.username,
		Device:        device,
		JellyfinToken: result.AccessToken,
	})
}

// loginPage is what login.html shows: a session to store, or an error.
type loginPage struct {
	Session *sessionResponse
	Error   string
}

func (s *SSO) serveLogin(w http.ResponseWriter, r *http.Request, u user) {
	session, err := s.signIn(w, r, u)
	if err != nil {
		clog.ErrorContext(r.Context(), "signing a browser in", "user", u.name, "error", err)
		render(w, r, http.StatusBadGateway, "login.html", loginPage{Error: "Jellyfin didn't sign this browser in."})
		return
	}
	render(w, r, http.StatusOK, "login.html", loginPage{Session: &session})
}

func (s *SSO) serveSession(w http.ResponseWriter, r *http.Request, u user) {
	session, err := s.signIn(w, r, u)
	if err != nil {
		clog.ErrorContext(r.Context(), "signing a browser in", "user", u.name, "error", err)
		http.Error(w, "signing in failed", http.StatusBadGateway)
		return
	}
	reply(w, session)
}

// accountPage is what account.html shows.
type accountPage struct {
	// QuickConnect is the result of approving a Quick Connect code.
	QuickConnect string
	// Code is a one-time code just issued, and CodeError why none was.
	Code      *issuedCode
	CodeError string
	// Sessions are the user's, and SessionsError why they aren't all
	// there, or why one didn't end.
	Sessions      []sessionRow
	SessionsError string
}

// issuedCode is a one-time code for an app without Quick Connect, as the
// account page shows it.
type issuedCode struct {
	Username string
	Code     string
	// Minutes is how long it lasts.
	Minutes int
}

// sessionRow is a session as the account page lists it.
type sessionRow struct {
	ID     string
	Kind   Kind
	Client string
	Device string
	// Ago is how long ago it was last used, roughly.
	Ago string
}

// renderAccount answers the account page, with the user's sessions, and
// with status.
func (s *SSO) renderAccount(w http.ResponseWriter, r *http.Request, u user, status int, page accountPage) {
	ctx := r.Context()
	sessions, err := s.Apps.Sessions.UserSessions(ctx, u.id)
	if err != nil {
		clog.ErrorContext(ctx, "listing sessions", "user", u.name, "error", err)
		page.SessionsError = "Listing your sessions failed."
	}
	now := time.Now()
	for _, session := range sessions {
		page.Sessions = append(page.Sessions, sessionRow{
			ID:     session.ID,
			Kind:   session.Kind,
			Client: session.Device.Client,
			Device: session.Device.Name,
			Ago:    ago(now.Sub(session.LastUsed)),
		})
	}
	render(w, r, status, "account.html", page)
}

// ago says how long ago something d ago was, roughly.
func ago(d time.Duration) string {
	minutes := int(d.Round(time.Minute) / time.Minute)
	switch hours := int(d.Round(time.Hour) / time.Hour); {
	case minutes < 2:
		return "just now"
	case minutes < 120:
		return fmt.Sprintf("%d minutes ago", minutes)
	case hours < 48:
		return fmt.Sprintf("%d hours ago", hours)
	default:
		return fmt.Sprintf("%d days ago", int(d.Round(24*time.Hour)/(24*time.Hour)))
	}
}

func (s *SSO) serveAccount(w http.ResponseWriter, r *http.Request, u user) {
	s.renderAccount(w, r, u, http.StatusOK, accountPage{})
}

func (s *SSO) serveQuickConnect(w http.ResponseWriter, r *http.Request, u user) {
	ctx := r.Context()
	code := strings.TrimSpace(r.PostFormValue("code"))
	err := s.Apps.Jellyfin.AuthorizeQuickConnect(ctx, code, u.id)
	switch {
	case jellyfin.IsStatus(err, http.StatusNotFound):
		s.renderAccount(w, r, u, http.StatusNotFound, accountPage{QuickConnect: "Not signed in: no app is showing that code."})
	case err != nil:
		clog.ErrorContext(ctx, "approving a Quick Connect code", "user", u.name, "error", err)
		s.renderAccount(w, r, u, http.StatusBadGateway, accountPage{QuickConnect: "Not signed in: Jellyfin refused the code."})
	default:
		clog.InfoContext(ctx, "approved a Quick Connect code", "user", u.name, "username", u.username)
		s.renderAccount(w, r, u, http.StatusOK, accountPage{QuickConnect: "Signed in. The app continues on its own."})
	}
}

func (s *SSO) serveCode(w http.ResponseWriter, r *http.Request, u user) {
	code, expires := s.Apps.IssueCode(u.id, u.username)
	clog.InfoContext(r.Context(), "issued an app code", "user", u.name, "username", u.username)
	s.renderAccount(w, r, u, http.StatusOK, accountPage{Code: &issuedCode{
		Username: u.username,
		Code:     code,
		Minutes:  int(time.Until(expires).Round(time.Minute) / time.Minute),
	}})
}

func (s *SSO) serveRevoke(w http.ResponseWriter, r *http.Request, u user) {
	ctx := r.Context()
	session, err := s.Apps.Sessions.UserSession(ctx, u.id, r.PostFormValue("id"))
	if err == nil {
		err = EndSession(ctx, s.Apps.Sessions, s.Apps.Jellyfin, session)
	}
	switch {
	case errors.Is(err, errNoSession):
		s.renderAccount(w, r, u, http.StatusNotFound, accountPage{SessionsError: "That session had ended already."})
	case err != nil:
		clog.ErrorContext(ctx, "ending a session", "user", u.name, "error", err)
		s.renderAccount(w, r, u, http.StatusBadGateway, accountPage{SessionsError: "Signing it out failed."})
	default:
		clog.InfoContext(ctx, "revoked a session", "user", u.name, "session", session.ID,
			"client", session.Device.Client, "device", session.Device.Name)
		s.renderAccount(w, r, u, http.StatusOK, accountPage{})
	}
}
