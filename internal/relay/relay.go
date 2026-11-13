// Package relay keeps a Matrix session with end-to-end encryption for an app
// that can't: it sends the app's messages to rooms, and forwards the app the
// messages it receives.
package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/sqlstatestore"
	_ "modernc.org/sqlite"

	"github.com/pdeslaur/matrix-relay/internal/sts"
)

// Config configures a Relay.
type Config struct {
	// HomeserverURL is the homeserver's client API.
	HomeserverURL string
	// StoreDir holds the crypto store, crypto.db, and the key encrypting
	// its secrets, pickle.key, generated the first time.
	StoreDir string
	// Login logs in on a device, or on one the homeserver creates when it's
	// empty.
	Login func(ctx context.Context, device id.DeviceID) (sts.Session, error)
	// Allowed are the users whose invites it accepts, declining others',
	// and whose messages it forwards.
	Allowed []id.UserID
	// InboundURL receives the messages it forwards, as JSON Inbound; empty
	// forwards none.
	InboundURL string
	// HTTP posts to InboundURL.
	HTTP *http.Client
	// Log is mautrix's.
	Log zerolog.Logger
}

// Inbound is a message the relay forwards.
type Inbound struct {
	RoomID    id.RoomID         `json:"room_id"`
	EventID   id.EventID        `json:"event_id"`
	Sender    id.UserID         `json:"sender"`
	MsgType   event.MessageType `json:"msgtype"`
	Body      string            `json:"body"`
	Encrypted bool              `json:"encrypted"`
}

// Relay is a Matrix session with end-to-end encryption.
type Relay struct {
	cfg     Config
	allowed map[id.UserID]bool
	db      *dbutil.Database
	cli     *mautrix.Client
	crypto  *cryptohelper.CryptoHelper
	// token is the access token requests carry, replaced as it's renewed.
	token atomic.Pointer[string]
	// expiresIn is how long the first access token lasts.
	expiresIn time.Duration
	// started is when it started: it forwards no message older.
	started time.Time
	ready   chan struct{}
}

// secretSeeds names the crypto store's secret holding the cross-signing
// keys' seeds.
const secretSeeds id.Secret = "matrix-relay.cross_signing_seeds"

// Open logs in on the device the store holds, or on a device the homeserver
// creates for an empty store, and sets up its end-to-end encryption: its
// device keys, and cross-signing keys signing its device, so that clients
// trust it as its user's.
func Open(ctx context.Context, cfg Config) (*Relay, error) {
	r := &Relay{
		cfg:     cfg,
		allowed: make(map[id.UserID]bool, len(cfg.Allowed)),
		started: time.Now(),
		ready:   make(chan struct{}),
	}
	for _, u := range cfg.Allowed {
		r.allowed[u] = true
	}
	if err := os.MkdirAll(cfg.StoreDir, 0o700); err != nil {
		return nil, err
	}
	pickleKey, err := pickleKey(filepath.Join(cfg.StoreDir, "pickle.key"))
	if err != nil {
		return nil, err
	}
	r.db, err = dbutil.NewWithDialect("file:"+filepath.Join(cfg.StoreDir, "crypto.db")+
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate", "sqlite")
	if err != nil {
		return nil, err
	}
	if err := r.open(ctx, pickleKey); err != nil {
		r.db.Close()
		return nil, err
	}
	return r, nil
}

func (r *Relay) open(ctx context.Context, pickleKey []byte) error {
	dbLog := dbutil.ZeroLogger(r.cfg.Log)
	store := crypto.NewSQLCryptoStore(r.db, dbLog, "", "", pickleKey)
	if err := store.DB.Upgrade(ctx); err != nil {
		return fmt.Errorf("upgrading the crypto store: %w", err)
	}
	state := sqlstatestore.NewSQLStateStore(r.db, dbLog, false)
	if err := state.Upgrade(ctx); err != nil {
		return fmt.Errorf("upgrading the state store: %w", err)
	}
	device, err := store.FindDeviceID(ctx)
	if err != nil {
		return fmt.Errorf("finding the store's device: %w", err)
	}
	session, err := r.cfg.Login(ctx, device)
	if err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	clog.InfoContext(ctx, "logged in", "user_id", session.UserID, "device_id", session.DeviceID, "expires_in", session.ExpiresIn())
	store.DeviceID = session.DeviceID
	r.token.Store(&session.AccessToken)
	r.expiresIn = session.ExpiresIn()

	r.cli, err = mautrix.NewClient(r.cfg.HomeserverURL, session.UserID, "")
	if err != nil {
		return err
	}
	r.cli.DeviceID = session.DeviceID
	r.cli.Log = r.cfg.Log
	// Requests carry the current access token, which the client doesn't
	// hold, so that renewing it doesn't race them.
	r.cli.Client = &http.Client{Transport: &bearer{token: &r.token, next: http.DefaultTransport}}
	r.cli.Store = store
	r.cli.StateStore = state
	syncer := &syncer{DefaultSyncer: mautrix.NewDefaultSyncer(), synced: sync.OnceFunc(func() { close(r.ready) })}
	r.cli.Syncer = syncer

	r.crypto, err = cryptohelper.NewCryptoHelper(r.cli, pickleKey, store)
	if err != nil {
		return err
	}
	// Init shares the device's keys the first time, and refuses a device
	// whose keys the homeserver lost, like after logging it out.
	if err := r.crypto.Init(ctx); err != nil {
		return fmt.Errorf("setting up encryption: %w", err)
	}
	r.cli.Crypto = r.crypto
	if err := r.crossSign(ctx, store); err != nil {
		return fmt.Errorf("cross-signing: %w", err)
	}

	syncer.OnEventType(event.StateMember, r.onMember)
	syncer.OnEventType(event.EventMessage, r.onMessage)
	return nil
}

// pickleKey reads the key at path, generating it the first time.
func pickleKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("%s holds %d bytes, want 32", path, len(key))
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	return key, f.Close()
}

