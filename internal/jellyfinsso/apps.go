package jellyfinsso

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/jellyfin"
)

const (
	// appCodeTTL is how long a one-time code lasts.
	appCodeTTL = 10 * time.Minute
	// appCodeDigits is a one-time code's length: digits, for a TV remote.
	appCodeDigits = 8
	// maxCodeFailures is how many wrong codes a username may send before
	// its code is voided.
	maxCodeFailures = 5
	// usedCodeMemory is how long a used code is remembered: an app keeps
	// sending the code it signed in with when its token stops working.
	usedCodeMemory = 90 * 24 * time.Hour
)

// Apps signs native apps in, with jellyfin-sso tokens lasting until revoked:
//
//   - An app with Quick Connect signs in with Jellyfin's own, approved on
//     jellyfin-sso's page or in jellyfin-web; Adopt turns the result Jellyfin
//     answers into a jellyfin-sso session.
//   - An app without, like Infuse, signs in with a username and password:
//     ServePasswordLogin takes a one-time code from jellyfin-sso's page as the
//     password, in place of Jellyfin's passwords, which never reach it.
type Apps struct {
	Sessions *Store
	Jellyfin *jellyfin.Client
	// now is the time, replaced in tests.
	now func() time.Time

	mu sync.Mutex
	// codes are the one-time codes, one per username, lowercase.
	codes map[string]*appCode
	// used are the codes used, and when: an app sending one again isn't
	// guessing.
	used map[usedCode]time.Time
}

// appCode is a one-time code and its user.
type appCode struct {
	code     string
	userID   string
	username string
	expires  time.Time
	// failures counts the wrong codes sent for its username.
	failures int
}

// usedCode is a code a username, lowercase, used.
type usedCode struct {
	username, code string
}

// NewApps returns an Apps keeping its sessions in sessions.
func NewApps(sessions *Store, client *jellyfin.Client) *Apps {
	return &Apps{
		Sessions: sessions,
		Jellyfin: client,
		now:      time.Now,
		codes:    map[string]*appCode{},
		used:     map[usedCode]time.Time{},
	}
}

// IssueCode returns a one-time code signing an app in as the Jellyfin user
// userID, called username, replacing the code it had, and when it expires.
func (a *Apps) IssueCode(userID, username string) (string, time.Time) {
	n, err := rand.Int(rand.Reader, big.NewInt(1e8))
	if err != nil {
		panic(err)
	}
	code := fmt.Sprintf("%0*d", appCodeDigits, n)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, c := range a.codes {
		if !now.Before(c.expires) {
			delete(a.codes, k)
		}
	}
	for k, when := range a.used {
		if now.Sub(when) > usedCodeMemory {
			delete(a.used, k)
		}
	}
	expires := now.Add(appCodeTTL)
	a.codes[strings.ToLower(username)] = &appCode{code: code, userID: userID, username: username, expires: expires}
	return code, expires
}

// redeem uses up code if it is username's, and returns its user. After
// maxCodeFailures wrong codes, username's code is voided. A code username
// used before isn't a wrong one: Infuse sends the one it signed in with
// again whenever its token stops working.
func (a *Apps) redeem(username, code string) (appCode, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := strings.ToLower(username)
	now := a.now()
	c, ok := a.codes[key]
	if ok && c.code == code && now.Before(c.expires) && c.failures < maxCodeFailures {
		delete(a.codes, key)
		a.used[usedCode{key, code}] = now
		return *c, true
	}
	if _, used := a.used[usedCode{key, code}]; used || !ok {
		return appCode{}, false
	}
	c.failures++
	if c.failures >= maxCodeFailures {
		delete(a.codes, key)
	}
	return appCode{}, false
}

// ServePasswordLogin answers POST /Users/AuthenticateByName, a password
// login from device, as Jellyfin would, if the password is a one-time code
// of the username's.
func (a *Apps) ServePasswordLogin(w http.ResponseWriter, r *http.Request, device jellyfin.Device) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Jellyfin binds JSON without case, as encoding/json does.
	var body struct {
		Username string
		Pw       string
	}
	if !decode(w, r, &body) {
		return
	}
	if !device.Complete() {
		http.Error(w, "the Authorization header has to name the device", http.StatusBadRequest)
		return
	}
	code, ok := a.redeem(body.Username, strings.TrimSpace(body.Pw))
	if !ok {
		clog.WarnContext(ctx, "an app sent a wrong code", "username", body.Username, "client", device.Client, "device", device.Name)
		http.Error(w, "Invalid username or password entered.", http.StatusUnauthorized)
		return
	}
	result, err := a.Jellyfin.Login(ctx, code.userID, device)
	if err != nil {
		clog.ErrorContext(ctx, "logging an app in", "username", code.username, "error", err)
		http.Error(w, "Jellyfin unavailable", http.StatusBadGateway)
		return
	}
	out, err := a.Adopt(ctx, result)
	if err != nil {
		clog.ErrorContext(ctx, "making an app's session", "username", code.username, "error", err)
		http.Error(w, "making a session failed", http.StatusInternalServerError)
		return
	}
	reply(w, json.RawMessage(out))
}

// Adopt makes an app session holding the Jellyfin token of result, a login
// Jellyfin answered, and returns the result with a jellyfin-sso token in its
// place. Jellyfin ended the tokens of the same user's earlier login on the
// same device, so their sessions go too.
func (a *Apps) Adopt(ctx context.Context, result *jellyfin.AuthenticationResult) ([]byte, error) {
	if result.Device.ID == "" {
		return nil, errors.New("jellyfinsso: a login without its device")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result.Raw, &fields); err != nil {
		return nil, err
	}
	if err := a.Sessions.DeleteDeviceSessions(ctx, App, result.UserID, result.Device.ID); err != nil {
		return nil, err
	}
	session, err := a.Sessions.CreateSession(ctx, Session{
		Kind:          App,
		UserID:        result.UserID,
		Username:      result.Username,
		Device:        result.Device,
		JellyfinToken: result.AccessToken,
	})
	if err != nil {
		return nil, err
	}
	token, err := a.Sessions.IssueToken(ctx, session.ID, 0)
	if err != nil {
		return nil, err
	}
	fields["AccessToken"], err = json.Marshal(token)
	if err != nil {
		return nil, err
	}
	clog.InfoContext(ctx, "signed an app in", "username", result.Username, "client", result.Device.Client,
		"device", result.Device.Name, "session", session.ID)
	return json.Marshal(fields)
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
