// Command spire-entries makes a SPIRE server's registration entries match a
// JSON file, through the server's admin socket, then exits.
package main

import (
	"context"
	"os"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/entries"
	"github.com/sethvargo/go-envconfig/v2"
	entryv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/entry/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type config struct {
	// EntriesFile is a JSON array of entries; see README.md.
	EntriesFile string `env:"ENTRIES_FILE,required"`
	// IDPrefix marks the entries this owns: every entry in the file has an ID
	// starting with it, and entries with it that the file lacks are deleted.
	IDPrefix string `env:"ID_PREFIX,required"`
	// SocketPath is the server's admin API socket.
	SocketPath string `env:"SOCKET_PATH,default=/tmp/spire-server/private/api.sock"`
	// Timeout bounds the whole run, including waiting for the server to
	// start listening.
	Timeout time.Duration `env:"TIMEOUT,default=2m"`
}

func main() {
	ctx := context.Background()
	cfg := envconfig.MustProcess(ctx, &config{})
	if err := run(ctx, cfg); err != nil {
		clog.ErrorContext(ctx, "reconciling entries", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	f, err := os.Open(cfg.EntriesFile)
	if err != nil {
		return err
	}
	defer f.Close()
	want, err := entries.Load(f, cfg.IDPrefix)
	if err != nil {
		return err
	}

	// The socket may not exist yet when the server is still starting: calls
	// wait for it until the timeout.
	conn, err := grpc.NewClient("unix://"+cfg.SocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)))
	if err != nil {
		return err
	}
	defer conn.Close()

	res, err := entries.Reconcile(ctx, entryv1.NewEntryClient(conn), cfg.IDPrefix, want)
	clog.InfoContext(ctx, "reconciled entries", "declared", len(want),
		"created", res.Created, "updated", res.Updated, "deleted", res.Deleted)
	return err
}
