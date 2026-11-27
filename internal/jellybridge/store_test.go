package jellybridge

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pdeslaur/hangar/internal/jellyfin"
)

// clock is a time tests move by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// openStore opens a store in a temporary directory, its time from a clock.
func openStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "sessions.db"), 45*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	s.now = c.now
	return s, c
}

func newSession(t *testing.T, s *Store, kind Kind) Session {
	t.Helper()
	session, err := s.CreateSession(context.Background(), Session{
		Kind:          kind,
		UserID:        "u1",
		Username:      "alice",
		Device:        jellyfin.Device{Client: "Jellyfin Web", Name: "Firefox", ID: "dev1", Version: "10.11.0"},
		JellyfinToken: "real",
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestStoreTokens(t *testing.T) {
	ctx := context.Background()
	s, c := openStore(t)
	session := newSession(t, s, Browser)

	short, err := s.IssueToken(ctx, session.ID, 8*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(short) != 32 {
		t.Errorf("token %q isn't 32 hex digits", short)
	}
	got, err := s.Lookup(ctx, short)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != session {
		t.Errorf("Lookup = %+v, want %+v", got, session)
	}

	for _, token := range []string{"", "unknown", "real"} {
		if _, err := s.Lookup(ctx, token); !errors.Is(err, errNoSession) {
			t.Errorf("Lookup(%q): %v, want errNoSession", token, err)
		}
	}

	// A refresh: the second token outlives the first, on the same session.
	c.t = c.t.Add(7 * time.Hour)
	next, err := s.IssueToken(ctx, session.ID, 8*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(2 * time.Hour)
	if _, err := s.Lookup(ctx, short); !errors.Is(err, errNoSession) {
		t.Errorf("Lookup of an expired token: %v", err)
	}
	if got, err := s.Lookup(ctx, next); err != nil || got.ID != session.ID {
		t.Errorf("Lookup of the refreshed token: %+v, %v", got, err)
	}

	// A refused token's session, while the bridge keeps it.
	got, expires, err := s.RefusedToken(ctx, short)
	if err != nil || got.ID != session.ID || expires == nil || !expires.Equal(c.t.Add(-time.Hour)) {
		t.Errorf("RefusedToken of the expired token: %+v, %v, %v", got, expires, err)
	}
	for _, token := range []string{"", "unknown", "real"} {
		if _, _, err := s.RefusedToken(ctx, token); !errors.Is(err, errNoSession) {
			t.Errorf("RefusedToken(%q): %v, want errNoSession", token, err)
		}
	}

	// An expired token stays a day, for RefusedToken.
	countTokens := func() int {
		t.Helper()
		var n int
		if err := s.db.Raw(`SELECT count(*) FROM tokens`).Row().Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := s.DeleteExpiredTokens(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countTokens(); n != 2 {
		t.Errorf("tokens left after DeleteExpiredTokens, within a day: %d", n)
	}
	c.t = c.t.Add(24 * time.Hour)
	if err := s.DeleteExpiredTokens(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countTokens(); n != 1 {
		t.Errorf("tokens left after DeleteExpiredTokens, past a day: %d", n)
	}
	if _, _, err := s.RefusedToken(ctx, short); !errors.Is(err, errNoSession) {
		t.Errorf("RefusedToken of a deleted token: %v", err)
	}
}

func TestStoreIdle(t *testing.T) {
	ctx := context.Background()
	s, c := openStore(t)
	used := newSession(t, s, App)
	unused := newSession(t, s, App)
	usedToken, _ := s.IssueToken(ctx, used.ID, 0)
	unusedToken, _ := s.IssueToken(ctx, unused.ID, 0)

	// Used every 30 days, a session lasts; unused for 45, it ends.
	for range 2 {
		c.t = c.t.Add(30 * 24 * time.Hour)
		session, err := s.Lookup(ctx, usedToken)
		if err != nil {
			t.Fatalf("Lookup after 30 days: %v", err)
		}
		if err := s.Touch(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Lookup(ctx, unusedToken); !errors.Is(err, errNoSession) {
		t.Errorf("Lookup of a session idle 60 days: %v", err)
	}

	idle, err := s.IdleSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(idle) != 1 || idle[0].ID != unused.ID || idle[0].JellyfinToken != "real" {
		t.Fatalf("IdleSessions = %+v, want the unused session", idle)
	}
	if err := s.DeleteSession(ctx, unused.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.Raw(`SELECT count(*) FROM tokens WHERE session_id = ?`, unused.ID).Row().Scan(&n); err != nil || n != 0 {
		t.Errorf("tokens left after DeleteSession: %d, %v", n, err)
	}
	if err := s.DeleteSession(ctx, unused.ID); !errors.Is(err, errNoSession) {
		t.Errorf("DeleteSession of a deleted session: %v, want errNoSession", err)
	}
}

func TestStoreTouchThrottles(t *testing.T) {
	ctx := context.Background()
	s, c := openStore(t)
	session := newSession(t, s, App)
	// current is the session as stored, as Lookup returns it.
	current := func() Session {
		var got Session
		if err := s.db.Take(&got, "id = ?", session.ID).Error; err != nil {
			t.Fatal(err)
		}
		return got
	}
	lastUsed := func() time.Time { return current().LastUsed }

	c.t = c.t.Add(time.Hour)
	s.Touch(ctx, current())
	first := lastUsed()
	if !first.Equal(c.t) {
		t.Errorf("last_used %v, want %v", first, c.t)
	}
	c.t = c.t.Add(time.Minute)
	s.Touch(ctx, current())
	if got := lastUsed(); !got.Equal(first) {
		t.Errorf("Touch a minute later wrote last_used %v", got)
	}
	c.t = c.t.Add(touchEvery)
	s.Touch(ctx, current())
	if got := lastUsed(); !got.Equal(c.t) {
		t.Errorf("Touch after touchEvery: last_used %v, want %v", got, c.t)
	}
}
