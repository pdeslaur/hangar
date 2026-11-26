package jellybridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/pdeslaur/hangar/internal/jellyfin"
	_ "modernc.org/sqlite"
)

// Kind is what a session was made for, which sets how its tokens expire.
type Kind string

const (
	// Browser is jellyfin-web, its tokens short-lived and refreshed.
	Browser Kind = "browser"
	// App is a native app, its token lasting until revoked.
	App Kind = "app"
)

// Session is one user's device: the Jellyfin token the bridge holds for it,
// and the bridge tokens it handed out for it.
type Session struct {
	ID   string
	Kind Kind
	// UserID and Username are the Jellyfin user's.
	UserID   string
	Username string
	// Device is as the bridge logged it in to Jellyfin.
	Device jellyfin.Device
	// JellyfinToken is the device's Jellyfin access token, which never
	// leaves the bridge.
	JellyfinToken string
	Created       time.Time
	// LastUsed is when a request last carried one of its tokens, to within
	// touchEvery.
	LastUsed time.Time
}

// touchEvery is how often a session's LastUsed is written while in use:
// a video plays as many requests.
const touchEvery = 5 * time.Minute

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id             TEXT PRIMARY KEY,
	kind           TEXT NOT NULL,
	user_id        TEXT NOT NULL,
	username       TEXT NOT NULL,
	client         TEXT NOT NULL,
	device_name    TEXT NOT NULL,
	device_id      TEXT NOT NULL,
	version        TEXT NOT NULL,
	jellyfin_token TEXT NOT NULL,
	created        INTEGER NOT NULL,
	last_used      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	hash       BLOB PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	-- Unix seconds; NULL never expires.
	expires    INTEGER
);
CREATE INDEX IF NOT EXISTS tokens_session ON tokens (session_id);
`

// Store keeps sessions and their tokens in a SQLite database. A token is
// kept as its SHA-256 hash, so the database doesn't hold bridge tokens.
type Store struct {
	db *sql.DB
	// Idle is how long a session lasts without use.
	Idle time.Duration
	// now is the time, replaced in tests.
	now func() time.Time

	mu sync.Mutex
	// touched is when each session's LastUsed was last written.
	touched map[string]time.Time
}

// Open opens or creates the store's database at path.
func Open(ctx context.Context, path string, idle time.Duration) (*Store, error) {
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, Idle: idle, now: time.Now, touched: map[string]time.Time{}}, nil
}

// Close closes its database.
func (s *Store) Close() error {
	return s.db.Close()
}

// randomHex returns n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// CreateSession stores session, giving it an ID and its times, and returns
// it.
func (s *Store) CreateSession(ctx context.Context, session Session) (Session, error) {
	now := s.now().UTC().Truncate(time.Second)
	session.ID = randomHex(16)
	session.Created = now
	session.LastUsed = now
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, kind, user_id, username, client, device_name, device_id, version, jellyfin_token, created, last_used)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.Kind, session.UserID, session.Username,
		session.Device.Client, session.Device.Name, session.Device.ID, session.Device.Version,
		session.JellyfinToken, now.Unix(), now.Unix())
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

// IssueToken returns a new bridge token for the session id, lasting ttl, or
// until its session ends if ttl is 0. Like Jellyfin's own, it is 32 hex
// digits.
func (s *Store) IssueToken(ctx context.Context, id string, ttl time.Duration) (string, error) {
	token := randomHex(16)
	var expires any
	if ttl > 0 {
		expires = s.now().Add(ttl).Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens (hash, session_id, expires) VALUES (?, ?, ?)`,
		hashToken(token), id, expires)
	if err != nil {
		return "", err
	}
	return token, nil
}

// errNoSession is a token that names no live session.
var errNoSession = errors.New("jellybridge: no session")

// Lookup returns the session of token, if the token hasn't expired and its
// session hasn't been idle past Idle, and errNoSession otherwise.
func (s *Store) Lookup(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, errNoSession
	}
	now := s.now()
	session, err := scanSession(s.db.QueryRowContext(ctx, `
		SELECT `+sessionColumns+`
		FROM tokens t JOIN sessions s ON s.id = t.session_id
		WHERE t.hash = ? AND (t.expires IS NULL OR t.expires > ?) AND s.last_used > ?`,
		hashToken(token), now.Unix(), now.Add(-s.Idle).Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, errNoSession
	}
	return session, err
}

// Touch records that the session id is in use, writing it at most every
// touchEvery.
func (s *Store) Touch(ctx context.Context, id string) error {
	now := s.now()
	s.mu.Lock()
	if now.Sub(s.touched[id]) < touchEvery {
		s.mu.Unlock()
		return nil
	}
	s.touched[id] = now
	s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_used = ? WHERE id = ?`, now.Unix(), id)
	return err
}

// IdleSessions returns the sessions idle past Idle, which the caller logs
// out of Jellyfin and deletes.
func (s *Store) IdleSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+sessionColumns+`
		FROM sessions s WHERE s.last_used <= ?`, s.now().Add(-s.Idle).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// sessionColumns are a session's columns, of the table aliased s, in the
// order scanSession reads them.
const sessionColumns = `s.id, s.kind, s.user_id, s.username, s.client, s.device_name, s.device_id, s.version, s.jellyfin_token, s.created, s.last_used`

// scanSession reads a row of sessionColumns.
func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var session Session
	var created, lastUsed int64
	err := row.Scan(&session.ID, &session.Kind, &session.UserID, &session.Username,
		&session.Device.Client, &session.Device.Name, &session.Device.ID, &session.Device.Version,
		&session.JellyfinToken, &created, &lastUsed)
	if err != nil {
		return Session{}, err
	}
	session.Created = time.Unix(created, 0).UTC()
	session.LastUsed = time.Unix(lastUsed, 0).UTC()
	return session, nil
}

// DeleteSession deletes the session id and its tokens.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.touched, id)
	s.mu.Unlock()
	return nil
}

// DeleteExpiredTokens deletes the tokens past their expiry.
func (s *Store) DeleteExpiredTokens(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE expires IS NOT NULL AND expires <= ?`, s.now().Unix())
	return err
}