// crossSign publishes cross-signing keys the first time, keeping their
// seeds in the store, and signs the device with them. A user whose keys
// the store doesn't hold keeps them, its device unsigned.
func (r *Relay) crossSign(ctx context.Context, store *crypto.SQLCryptoStore) error {
	mach := r.crypto.Machine()
	resp, err := r.cli.QueryKeys(ctx, &mautrix.ReqQueryKeys{
		DeviceKeys: map[id.UserID]mautrix.DeviceIDList{r.cli.UserID: {}},
	})
	if err != nil {
		return fmt.Errorf("querying the user's keys: %w", err)
	}
	var published id.Ed25519
	if master, ok := resp.MasterKeys[r.cli.UserID]; ok {
		published = master.FirstKey()
	}

	stored, err := store.GetSecret(ctx, secretSeeds)
	if err != nil {
		return err
	}
	if stored == "" {
		if published != "" {
			clog.WarnContext(ctx, "the user has cross-signing keys the store doesn't hold: its device stays unsigned", "master_key", published)
			return nil
		}
		keys, err := mach.GenerateCrossSigningKeys()
		if err != nil {
			return err
		}
		seeds, err := json.Marshal(keys.Seeds())
		if err != nil {
			return err
		}
		// Kept before publishing them, so that a failure to publish them
		// publishes them next time instead of leaving keys it lost.
		if err := store.PutSecret(ctx, secretSeeds, string(seeds)); err != nil {
			return err
		}
		stored = string(seeds)
	}
	var seeds crypto.CrossSigningSeeds
	if err := json.Unmarshal([]byte(stored), &seeds); err != nil {
		return fmt.Errorf("decoding the cross-signing seeds: %w", err)
	}
	if err := mach.ImportCrossSigningKeys(seeds); err != nil {
		return err
	}
	ours := mach.CrossSigningKeys.MasterKey.PublicKey()
	switch published {
	case "":
		// The homeserver takes a user's first cross-signing keys without
		// user-interactive auth, which a user without a password couldn't
		// complete.
		if err := mach.PublishCrossSigningKeys(ctx, mach.CrossSigningKeys, func(*mautrix.RespUserInteractive) any { return nil }); err != nil {
			return fmt.Errorf("publishing cross-signing keys: %w", err)
		}
		clog.InfoContext(ctx, "published cross-signing keys", "master_key", ours)
	case ours:
	default:
		clog.WarnContext(ctx, "the user has other cross-signing keys than the store's: its device stays unsigned", "master_key", published)
		return nil
	}
	return mach.SignOwnDevice(ctx, mach.OwnIdentity())
}

