package spliit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// ErrNotAuthorized means no one has signed in through the login flow yet.
var ErrNotAuthorized = errors.New("not signed in to Spliit: visit /login")

var scopes = []string{"openid", "offline_access", "spliit:groups:read", "spliit:expenses:read"}

// Auth holds the OAuth grant for Spliit. Spliit only issues tokens through the
// authorization code flow, and its refresh tokens are single-use: each refresh
// returns a new one, and replaying an old one revokes the grant. Auth therefore
// serializes refreshes and writes every new refresh token to statePath before
// handing out the access token that came with it.
type Auth struct {
	baseURL     string
	redirectURL string
	statePath   string
	http        *http.Client

	mu      sync.Mutex
	stored  storedToken
	source  oauth2.TokenSource // nil until signed in
	pending *pendingLogin
}

type storedToken struct {
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
}

type pendingLogin struct {
	state, verifier, clientID string
}

// NewAuth loads a previously stored grant from statePath, if there is one.
// redirectURL is where Spliit sends the browser back to, and must be served by
// Callback.
func NewAuth(baseURL, redirectURL, statePath string) (*Auth, error) {
	a := &Auth{
		baseURL:     baseURL,
		redirectURL: redirectURL,
		statePath:   statePath,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: userAgentTransport{http.DefaultTransport},
		},
	}
	b, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &a.stored); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", statePath, err)
	}
	a.source = a.config(a.stored.ClientID).TokenSource(a.ctx(), &oauth2.Token{RefreshToken: a.stored.RefreshToken})
	return a, nil
}

// LoginURL registers a fresh OAuth client with Spliit and returns the URL to
// send the browser to.
func (a *Auth) LoginURL(ctx context.Context) (string, error) {
	clientID, err := a.register(ctx)
	if err != nil {
		return "", fmt.Errorf("registering OAuth client: %w", err)
	}
	p := &pendingLogin{
		state:    oauth2.GenerateVerifier(),
		verifier: oauth2.GenerateVerifier(),
		clientID: clientID,
	}
	a.mu.Lock()
	a.pending = p
	a.mu.Unlock()
	return a.config(clientID).AuthCodeURL(p.state, oauth2.S256ChallengeOption(p.verifier)), nil
}

// Callback completes the login started by LoginURL.
func (a *Auth) Callback(ctx context.Context, state, code string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pending
	if p == nil || state != p.state {
		return errors.New("unknown login state: start again at /login")
	}
	a.pending = nil

	cfg := a.config(p.clientID)
	tok, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, a.http), code, oauth2.VerifierOption(p.verifier))
	if err != nil {
		return fmt.Errorf("exchanging code: %w", err)
	}
	if tok.RefreshToken == "" {
		return errors.New("spliit returned no refresh token")
	}
	if err := a.save(storedToken{ClientID: p.clientID, RefreshToken: tok.RefreshToken}); err != nil {
		return err
	}
	a.source = cfg.TokenSource(a.ctx(), tok)
	return nil
}

// Token returns a valid access token, refreshing and persisting the grant when
// needed. It implements oauth2.TokenSource.
func (a *Auth) Token() (*oauth2.Token, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.source == nil {
		return nil, ErrNotAuthorized
	}
	tok, err := a.source.Token()
	if err != nil {
		return nil, fmt.Errorf("refreshing Spliit token (visit /login if this persists): %w", err)
	}
	if tok.RefreshToken != a.stored.RefreshToken {
		if err := a.save(storedToken{ClientID: a.stored.ClientID, RefreshToken: tok.RefreshToken}); err != nil {
			return nil, err
		}
	}
	return tok, nil
}

func (a *Auth) httpClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &oauth2.Transport{Source: a, Base: a.http.Transport},
	}
}

func (a *Auth) config(clientID string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: clientID,
		Endpoint: oauth2.Endpoint{
			AuthURL:   a.baseURL + "/auth/oauth2/authorize",
			TokenURL:  a.baseURL + "/auth/oauth2/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		RedirectURL: a.redirectURL,
		Scopes:      scopes,
	}
}

// ctx carries the HTTP client for token refreshes, which outlive any request.
func (a *Auth) ctx() context.Context {
	return context.WithValue(context.Background(), oauth2.HTTPClient, a.http)
}

// register enrolls a public client through dynamic client registration.
func (a *Auth) register(ctx context.Context) (string, error) {
	body, err := json.Marshal(map[string]any{
		"client_name":                "Spliit for YNAB",
		"redirect_uris":              []string{a.redirectURL},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      strings.Join(scopes, " "),
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/auth/oauth2/register", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ClientID         string `json:"client_id"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("%s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, out.ErrorDescription)
	}
	return out.ClientID, nil
}

// save writes the grant atomically, so a crash never leaves a torn file.
func (a *Auth) save(t storedToken) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.statePath), ".token-*")
	if err != nil {
		return fmt.Errorf("saving Spliit token: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("saving Spliit token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("saving Spliit token: %w", err)
	}
	if err := os.Rename(tmp.Name(), a.statePath); err != nil {
		return fmt.Errorf("saving Spliit token: %w", err)
	}
	a.stored = t
	return nil
}

type userAgentTransport struct {
	base http.RoundTripper
}

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", userAgent)
	return t.base.RoundTrip(req)
}
