package jellybridge

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
	// maxLoginRequest bounds the body of a password login.
	maxLoginRequest = 4 << 10
)

// Apps signs native apps in, with bridge tokens lasting until revoked:
//
//   - An app with Quick Connect signs in with Jellyfin's own, approved on
//     the bridge's page or in jellyfin-web; Adopt turns the result Jellyfin
//     answers into a bridge session.
//   - An app without, like Infuse, signs in with a username and password:
//     ServePasswordLogin takes a one-time code from the bridge's page as the
//     password, in place of Jellyfin's passwords, which never reach it.
type Apps struct {
	Sessions *Store
	Jellyfin *jellyfin.Client
	// now is the time, replaced in tests.
	now func() time.Time

	mu sync.Mutex
	// codes are the live one-time codes.
	codes map[string]appCode
	// failures counts the wrong codes sent for each username, lowercase,
	// that has a code.
	failures map[string]int
}

// appCode is a one-time code's user.
type appCode struct {
	userID   string
	username string
	expires  time.Time
}

// NewApps returns an Apps keeping its sessions in sessions.
func NewApps(sessions *Store, client *jellyfin.Client) *Apps {
	return &Apps{
		Sessions: sessions,
		Jellyfin: client,
		now:      time.Now,
		codes:    map[string]appCode{},
		failures: map[string]int{},
	}
}

// IssueCode returns a one-time code signing an app in as the Jellyfin user
// userID, called username, replacing the code it had, and when it expires.
func (a *Apps) IssueCode(userID, username string) (string, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for code, c := range a.codes {
		if c.userID == userID || !now.Before(c.expires) {
			delete(a.codes, code)
		}
	}
	delete(a.failures, strings.ToLower(username))
	var code string
	for {
		n, err := rand.Int(rand.Reader, big.NewInt(1e8))
		if err != nil {
			panic(err)
		}
		code = fmt.Sprintf("%0*d", appCodeDigits, n)
		if _, taken := a.codes[code]; !taken {
			break
		}
	}
	expires := now.Add(appCodeTTL)
	a.codes[code] = appCode{userID: userID, username: username, expires: expires}
	return code, expires
}

// redeem uses up code if it is username's, and returns its user. After
// maxCodeFailures wrong codes, username's code is voided.
func (a *Apps) redeem(username, code string) (appCode, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := strings.ToLower(username)
	now := a.now()
	c, ok := a.codes[code]
	if ok && now.Before(c.expires) && strings.EqualFold(c.username, username) && a.failures[key] < maxCodeFailures {
		delete(a.codes, code)
		delete(a.failures, key)
		return c, true
	}
	// Count the failure if username has a code to guess: IssueCode keeps
	// one per user.
	for k, c := range a.codes {
		if !strings.EqualFold(c.username, username) {
			continue
		}
		a.failures[key]++
		if a.failures[key] >= maxCodeFailures {
			delete(a.codes, k)
		}
		break
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginRequest)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if device.Client == "" || device.Name == "" || device.ID == "" || device.Version == "" {
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out)
}

// Adopt makes an app session holding the Jellyfin token of result, a login
// Jellyfin answered, and returns the result with a bridge token in its
// place. Jellyfin ended the tokens of the same user's earlier login on the
// same device, so their sessions go too.
func (a *Apps) Adopt(ctx context.Context, result *jellyfin.AuthenticationResult) ([]byte, error) {
	if result.Device.ID == "" {
		return nil, errors.New("jellybridge: a login without its device")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result.Raw, &fields); err != nil {
		return nil, err
	}
	if _, err := a.Sessions.DeleteDeviceSessions(ctx, App, result.UserID, result.Device.ID); err != nil {
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
