package jellybridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/pdeslaur/hangar/internal/jellyfin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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
	ID   string `gorm:"primaryKey"`
	Kind Kind   `gorm:"not null"`
	// UserID and Username are the Jellyfin user's.
	UserID   string `gorm:"not null"`
	Username string `gorm:"not null"`
	// Device is as the bridge logged it in to Jellyfin.
	Device jellyfin.Device `gorm:"embedded;embeddedPrefix:device_"`
	// JellyfinToken is the device's Jellyfin access token, which never
	// leaves the bridge.
	JellyfinToken string    `gorm:"not null"`
	Created       time.Time `gorm:"not null"`
	// LastUsed is when a request last carried one of its tokens, to within
	// touchEvery.
	LastUsed time.Time `gorm:"not null"`
}

// token is a bridge token, kept as its SHA-256 hash so the database doesn't
// hold bridge tokens.
type token struct {
	Hash      []byte   `gorm:"primaryKey"`
	SessionID string   `gorm:"not null;index"`
	Session   *Session `gorm:"constraint:OnDelete:CASCADE"`
	// Expires is nil for a token that never expires.
	Expires *time.Time
}

// touchEvery is how often a session's LastUsed is written while in use:
// a video plays as many requests.
const touchEvery = 5 * time.Minute

// Store keeps sessions and their tokens in a SQLite database.
type Store struct {
	db *gorm.DB
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
	db, err := gorm.Open(sqlite.New(sqlite.Config{DriverName: "sqlite", DSN: dsn}), &gorm.Config{
		Logger:                 logger.Discard,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, Idle: idle, now: time.Now, touched: map[string]time.Time{}}
	if err := db.WithContext(ctx).AutoMigrate(&Session{}, &token{}); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes its database.
func (s *Store) Close() error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
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
	if err := s.db.WithContext(ctx).Create(&session).Error; err != nil {
		return Session{}, err
	}
	return session, nil
}

// IssueToken returns a new bridge token for the session id, lasting ttl, or
// until its session ends if ttl is 0. Like Jellyfin's own, it is 32 hex
// digits.
func (s *Store) IssueToken(ctx context.Context, id string, ttl time.Duration) (string, error) {
	t := randomHex(16)
	row := token{Hash: hashToken(t), SessionID: id}
	if ttl > 0 {
		expires := s.now().Add(ttl).UTC()
		row.Expires = &expires
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return "", err
	}
	return t, nil
}

// errNoSession is a token that names no live session.
var errNoSession = errors.New("jellybridge: no session")

// live limits a query of sessions to those used within Idle.
func (s *Store) live(db *gorm.DB) *gorm.DB {
	return db.Where("sessions.last_used > ?", s.now().Add(-s.Idle).UTC())
}

// first returns the first session db selects, and errNoSession if there's
// none.
func first(db *gorm.DB) (Session, error) {
	var session Session
	err := db.Take(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Session{}, errNoSession
	}
	return session, err
}

// Lookup returns the session of token, if the token hasn't expired and its
// session hasn't been idle past Idle, and errNoSession otherwise.
func (s *Store) Lookup(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, errNoSession
	}
	return first(s.db.WithContext(ctx).Scopes(s.live).
		Joins("JOIN tokens ON tokens.session_id = sessions.id").
		Where("tokens.hash = ?", hashToken(token)).
		Where("tokens.expires IS NULL OR tokens.expires > ?", s.now().UTC()))
}

// FindSession returns the live session of kind for the Jellyfin user
// userID on the device deviceID, and errNoSession if there's none.
func (s *Store) FindSession(ctx context.Context, kind Kind, userID, deviceID string) (Session, error) {
	return first(s.db.WithContext(ctx).Scopes(s.live).
		Where(&Session{Kind: kind, UserID: userID, Device: jellyfin.Device{ID: deviceID}}).
		Order("last_used DESC"))
}

// UserSessions returns the Jellyfin user userID's live sessions, the most
// recently used first.
func (s *Store) UserSessions(ctx context.Context, userID string) ([]Session, error) {
	var sessions []Session
	err := s.db.WithContext(ctx).Scopes(s.live).
		Where(&Session{UserID: userID}).
		Order("last_used DESC").
		Find(&sessions).Error
	return sessions, err
}

// DeleteDeviceSessions deletes the user userID's sessions of kind on the
// device deviceID, returning them: Jellyfin has ended their tokens when it
// logged the device in again.
func (s *Store) DeleteDeviceSessions(ctx context.Context, kind Kind, userID, deviceID string) ([]Session, error) {
	var sessions []Session
	err := s.db.WithContext(ctx).
		Where(&Session{Kind: kind, UserID: userID, Device: jellyfin.Device{ID: deviceID}}).
		Find(&sessions).Error
	if err != nil {
		return nil, err
	}
	for _, session := range sessions {
		if err := s.DeleteSession(ctx, session.ID); err != nil {
			return nil, err
		}
	}
	return sessions, nil
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
	return s.db.WithContext(ctx).Model(&Session{ID: id}).Update("last_used", now.UTC()).Error
}

// IdleSessions returns the sessions idle past Idle, which the caller logs
// out of Jellyfin and deletes.
func (s *Store) IdleSessions(ctx context.Context) ([]Session, error) {
	var sessions []Session
	err := s.db.WithContext(ctx).
		Where("last_used <= ?", s.now().Add(-s.Idle).UTC()).
		Find(&sessions).Error
	return sessions, err
}

// DeleteSession deletes the session id and its tokens.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if err := s.db.WithContext(ctx).Delete(&Session{ID: id}).Error; err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.touched, id)
	s.mu.Unlock()
	return nil
}

// DeleteExpiredTokens deletes the tokens past their expiry.
func (s *Store) DeleteExpiredTokens(ctx context.Context) error {
	return s.db.WithContext(ctx).
		Where("expires IS NOT NULL AND expires <= ?", s.now().UTC()).
		Delete(&token{}).Error
}
