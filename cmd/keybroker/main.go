// Command keybroker derives each workload's secrets from one master secret
// and the workload's SPIFFE ID. "keybroker serve" serves them over mTLS;
// "keybroker fetch" fetches a workload's secrets into env files.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/keybroker"
	"github.com/pdeslaur/hangar/internal/systemd"
	"github.com/sethvargo/go-envconfig"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

type serveConfig struct {
	// Addr is where it listens.
	Addr string `env:"ADDR,default=:8443"`
	// MasterFile holds the master secret.
	MasterFile string `env:"MASTER_FILE,required"`
	// MasterCreate creates a random master when MasterFile is missing;
	// otherwise a missing file stops it.
	MasterCreate bool `env:"MASTER_CREATE,default=true"`
}

type fetchConfig struct {
	// BrokerURL is the broker's base URL, like https://10.0.0.1:8443.
	BrokerURL string `env:"BROKER_URL,required"`
	// BrokerID is the SPIFFE ID the broker must present.
	BrokerID string `env:"BROKER_ID,required"`
	// Dir is where it writes the env files.
	Dir string `env:"DIR,required"`
	// Files is the JSON list of keybroker.File to write.
	Files keybroker.Files `env:"FILES,required"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: keybroker serve|fetch")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, envconfig.MustProcess(ctx, &serveConfig{}))
	case "fetch":
		err = fetch(ctx, envconfig.MustProcess(ctx, &fetchConfig{}))
	default:
		fmt.Fprintln(os.Stderr, "usage: keybroker serve|fetch")
		os.Exit(2)
	}
	if err != nil {
		clog.ErrorContext(ctx, os.Args[1], "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, cfg *serveConfig) error {
	master, err := keybroker.LoadMaster(cfg.MasterFile, cfg.MasterCreate)
	if err != nil {
		return err
	}

	// The Workload API's address comes from SPIFFE_ENDPOINT_SOCKET.
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		return err
	}
	defer source.Close()
	svid, err := source.GetX509SVID()
	if err != nil {
		return err
	}
	td := svid.ID.TrustDomain()

	srv := keybroker.NewServer(cfg.Addr, master, source, source, td)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return err
	}
	if err := systemd.Ready(); err != nil {
		return err
	}
	clog.InfoContext(ctx, "listening", "addr", cfg.Addr, "id", svid.ID.String())
	if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func fetch(ctx context.Context, cfg *fetchConfig) error {
	if err := cfg.Files.Validate(); err != nil {
		return err
	}
	brokerID, err := spiffeid.FromString(cfg.BrokerID)
	if err != nil {
		return err
	}

	// Waits for the agent, retrying, until the workload has an SVID.
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		return err
	}
	defer source.Close()
	client := &keybroker.Client{
		URL: cfg.BrokerURL,
		HTTP: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeID(brokerID)),
			},
		},
	}

	// Retries while the broker is unreachable, until systemd's start
	// timeout stops it.
	backoff := time.Second
	for {
		contents, err := client.Render(ctx, cfg.Files)
		if err == nil {
			if err := keybroker.WriteFiles(cfg.Dir, contents); err != nil {
				return err
			}
			clog.InfoContext(ctx, "wrote secrets", "dir", cfg.Dir, "files", len(contents))
			return nil
		}
		var se *keybroker.StatusError
		if errors.As(err, &se) && se.Permanent() {
			return err
		}
		clog.WarnContext(ctx, "fetching secrets", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}
