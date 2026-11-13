package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/pdeslaur/matrix-relay/internal/sts"
)

// jwtKey is the HMAC key the test's tuwunel trusts for JWT logins, like
// tuwunel-svid-sts's key.
const jwtKey = "matrix-relay-test-key-of-32-bytes"

// password is the password of the users other than the relay's, which
// log in without JWTs, so that their access tokens don't expire.
const password = "correct-horse-battery-staple"

// startTuwunel runs the tuwunel image named by TUWUNEL_IMAGE with docker,
// with the users "phil", its admin, "bot", the relay's, and "mallory", and
// returns its client API URL. JWT logins' access tokens last ttl. Without
// TUWUNEL_IMAGE, the test is skipped.
func startTuwunel(t *testing.T, ttl time.Duration) string {
	t.Helper()
	image := os.Getenv("TUWUNEL_IMAGE")
	if image == "" {
		t.Skip("TUWUNEL_IMAGE unset, like ghcr.io/matrix-construct/tuwunel:latest")
	}
	dir := t.TempDir()
	config := "[global.jwt]\nenable = true\nformat = \"HMAC\"\nalgorithm = \"HS256\"\nkey = \"" + jwtKey + "\"\nregister_user = false\nexpire_access_token = true\n"
	if err := os.WriteFile(filepath.Join(dir, "jwt.toml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	users := []string{"phil", "bot", "mallory"}
	args := []string{"run", "--detach", "--rm",
		"--publish", "127.0.0.1::6167",
		"--volume", dir + ":/etc/relay:ro",
		"--env", "TUWUNEL_CONFIG=/etc/relay/jwt.toml",
		"--env", "TUWUNEL_SERVER_NAME=example.org",
		"--env", "TUWUNEL_DATABASE_PATH=/tmp/db",
		"--env", "TUWUNEL_ADDRESS=0.0.0.0",
		"--env", "TUWUNEL_PORT=6167",
		"--env", "TUWUNEL_ACCESS_TOKEN_TTL=" + strconv.Itoa(int(ttl.Seconds())),
		image}
	for _, u := range users {
		args = append(args, "--execute", "users create-user "+u+" "+password)
	}
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("starting tuwunel: %v", err)
	}
	cid := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", cid).CombinedOutput()
			t.Logf("tuwunel logs:\n%s", logs)
		}
		exec.Command("docker", "rm", "--force", cid).Run()
	})
	out, err = exec.Command("docker", "port", cid, "6167/tcp").Output()
	if err != nil {
		t.Fatalf("finding tuwunel's port: %v", err)
	}
	url := "http://" + strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])

	// tuwunel listens, then runs --execute: wait for the users to exist.
	waitFor(t, "tuwunel's users", func() bool {
		for _, u := range users {
			resp, err := http.Get(url + "/_matrix/client/v3/profile/@" + u + ":example.org")
			if err != nil {
				return false
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return false
			}
		}
		return true
	})
	return url
}

// botJWT signs a JWT logging bot in, like tuwunel-svid-sts's.
func botJWT() (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(jwtKey)}, nil)
	if err != nil {
		return "", err
	}
	return jwt.Signed(signer).Claims(jwt.Claims{
		Subject: "bot",
		Expiry:  jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}).Serialize()
}

// stsReplaceCrossSigningKeys replaces bot's cross-signing keys like
// tuwunel-svid-sts does, with a JWT as auth.
func stsReplaceCrossSigningKeys(url string) func(context.Context, sts.CrossSigningKeys) error {
	return func(ctx context.Context, keys sts.CrossSigningKeys) error {
		token, err := botJWT()
		if err != nil {
			return err
		}
		body, err := json.Marshal(map[string]any{
			"master_key":       keys.MasterKey,
			"self_signing_key": keys.SelfSigningKey,
			"user_signing_key": keys.UserSigningKey,
			"auth":             map[string]string{"type": "org.matrix.login.jwt", "token": token},
		})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/_matrix/client/v3/keys/device_signing/upload", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+keys.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("uploading cross-signing keys: %s: %s", resp.Status, b)
		}
		return nil
	}
}

