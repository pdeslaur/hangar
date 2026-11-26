package jellybridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pdeslaur/hangar/internal/jellyfin"
)

func TestSweep(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var loggedOut []string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Sessions/Logout" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		loggedOut = append(loggedOut, parseAuthorization(r.Header.Get("Authorization"))["Token"])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &jellyfin.Client{URL: srv.URL, HTTP: srv.Client()}

	s, c := openStore(t)
	s.CreateSession(ctx, Session{Kind: App, UserID: "u1", JellyfinToken: "idle-token"})
	used, _ := s.CreateSession(ctx, Session{Kind: Browser, UserID: "u1", JellyfinToken: "used-token"})
	short, _ := s.IssueToken(ctx, used.ID, time.Hour)
	c.t = c.t.Add(40 * 24 * time.Hour)
	s.Touch(ctx, used)
	c.t = c.t.Add(10 * 24 * time.Hour)

	// Jellyfin down: the idle session stays, for the next sweep.
	mu.Lock()
	fail = true
	mu.Unlock()
	if err := Sweep(ctx, s, client); err == nil {
		t.Error("Sweep with Jellyfin failing succeeded")
	}
	if sessions, _ := s.IdleSessions(ctx); len(sessions) != 1 {
		t.Errorf("idle sessions after a failed sweep: %d", len(sessions))
	}

	mu.Lock()
	fail = false
	mu.Unlock()
	if err := Sweep(ctx, s, client); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(loggedOut) != 1 || loggedOut[0] != "idle-token" {
		t.Errorf("logged out %v, want the idle session's token", loggedOut)
	}
	if sessions, _ := s.IdleSessions(ctx); len(sessions) != 0 {
		t.Errorf("idle sessions after the sweep: %d", len(sessions))
	}
	if _, err := s.FindSession(ctx, App, "u1", ""); !errors.Is(err, errNoSession) {
		t.Errorf("the idle session after the sweep: %v", err)
	}
	var tokens int
	s.db.Raw(`SELECT count(*) FROM tokens WHERE hash = ?`, hashToken(short)).Row().Scan(&tokens)
	if tokens != 0 {
		t.Error("the expired token outlived the sweep")
	}
}
