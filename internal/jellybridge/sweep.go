package jellybridge

import (
	"context"
	"errors"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/hangar/internal/jellyfin"
)

// Sweep ends the sessions idle past the store's Idle, logging their devices
// out of Jellyfin, and deletes the tokens past their expiry. A session it
// can't log out stays for the next sweep: Lookup refuses it already.
func Sweep(ctx context.Context, sessions *Store, client *jellyfin.Client) error {
	idle, err := sessions.IdleSessions(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, session := range idle {
		if err := client.Logout(ctx, session.JellyfinToken); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := sessions.DeleteSession(ctx, session.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		clog.InfoContext(ctx, "ended an idle session", "session", session.ID, "username", session.Username,
			"kind", session.Kind, "last_used", session.LastUsed)
	}
	if err := sessions.DeleteExpiredTokens(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
