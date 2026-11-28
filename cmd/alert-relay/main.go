// Command alert-relay takes a rule engine's alerts, as Alertmanager's API
// receives them, and sends each one to a Matrix room through matrix-relay,
// once when it fires and once when it resolves.
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/sethvargo/go-envconfig/v2"

	"github.com/pdeslaur/hangar/internal/alertrelay"
	"github.com/pdeslaur/hangar/internal/systemd"
)

type config struct {
	// Addr is where it listens for alerts: Alertmanager's port, on
	// loopback, so that only the rule engine in its pod reaches it.
	Addr string `env:"ADDR,default=127.0.0.1:9093"`
	// SendURL is matrix-relay's /send, like http://127.0.0.1:8090/send.
	SendURL string `env:"SEND_URL,required"`
	// Room is the room ID or alias the alerts go to.
	Room string `env:"ROOM,required"`
	// StoreDir holds the database of the alerts firing.
	StoreDir string `env:"STORE_DIR,default=/var/lib/alert-relay"`
	// SweepInterval is how often it looks for alerts whose end passed
	// without an update.
	SweepInterval time.Duration `env:"SWEEP_INTERVAL,default=1m"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := envconfig.MustProcess(ctx, &config{})
	if err := run(ctx, cfg); err != nil {
		clog.ErrorContext(ctx, "relaying alerts", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config) error {
	if err := os.MkdirAll(cfg.StoreDir, 0o700); err != nil {
		return err
	}
	store, err := alertrelay.OpenStore(ctx, filepath.Join(cfg.StoreDir, "alerts.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	sender := &alertrelay.Sender{
		URL:  cfg.SendURL,
		Room: cfg.Room,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
	r := &alertrelay.Relay{Store: store, Send: sender.Send}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v2/alerts", r)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	go sweep(ctx, r, cfg.SweepInterval)
	if err := systemd.Ready(); err != nil {
		return err
	}
	clog.InfoContext(ctx, "listening", "addr", cfg.Addr, "room", cfg.Room)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// sweep resolves expired alerts every interval until ctx is done. A failed
// sweep, matrix-relay being down, is tried again at the next.
func sweep(ctx context.Context, r *alertrelay.Relay, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Sweep(ctx); err != nil {
				clog.WarnContext(ctx, "sweeping expired alerts", "error", err)
			}
		}
	}
}
