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

	if err := s.DeleteExpiredTokens(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM tokens`).Scan(&n); err != nil || n != 1 {
		t.Errorf("tokens left after DeleteExpiredTokens: %d, %v", n, err)
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
		if _, err := s.Lookup(ctx, usedToken); err != nil {
			t.Fatalf("Lookup after 30 days: %v", err)
		}
		if err := s.Touch(ctx, used.ID); err != nil {
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
	if err := s.db.QueryRow(`SELECT count(*) FROM tokens WHERE session_id = ?`, unused.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("tokens left after DeleteSession: %d, %v", n, err)
	}
}

func TestStoreTouchThrottles(t *testing.T) {
	ctx := context.Background()
	s, c := openStore(t)
	session := newSession(t, s, App)
	lastUsed := func() int64 {
		var v int64
		if err := s.db.QueryRow(`SELECT last_used FROM sessions WHERE id = ?`, session.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	c.t = c.t.Add(time.Hour)
	s.Touch(ctx, session.ID)
	first := lastUsed()
	if first != c.t.Unix() {
		t.Errorf("last_used %d, want %d", first, c.t.Unix())
	}
	c.t = c.t.Add(time.Minute)
	s.Touch(ctx, session.ID)
	if got := lastUsed(); got != first {
		t.Errorf("Touch a minute later wrote last_used %d", got)
	}
	c.t = c.t.Add(touchEvery)
	s.Touch(ctx, session.ID)
	if got := lastUsed(); got != c.t.Unix() {
		t.Errorf("Touch after touchEvery: last_used %d, want %d", got, c.t.Unix())
	}
}
