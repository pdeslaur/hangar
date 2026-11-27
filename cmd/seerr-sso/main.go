// Command seerr-sso signs Seerr's users in through single sign-on. It runs
// in Seerr's pod, between the reverse proxy doing the sign-on and Seerr,
// and signs users in with Seerr's own Jellyfin Quick Connect sign-in, its
// code approved by jellyfin-bridge for the user the sign-on names.
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/seerrsso"
	"github.com/pdeslaur/hangar/internal/systemd"
	"github.com/sethvargo/go-envconfig/v2"
)

type config struct {
	// Addr is where it listens: the reverse proxy's upstream.
	Addr string `env:"ADDR,default=127.0.0.1:5056"`
	// SeerrURL is Seerr's.
	SeerrURL string `env:"SEERR_URL,default=http://127.0.0.1:5055"`
	// BridgeURL is jellyfin-bridge's internal address, its INTERNAL_ADDR,
	// like through an mTLS tunnel.
	BridgeURL string `env:"BRIDGE_URL,required"`
	// UserHeader is the header the reverse proxy names the signed-in user
	// in, on requests to /sso/.
	UserHeader string `env:"USER_HEADER,default=X-Bridge-User"`
	// SessionDuration is how long a Seerr session lasts from its sign-in,
	// rather than Seerr's 30 days; 0 leaves Seerr's.
	SessionDuration time.Duration `env:"SESSION_DURATION,default=8h"`
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

func run(ctx context.Context, cfg *config) error {
	seerr, err := url.Parse(cfg.SeerrURL)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: &seerrsso.Handler{
			Seerr:           seerr,
			Bridge:          cfg.BridgeURL,
			UserHeader:      cfg.UserHeader,
			SessionDuration: cfg.SessionDuration,
			Client: &http.Client{
				Timeout: 30 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
		},
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
	clog.InfoContext(ctx, "listening", "addr", cfg.Addr, "seerr", cfg.SeerrURL, "bridge", cfg.BridgeURL, "session", cfg.SessionDuration)
	if err := srv.Serve(lis); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