// stsLogin logs bot in like tuwunel-svid-sts does, with a JWT, counting its
// logins.
func stsLogin(t *testing.T, url string, logins *atomic.Int32) func(context.Context, id.DeviceID) (sts.Session, error) {
	return func(ctx context.Context, device id.DeviceID) (sts.Session, error) {
		logins.Add(1)
		token, err := botJWT()
		if err != nil {
			return sts.Session{}, err
		}
		body, err := json.Marshal(map[string]string{"type": "org.matrix.login.jwt", "token": token, "device_id": string(device)})
		if err != nil {
			return sts.Session{}, err
		}
		resp, err := http.Post(url+"/_matrix/client/v3/login", "application/json", bytes.NewReader(body))
		if err != nil {
			return sts.Session{}, err
		}
		defer resp.Body.Close()
		var s sts.Session
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			return sts.Session{}, err
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("logging in bot: %s", resp.Status)
		}
		return s, nil
	}
}

// inbound is an app receiving the messages a relay forwards.
type inbound struct {
	*httptest.Server
	messages chan Inbound
}

func newInbound(t *testing.T) *inbound {
	in := &inbound{messages: make(chan Inbound, 16)}
	in.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg Inbound
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Errorf("decoding a forwarded message: %v", err)
		}
		in.messages <- msg
	}))
	t.Cleanup(in.Close)
	return in
}

// startRelay opens a relay for bot with its store in dir, runs it until the
// test ends, and waits for it to be ready.
func startRelay(t *testing.T, url, dir string, logins *atomic.Int32, in *inbound) *Relay {
	t.Helper()
	ctx := t.Context()
	cfg := Config{
		HomeserverURL:           url,
		StoreDir:                dir,
		Login:                   stsLogin(t, url, logins),
		ReplaceCrossSigningKeys: stsReplaceCrossSigningKeys(url),
		Allowed:                 []id.UserID{"@phil:example.org"},
		HTTP:                    http.DefaultClient,
		Log:                     zerolog.Nop(),
	}
	if in != nil {
		cfg.InboundURL = in.URL
	}
	r, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("opening the relay: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := r.Run(ctx); err != nil {
			t.Errorf("running the relay: %v", err)
		}
	})
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		r.Close()
	})
	select {
	case <-r.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("the relay never synced")
	}
	return r
}

// user is a user's client with end-to-end encryption, like Element.
type user struct {
	*mautrix.Client
	messages chan *event.Event
}

