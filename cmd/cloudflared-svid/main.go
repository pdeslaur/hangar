// Command cloudflared-svid runs a Cloudflare tunnel with credentials it gets
// from cloudflare-sts for the workload's JWT-SVID, then execs cloudflared.
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/cloudflarests"
	"github.com/sethvargo/go-envconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

type config struct {
	// BrokerURL is cloudflare-sts's, and the audience of the JWT-SVID it
	// exchanges there.
	BrokerURL string `env:"BROKER_URL,required"`
	// Profile is the broker's profile for the tunnel.
	Profile string `env:"PROFILE,required"`
	// CredentialsFile is where it writes the tunnel's credentials: on a
	// tmpfs, only the container's.
	CredentialsFile string `env:"CREDENTIALS_FILE,required"`
	// CloudflaredConfig is cloudflared's config file, with the tunnel's
	// ingress rules.
	CloudflaredConfig string `env:"CLOUDFLARED_CONFIG,required"`
	// Cloudflared is the cloudflared binary it execs.
	Cloudflared string `env:"CLOUDFLARED,default=/usr/local/bin/cloudflared"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := envconfig.MustProcess(ctx, &config{})
	// The Workload API's address comes from SPIFFE_ENDPOINT_SOCKET.
	svids, err := workloadapi.New(ctx)
	if err != nil {
		clog.FatalContext(ctx, "connecting to the Workload API", "error", err)
	}
	broker := &cloudflarests.Client{URL: cfg.BrokerURL, SVIDs: svids, HTTP: cloudflarests.NewHTTPClient()}
	argv, err := prepare(ctx, cfg, broker, time.Second)
	if err != nil {
		clog.FatalContext(ctx, "getting the tunnel's credentials", "error", err)
	}
	svids.Close()
	stop()
	err = syscall.Exec(cfg.Cloudflared, argv, os.Environ())
	clog.FatalContext(ctx, "executing cloudflared", "path", cfg.Cloudflared, "error", err)
}

// maxDelay bounds the delay between two exchanges.
const maxDelay = 30 * time.Second

// prepare exchanges a JWT-SVID for the tunnel's credentials, retrying from
// delay while the Workload API or the broker is unavailable, writes them to
// cfg.CredentialsFile and returns cloudflared's command line.
func prepare(ctx context.Context, cfg *config, broker *cloudflarests.Client, delay time.Duration) ([]string, error) {
	for {
		creds, err := broker.Tunnel(ctx, cfg.Profile)
		if err == nil {
			if err := writeCredentials(cfg.CredentialsFile, creds); err != nil {
				return nil, err
			}
			clog.InfoContext(ctx, "wrote the tunnel's credentials", "tunnel", creds.TunnelID, "path", cfg.CredentialsFile)
			return []string{
				cfg.Cloudflared, "tunnel", "--no-autoupdate", "--config", cfg.CloudflaredConfig,
				"run", "--credentials-file", cfg.CredentialsFile, creds.TunnelID,
			}, nil
		}
		if !cloudflarests.Temporary(err) {
			return nil, err
		}
		clog.WarnContext(ctx, "getting the tunnel's credentials, retrying", "error", err, "delay", delay)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, maxDelay)
	}
}

// writeCredentials writes creds to path, readable only by its owner,
// replacing what was there at once.
func writeCredentials(path string, creds cloudflarests.TunnelCredentials) error {
	b, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	// CreateTemp makes the file 0600.
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