// Run syncs, and renews the access token before it expires, until ctx is
// done.
func (r *Relay) Run(ctx context.Context) error {
	go r.renew(ctx)
	err := r.cli.SyncWithContext(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// renew logs in again on the device once four fifths of the access token's
// lifetime has passed, retrying every minute while it fails.
func (r *Relay) renew(ctx context.Context) {
	expiresIn := r.expiresIn
	for expiresIn > 0 {
		wait := expiresIn * 4 / 5
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			session, err := r.cfg.Login(ctx, r.cli.DeviceID)
			if err != nil {
				clog.ErrorContext(ctx, "renewing the access token", "error", err)
				wait = time.Minute
				continue
			}
			r.token.Store(&session.AccessToken)
			expiresIn = session.ExpiresIn()
			clog.InfoContext(ctx, "renewed the access token", "expires_in", expiresIn)
			break
		}
	}
}

// Ready is closed once the first sync is processed: it knows the rooms it's
// in, and which are encrypted.
func (r *Relay) Ready() <-chan struct{} { return r.ready }

// UserID is the relay's user.
func (r *Relay) UserID() id.UserID { return r.cli.UserID }

// DeviceID is the relay's device.
func (r *Relay) DeviceID() id.DeviceID { return r.cli.DeviceID }

// Close closes the store.
func (r *Relay) Close() error { return r.db.Close() }

// onMember joins rooms an allowed user invites it to, and declines others.
func (r *Relay) onMember(ctx context.Context, evt *event.Event) {
	if evt.GetStateKey() != r.cli.UserID.String() || evt.Content.AsMember().Membership != event.MembershipInvite {
		return
	}
	if !r.allowed[evt.Sender] {
		clog.WarnContext(ctx, "declining an invite", "room_id", evt.RoomID, "sender", evt.Sender)
		if _, err := r.cli.LeaveRoom(ctx, evt.RoomID); err != nil {
			clog.ErrorContext(ctx, "declining an invite", "room_id", evt.RoomID, "error", err)
		}
		return
	}
	// A sync may carry an invite more than once.
	if r.cli.StateStore.IsMembership(ctx, evt.RoomID, r.cli.UserID, event.MembershipJoin) {
		return
	}
	clog.InfoContext(ctx, "joining", "room_id", evt.RoomID, "sender", evt.Sender)
	if _, err := r.cli.JoinRoomByID(ctx, evt.RoomID); err != nil {
		clog.ErrorContext(ctx, "joining", "room_id", evt.RoomID, "error", err)
	}
}

// onMessage forwards messages allowed users send after it started.
func (r *Relay) onMessage(ctx context.Context, evt *event.Event) {
	if r.cfg.InboundURL == "" || !r.allowed[evt.Sender] || evt.Sender == r.cli.UserID ||
		time.UnixMilli(evt.Timestamp).Before(r.started) {
		return
	}
	msg := evt.Content.AsMessage()
	if err := r.forward(ctx, Inbound{
		RoomID:    evt.RoomID,
		EventID:   evt.ID,
		Sender:    evt.Sender,
		MsgType:   msg.MsgType,
		Body:      msg.Body,
		Encrypted: evt.Mautrix.WasEncrypted,
	}); err != nil {
		clog.ErrorContext(ctx, "forwarding a message", "room_id", evt.RoomID, "event_id", evt.ID, "error", err)
	}
}

func (r *Relay) forward(ctx context.Context, in Inbound) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.InboundURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s answered %s", r.cfg.InboundURL, resp.Status)
	}
	return nil
}

// syncer marks the relay ready once it has processed a sync.
type syncer struct {
	*mautrix.DefaultSyncer
	synced func()
}

func (s *syncer) ProcessResponse(ctx context.Context, res *mautrix.RespSync, since string) error {
	if err := s.DefaultSyncer.ProcessResponse(ctx, res, since); err != nil {
		return err
	}
	s.synced()
	return nil
}

// bearer authorizes requests with the current access token.
type bearer struct {
	token *atomic.Pointer[string]
	next  http.RoundTripper
}

func (b *bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+*b.token.Load())
	return b.next.RoundTrip(req)
}
