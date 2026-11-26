// Command oidc-relay authenticates an app's OIDC client with the workload's
// JWT-SVID, for an app that can only send a client secret: the app reads its
// discovery document, and sends its token requests, to the relay, which
// replaces the secret with a client assertion.
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/oidcrelay"
	"github.com/pdeslaur/hangar/internal/systemd"
	"github.com/sethvargo/go-envconfig/v2"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

type config struct {
	// Addr is where it listens. It defaults to loopback, so that only its
	// pod reaches it: anything that does authenticates as the client.
	Addr string `env:"ADDR,default=127.0.0.1:8095"`
	// Issuer is the OpenID provider's issuer, and the audience of the
	// JWT-SVIDs.
	Issuer string `env:"ISSUER,required"`
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

// issuerClient calls the issuer. It follows no redirect, which would send
// the client's assertions and codes elsewhere.
func issuerClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func run(ctx context.Context, cfg *config) error {
	// The Workload API's address comes from SPIFFE_ENDPOINT_SOCKET.
	svids, err := workloadapi.New(ctx)
	if err != nil {
		return err
	}
	defer svids.Close()

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: &oidcrelay.Handler{
			Issuer: strings.TrimSuffix(cfg.Issuer, "/"),
			SVIDs:  svids,
			Client: issuerClient(),
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	// Ready once it listens: an app reading the discovery document only as
	// it starts would otherwise go without its provider. The Workload API
	// isn't needed until a login.
	if err := systemd.Ready(); err != nil {
		return err
	}
	clog.InfoContext(ctx, "listening", "addr", cfg.Addr, "issuer", cfg.Issuer)
	if err := srv.Serve(lis); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
