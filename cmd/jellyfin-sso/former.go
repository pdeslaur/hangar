package main

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/chainguard-dev/clog"
)

// formerName is jellyfin-sso's former name, which named its API key in
// Jellyfin and its database.
const formerName = "jellyfin-bridge"

// moveFormerState moves the database at from, of jellyfin-sso under its
// former name, to to, unless to exists already or from doesn't, keeping
// its sessions. SQLite's write-ahead log and shared memory go first and the
// database last, so that a move cut short is finished at the next start,
// rather than leaving the database without its log.
func moveFormerState(ctx context.Context, from, to string) error {
	if _, err := os.Stat(to); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(from); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", ""} {
		err := os.Rename(from+suffix, to+suffix)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	clog.InfoContext(ctx, "moved the sessions to the database's new name", "from", from, "to", to)
	return nil
}
