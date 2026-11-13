// Command matrix-relay keeps a Matrix session with end-to-end encryption for
// an app in its pod that can't: the app sends messages through it, and
// receives the messages allowed users send. It logs in through
// tuwunel-svid-sts with the pod's SPIFFE identity, holding no secret.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/rs/zerolog"
	"github.com/sethvargo/go-envconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"maunium.net/go/mautrix/id"

	"github.com/pdeslaur/matrix-relay/internal/relay"
	"github.com/pdeslaur/matrix-relay/internal/sts"
	"github.com/pdeslaur/matrix-relay/internal/systemd"
)

type config struct {
	// Addr is where it listens for messages to send. It defaults to
	// loopback, so that only the app in its pod reaches it.
	Addr string `env:"ADDR,default=127.0.0.1:8090"`
	// HomeserverURL is the homeserver's client API.
	HomeserverURL string `env:"HOMESERVER_URL,required"`
	// STSURL is tuwunel-svid-sts's, like http://matrix:8080/ through the
	// pod's mTLS client.
	STSURL string `env:"STS_URL,required"`
	// STSAudience is the audience tuwunel-svid-sts requires of JWT-SVIDs.
	STSAudience string `env:"STS_AUDIENCE,required"`
	// StoreDir holds the crypto store and its key.
	StoreDir string `env:"STORE_DIR,default=/var/lib/matrix-relay"`
	// AllowedUsers are the users whose invites it accepts and whose
	// messages it forwards: @phil:example.org,@other:example.org.
	AllowedUsers []id.UserID `env:"ALLOWED_USERS,required"`
	// InboundURL receives the messages it forwards; unset forwards none.
	InboundURL string `env:"INBOUND_URL"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := envconfig.MustProcess(ctx, &config{})
	if err := run(ctx, cfg); err != nil {
		clog.ErrorContext(ctx, "relaying", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config) error {
	// The Workload API's address comes from SPIFFE_ENDPOINT_SOCKET.
	svids, err := workloadapi.NewJWTSource(ctx)
	if err != nil {
		return err
	}
	defer svids.Close()
	login := &sts.Client{
		URL:      cfg.STSURL,
		Audience: cfg.STSAudience,
		SVIDs:    svids,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}

	r, err := relay.Open(ctx, relay.Config{
		HomeserverURL:           cfg.HomeserverURL,
		StoreDir:                cfg.StoreDir,
		Login:                   login.Login,
		ReplaceCrossSigningKeys: login.ReplaceCrossSigningKeys,
		Allowed:                 cfg.AllowedUsers,
		InboundURL:              cfg.InboundURL,
		HTTP:                    &http.Client{Timeout: 10 * time.Second},
		// mautrix logs warnings and errors as JSON.
		Log: zerolog.New(os.Stderr).Level(zerolog.WarnLevel).With().Timestamp().Str("component", "mautrix").Logger(),
	})
	if err != nil {
		return err
	}
	defer r.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	synced := make(chan error, 1)
	go func() {
		synced <- r.Run(ctx)
		cancel()
	}()
	select {
	case <-r.Ready():
	case err := <-synced:
		return err
	}
	if err := systemd.Ready(); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/send", r)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	clog.InfoContext(ctx, "listening", "addr", cfg.Addr, "user_id", r.UserID(), "device_id", r.DeviceID())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-synced
}
