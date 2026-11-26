// Command jellyfin-bridge puts Jellyfin behind single sign-on. It runs in
// Jellyfin's pod, between the reverse proxy doing the sign-on and Jellyfin:
// it signs browsers in as the user the sign-on names, hands clients tokens
// of its own, and forwards their requests to Jellyfin with the Jellyfin
// token of each one's session, which never leaves it.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/jellybridge"
	"github.com/pdeslaur/hangar/internal/jellyfin"
	"github.com/pdeslaur/hangar/internal/systemd"
	"github.com/sethvargo/go-envconfig/v2"
)

type config struct {
	// Addr is where it listens: the reverse proxy's upstream.
	Addr string `env:"ADDR,default=127.0.0.1:8097"`
	// JellyfinURL is Jellyfin's, which nothing else should reach.
	JellyfinURL string `env:"JELLYFIN_URL,default=http://127.0.0.1:8096"`
	// JellyfinDB is Jellyfin's database, where it writes its API key, a
	// random one each time it starts.
	JellyfinDB string `env:"JELLYFIN_DB,required"`
	// APIKeyName is its API key's name in Jellyfin.
	APIKeyName string `env:"API_KEY_NAME,default=jellyfin-bridge"`
	// State is its own database, of sessions.
	State string `env:"STATE,required"`
	// UserHeader is the header the reverse proxy names the signed-in user
	// in, on requests to /sso/.
	UserHeader string `env:"USER_HEADER,default=X-Bridge-User"`
	// Users maps a signed-in user to their Jellyfin username, like
	// "phil:Phil,anna:Anna", and only they sign in. Without it, a signed-in
	// user's name is their Jellyfin username.
	Users map[string]string `env:"USERS"`
	// BrowserTTL is how long a browser's token lasts.
	BrowserTTL time.Duration `env:"BROWSER_TTL,default=8h"`
	// RefreshBefore is how long before its token expires a browser
	// refreshes it, at most half of BrowserTTL.
	RefreshBefore time.Duration `env:"REFRESH_BEFORE,default=1h"`
	// Idle is how long a session lasts without use.
	Idle time.Duration `env:"IDLE,default=1080h"`
	// SweepEvery is how often it ends idle sessions.
	SweepEvery time.Duration `env:"SWEEP_EVERY,default=1h"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := envconfig.MustProcess(ctx, &config{})
	if err := run(ctx, cfg); err != nil {
		clog.ErrorContext(ctx, "serving", "error", err)
		os.Exit(1)
	}
}

// waitForJellyfin returns once Jellyfin has started, and with it its
// database.
func waitForJellyfin(ctx context.Context, client *jellyfin.Client) error {
	for {
		err := client.Ready(ctx)
		if err == nil {
			return nil
		}
		clog.InfoContext(ctx, "waiting for jellyfin", "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func run(ctx context.Context, cfg *config) error {
	target, err := url.Parse(cfg.JellyfinURL)
	if err != nil {
		return err
	}
	// A key of its own, replacing the one it wrote before: it never leaves
	// the bridge, and sessions don't depend on it.
	key := make([]byte, 16)
	rand.Read(key)
	client := &jellyfin.Client{
		URL:  cfg.JellyfinURL,
		Key:  hex.EncodeToString(key),
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
	if err := waitForJellyfin(ctx, client); err != nil {
		return err
	}
	if err := jellyfin.EnsureAPIKey(ctx, cfg.JellyfinDB, cfg.APIKeyName, client.Key); err != nil {
		return err
	}
	serverID, err := client.ServerID(ctx)
	if err != nil {
		return err
	}
	// Check the key, and that each user mapped exists.
	for user, username := range cfg.Users {
		if _, err := client.UserID(ctx, username); err != nil {
			clog.WarnContext(ctx, "a mapped user isn't in Jellyfin", "user", user, "username", username, "error", err)
		}
	}

	sessions, err := jellybridge.Open(ctx, cfg.State, cfg.Idle)
	if err != nil {
		return err
	}
	defer sessions.Close()
	go func() {
		for {
			if err := jellybridge.Sweep(ctx, sessions, client); err != nil {
				clog.WarnContext(ctx, "ending idle sessions", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(cfg.SweepEvery):
			}
		}
	}()

	mux := http.NewServeMux()
	apps := jellybridge.NewApps(sessions, client)
	mux.Handle("/sso/", &jellybridge.SSO{
		Apps:          apps,
		ServerID:      serverID,
		UserHeader:    cfg.UserHeader,
		Users:         cfg.Users,
		BrowserTTL:    cfg.BrowserTTL,
		RefreshBefore: cfg.RefreshBefore,
	})
	mux.Handle("/", jellybridge.NewProxy(target, apps))

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := systemd.Ready(); err != nil {
		return err
	}
	clog.InfoContext(ctx, "listening", "addr", cfg.Addr, "jellyfin", cfg.JellyfinURL, "server", serverID)
	if err := srv.Serve(lis); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