// login logs localpart in with its password, with its crypto store in dir,
// and syncs until the test ends.
func login(t *testing.T, url, localpart, dir string) *user {
	t.Helper()
	cli, err := mautrix.NewClient(url, "", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := dbutil.NewWithDialect("file:"+filepath.Join(dir, localpart+".db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate", "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := cryptohelper.NewCryptoHelper(cli, []byte("pickle"), db)
	if err != nil {
		t.Fatal(err)
	}
	helper.LoginAs = &mautrix.ReqLogin{
		Type:       mautrix.AuthTypePassword,
		Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: localpart},
		Password:   password,
	}
	if err := helper.Init(t.Context()); err != nil {
		t.Fatalf("logging in %s: %v", localpart, err)
	}
	cli.Crypto = helper
	u := &user{Client: cli, messages: make(chan *event.Event, 16)}
	cli.Syncer.(*mautrix.DefaultSyncer).OnEventType(event.EventMessage, func(_ context.Context, evt *event.Event) {
		u.messages <- evt
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { cli.SyncWithContext(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		helper.Close()
	})
	return u
}

// createRoom creates an encrypted room inviting invitee.
func (u *user) createRoom(t *testing.T, invitee id.UserID) id.RoomID {
	t.Helper()
	resp, err := u.CreateRoom(t.Context(), &mautrix.ReqCreateRoom{
		Preset: "private_chat",
		Invite: []id.UserID{invitee},
		InitialState: []*event.Event{{
			Type:    event.StateEncryption,
			Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.RoomID
}

// membership is who's membership in room, as u's sync shows it.
func (u *user) membership(t *testing.T, room id.RoomID, who id.UserID) event.Membership {
	m, err := u.StateStore.GetMember(t.Context(), room, who)
	if err != nil {
		t.Fatal(err)
	}
	return m.Membership
}

// receive returns the first message from sender to u, within 30 seconds.
func (u *user) receive(t *testing.T, sender id.UserID) *event.Event {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case evt := <-u.messages:
			if evt.Sender == sender {
				return evt
			}
		case <-timeout:
			t.Fatalf("no message from %s", sender)
			return nil
		}
	}
}

// send asks r to send msg, retrying until the relay is in the room, and
// returns the status of its answer.
func send(t *testing.T, r *Relay, msg Message) {
	t.Helper()
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var rec *httptest.ResponseRecorder
	waitFor(t, "the relay to send to "+msg.Room, func() bool {
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
		return rec.Code != http.StatusForbidden
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("sending %q: %d %s", msg.Body, rec.Code, rec.Body)
	}
}

// waitFor polls cond for up to 30 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestRelay checks that the relay joins the rooms of allowed users only,
// sends and forwards messages encrypted, and cross-signs its device.
func TestRelay(t *testing.T) {
	url := startTuwunel(t, time.Hour)
	dir := t.TempDir()
	var logins atomic.Int32
	in := newInbound(t)
	r := startRelay(t, url, filepath.Join(dir, "relay"), &logins, in)
	phil := login(t, url, "phil", dir)
	mallory := login(t, url, "mallory", dir)
	bot := r.UserID()

	room := phil.createRoom(t, bot)
	waitFor(t, "the relay to join phil's room", func() bool {
		return phil.membership(t, room, bot) == event.MembershipJoin
	})
	other := mallory.createRoom(t, bot)
	waitFor(t, "the relay to decline mallory's invite", func() bool {
		return mallory.membership(t, other, bot) == event.MembershipLeave
	})

	send(t, r, Message{Room: room.String(), Body: "from the relay"})
	evt := phil.receive(t, bot)
	if got := evt.Content.AsMessage().Body; got != "from the relay" || !evt.Mautrix.WasEncrypted {
		t.Errorf("phil received %q, encrypted %v", got, evt.Mautrix.WasEncrypted)
	}
	send(t, r, Message{Room: room.String(), Body: "notice", Notice: true})
	if got := phil.receive(t, bot).Content.AsMessage().MsgType; got != event.MsgNotice {
		t.Errorf("notice sent as %s", got)
	}

	if _, err := phil.SendText(t.Context(), room, "to the relay"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-in.messages:
		want := Inbound{RoomID: room, EventID: msg.EventID, Sender: phil.UserID, MsgType: event.MsgText, Body: "to the relay", Encrypted: true}
		if msg != want {
			t.Errorf("forwarded %+v, want %+v", msg, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the relay forwarded nothing")
	}

	// Phil's client finds its device cross-signed by its user, as clients
	// need to show its messages as its user's: its master key signs its
	// self-signing key, which signs its device.
	mach := phil.Crypto.(*cryptohelper.CryptoHelper).Machine()
	if _, err := mach.FetchKeys(t.Context(), []id.UserID{bot}, true); err != nil {
		t.Fatal(err)
	}
	device, err := mach.GetOrFetchDevice(t.Context(), bot, r.DeviceID())
	if err != nil {
		t.Fatal(err)
	}
	if trust, err := mach.ResolveTrustContext(t.Context(), device); err != nil || trust != id.TrustStateCrossSignedTOFU {
		t.Errorf("phil trusts device %s as %s (%v), want %s", r.DeviceID(), trust, err, id.TrustStateCrossSignedTOFU)
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("logged in %d times, want 1", n)
	}
}

// TestRelayRestart checks that a relay keeps its device and cross-signing
// keys across restarts, and sends to the rooms it's in.
func TestRelayRestart(t *testing.T) {
	url := startTuwunel(t, time.Hour)
	dir := t.TempDir()
	store := filepath.Join(dir, "relay")
	phil := login(t, url, "phil", dir)
	var logins atomic.Int32

	first := startRelay(t, url, store, &logins, nil)
	device := first.DeviceID()
	room := phil.createRoom(t, first.UserID())
	waitFor(t, "the relay to join phil's room", func() bool {
		return phil.membership(t, room, first.UserID()) == event.MembershipJoin
	})
	keys, err := phil.QueryKeys(t.Context(), &mautrix.ReqQueryKeys{DeviceKeys: map[id.UserID]mautrix.DeviceIDList{first.UserID(): {}}})
	if err != nil {
		t.Fatal(err)
	}
	masterKeys := keys.MasterKeys[first.UserID()]
	master := masterKeys.FirstKey()

	// A second relay on the same store, as after a restart; the first
	// stops when the test ends, which a restart wouldn't wait for.
	second := startRelay(t, url, store, &logins, nil)
	if second.DeviceID() != device {
		t.Errorf("restarted on %s, want %s", second.DeviceID(), device)
	}
	if got := second.crypto.Machine().CrossSigningKeys.MasterKey.PublicKey(); got != master {
		t.Errorf("restarted with master key %s, want %s", got, master)
	}
	send(t, second, Message{Room: room.String(), Body: "after restarting"})
	evt := phil.receive(t, second.UserID())
	if got := evt.Content.AsMessage().Body; got != "after restarting" || !evt.Mautrix.WasEncrypted {
		t.Errorf("phil received %q, encrypted %v", got, evt.Mautrix.WasEncrypted)
	}
}

// TestRelayReplacesKeys checks that a relay replaces cross-signing keys its
// store doesn't hold, such as a client's from before it, and signs its device
// with its own.
func TestRelayReplacesKeys(t *testing.T) {
	url := startTuwunel(t, time.Hour)
	dir := t.TempDir()
	phil := login(t, url, "phil", dir)

	// bot's keys from a client, which phil has seen.
	client := login(t, url, "bot", dir).Crypto.(*cryptohelper.CryptoHelper).Machine()
	keys, err := client.GenerateCrossSigningKeys()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PublishCrossSigningKeys(t.Context(), keys, nil); err != nil {
		t.Fatalf("publishing the client's keys: %v", err)
	}
	mach := phil.Crypto.(*cryptohelper.CryptoHelper).Machine()
	if _, err := mach.FetchKeys(t.Context(), []id.UserID{"@bot:example.org"}, true); err != nil {
		t.Fatal(err)
	}

	var logins atomic.Int32
	r := startRelay(t, url, filepath.Join(dir, "relay"), &logins, nil)
	ours := r.crypto.Machine().CrossSigningKeys.MasterKey.PublicKey()
	if ours == keys.MasterKey.PublicKey() {
		t.Fatal("the relay holds the client's keys")
	}
	resp, err := phil.QueryKeys(t.Context(), &mautrix.ReqQueryKeys{DeviceKeys: map[id.UserID]mautrix.DeviceIDList{r.UserID(): {}}})
	if err != nil {
		t.Fatal(err)
	}
	masterKeys := resp.MasterKeys[r.UserID()]
	if got := masterKeys.FirstKey(); got != ours {
		t.Errorf("published master key %s, want the relay's %s", got, ours)
	}

	// Phil's client finds its device cross-signed by its user's identity,
	// which changed since phil saw it.
	if _, err := mach.FetchKeys(t.Context(), []id.UserID{r.UserID()}, true); err != nil {
		t.Fatal(err)
	}
	device, err := mach.GetOrFetchDevice(t.Context(), r.UserID(), r.DeviceID())
	if err != nil {
		t.Fatal(err)
	}
	if trust, err := mach.ResolveTrustContext(t.Context(), device); err != nil || trust != id.TrustStateCrossSignedUntrusted {
		t.Errorf("phil trusts device %s as %s (%v), want %s", r.DeviceID(), trust, err, id.TrustStateCrossSignedUntrusted)
	}
}

// TestRelayRenewal checks that a relay logs in again on its device before
// its access token expires, and goes on sending.
func TestRelayRenewal(t *testing.T) {
	const ttl = 10 * time.Second
	url := startTuwunel(t, ttl)
	dir := t.TempDir()
	phil := login(t, url, "phil", dir)
	var logins atomic.Int32
	r := startRelay(t, url, filepath.Join(dir, "relay"), &logins, nil)
	room := phil.createRoom(t, r.UserID())
	waitFor(t, "the relay to join phil's room", func() bool {
		return phil.membership(t, room, r.UserID()) == event.MembershipJoin
	})

	// Past the first access token's expiry.
	time.Sleep(ttl + 2*time.Second)
	if n := logins.Load(); n < 2 {
		t.Fatalf("logged in %d times, want at least 2", n)
	}
	send(t, r, Message{Room: room.String(), Body: "after renewing"})
	if got := phil.receive(t, r.UserID()).Content.AsMessage().Body; got != "after renewing" {
		t.Errorf("phil received %q", got)
	}
}
