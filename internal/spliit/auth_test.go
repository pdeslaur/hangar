package spliit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeSpliit issues single-use refresh tokens, like Spliit does, and serves
// groups.get to valid access tokens.
type fakeSpliit struct {
	mu      sync.Mutex
	n       int
	refresh string // the only refresh token that still works
	access  string
}

func (f *fakeSpliit) issue(w http.ResponseWriter) {
	f.n++
	f.refresh = fmt.Sprintf("refresh-%d", f.n)
	f.access = fmt.Sprintf("access-%d", f.n)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token":  f.access,
		"refresh_token": f.refresh,
		"token_type":    "Bearer",
		// Shorter than oauth2's expiry delta, so every call refreshes.
		"expires_in": 1,
	})
}

func (f *fakeSpliit) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/oauth2/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"client_id": "client-1"})
	})
	mux.HandleFunc("POST /auth/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != f.refresh {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
		}
		f.issue(w)
	})
	mux.HandleFunc("GET /trpc/groups.get", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.access {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"json":{"message":"Unauthorized"}}}`)
			return
		}
		fmt.Fprint(w, `{"result":{"data":{"json":{"group":{"currency":"$","currencyCode":"USD"},"currentLedgerParticipantId":"me"}}}}`)
	})
	return mux
}

func login(t *testing.T, a *Auth) {
	t.Helper()
	ctx := context.Background()
	u, err := a.LoginURL(ctx)
	if err != nil {
		t.Fatalf("LoginURL() = %v", err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Callback(ctx, parsed.Query().Get("state"), "good-code"); err != nil {
		t.Fatalf("Callback() = %v", err)
	}
}

func storedRefreshToken(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s storedToken
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s.RefreshToken
}

func TestAuth(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSpliit{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	state := filepath.Join(t.TempDir(), "token.json")

	a, err := NewAuth(srv.URL, "http://127.0.0.1/callback", state)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(srv.URL, a)

	if _, err := c.Group(ctx, "g"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("Group() before login = %v, want ErrNotAuthorized", err)
	}

	login(t, a)
	if got := storedRefreshToken(t, state); got != "refresh-1" {
		t.Errorf("stored refresh token after login = %q, want refresh-1", got)
	}

	info, err := c.Group(ctx, "g")
	if err != nil {
		t.Fatalf("Group() = %v", err)
	}
	if info.CurrentLedgerParticipantID != "me" {
		t.Errorf("CurrentLedgerParticipantID = %q, want me", info.CurrentLedgerParticipantID)
	}
	if got := storedRefreshToken(t, state); got != fake.refresh {
		t.Errorf("stored refresh token after refresh = %q, want %q", got, fake.refresh)
	}

	// A restarted service picks up the rotated token rather than replaying a
	// spent one.
	restarted, err := NewAuth(srv.URL, "http://127.0.0.1/callback", state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(srv.URL, restarted).Group(ctx, "g"); err != nil {
		t.Fatalf("Group() after restart = %v", err)
	}
}

func TestCallbackRejectsUnknownState(t *testing.T) {
	srv := httptest.NewServer((&fakeSpliit{}).handler())
	defer srv.Close()
	a, err := NewAuth(srv.URL, "http://127.0.0.1/callback", filepath.Join(t.TempDir(), "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.LoginURL(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Callback(context.Background(), "forged", "good-code"); err == nil {
		t.Error("Callback() with forged state succeeded, want error")
	}
}
