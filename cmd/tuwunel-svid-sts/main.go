// Command tuwunel-svid-sts exchanges SPIFFE JWT-SVIDs for Matrix access
// tokens, through tuwunel's JWT login, with a signing key that exists only in
// memory.
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
	"github.com/pdeslaur/tuwunel-svid-sts/internal/login"
	"github.com/sethvargo/go-envconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

type config struct {
	// Addr is where it listens, behind an mTLS sidecar.
	Addr string `env:"ADDR,default=:8080"`
	// Audience is the aud a JWT-SVID needs.
	Audience string `env:"AUDIENCE,required"`
	// Users maps SPIFFE IDs to Matrix localparts:
	// spiffe://example.org/bot=bot,spiffe://example.org/other=other.
	Users map[string]string `env:"USERS,required,separator=="`
	// MatrixURL is tuwunel's client API base URL.
	MatrixURL string `env:"MATRIX_URL,default=http://localhost:6167"`
	// TuwunelConfigFile is where it writes the tuwunel config file trusting
	// its key, which tuwunel reads through TUWUNEL_CONFIG.
	TuwunelConfigFile string `env:"TUWUNEL_CONFIG_FILE,required"`
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
	signer, err := login.NewSigner()
	if err != nil {
		return err
	}
	if err := signer.WriteTuwunelConfig(cfg.TuwunelConfigFile); err != nil {
		return err
	}
	clog.InfoContext(ctx, "wrote tuwunel config", "path", cfg.TuwunelConfigFile)

	// The Workload API's address comes from SPIFFE_ENDPOINT_SOCKET.
	bundles, err := workloadapi.NewJWTSource(ctx)
	if err != nil {
		return err
	}
	defer bundles.Close()

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: &login.Handler{
			Bundles:   bundles,
			Audience:  cfg.Audience,
			Users:     cfg.Users,
			Signer:    signer,
			MatrixURL: cfg.MatrixURL,
			Client:    &http.Client{Timeout: 30 * time.Second},
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	clog.InfoContext(ctx, "listening", "addr", cfg.Addr, "users", len(cfg.Users))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
