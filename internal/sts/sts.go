// Package sts logs in to Matrix through tuwunel-svid-sts, exchanging the
// workload's JWT-SVIDs for access tokens.
package sts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// SVIDSource fetches JWT-SVIDs, like the Workload API's workloadapi.JWTSource.
type SVIDSource interface {
	FetchJWTSVID(ctx context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error)
}

// Session is a Matrix session tuwunel-svid-sts logged in.
type Session struct {
	UserID      id.UserID   `json:"user_id"`
	AccessToken string      `json:"access_token"`
	DeviceID    id.DeviceID `json:"device_id"`
	// ExpiresInMS is how long the access token lasts, in milliseconds; 0
	// when it doesn't expire.
	ExpiresInMS int64 `json:"expires_in_ms"`
}

// ExpiresIn is how long the access token lasts; 0 when it doesn't expire.
func (s Session) ExpiresIn() time.Duration {
	return time.Duration(s.ExpiresInMS) * time.Millisecond
}

// Client logs in through tuwunel-svid-sts.
type Client struct {
	// URL is tuwunel-svid-sts's, like http://matrix:8080/ through the
	// pod's mTLS client.
	URL string
	// Audience is the audience tuwunel-svid-sts requires of JWT-SVIDs.
	Audience string
	SVIDs    SVIDSource
	HTTP     *http.Client
}

// maxResponse bounds the response it reads.
const maxResponse = 64 << 10

// Login logs in on device, or on a device tuwunel creates when it's empty,
// with a JWT-SVID fetched for the purpose.
func (c *Client) Login(ctx context.Context, device id.DeviceID) (Session, error) {
	body, err := json.Marshal(struct {
		DeviceID id.DeviceID `json:"device_id,omitempty"`
	}{device})
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := c.post(ctx, c.URL, body, &s); err != nil {
		return Session{}, err
	}
	if s.UserID == "" || s.AccessToken == "" || s.DeviceID == "" {
		return Session{}, fmt.Errorf("tuwunel-svid-sts answered an incomplete session for %q on %q", s.UserID, s.DeviceID)
	}
	if device != "" && s.DeviceID != device {
		return Session{}, fmt.Errorf("logged in on %s, not %s", s.DeviceID, device)
	}
	return s, nil
}

// CrossSigningKeys is a request replacing the user's cross-signing keys,
// which needs user-interactive auth: tuwunel-svid-sts completes it, with a JWT
// that never leaves it.
type CrossSigningKeys struct {
	// AccessToken is the relay's, of the user.
	AccessToken    string                   `json:"access_token"`
	MasterKey      mautrix.CrossSigningKeys `json:"master_key"`
	SelfSigningKey mautrix.CrossSigningKeys `json:"self_signing_key"`
	UserSigningKey mautrix.CrossSigningKeys `json:"user_signing_key"`
}

// ReplaceCrossSigningKeys replaces the user's cross-signing keys with keys,
// with a JWT-SVID fetched for the purpose.
func (c *Client) ReplaceCrossSigningKeys(ctx context.Context, keys CrossSigningKeys) error {
	u, err := url.JoinPath(c.URL, "cross-signing-keys")
	if err != nil {
		return err
	}
	body, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	return c.post(ctx, u, body, &struct{}{})
}

// post sends body to u with a JWT-SVID, decoding the answer into v.
func (c *Client) post(ctx context.Context, u string, body []byte, v any) error {
	svid, err := c.SVIDs.FetchJWTSVID(ctx, jwtsvid.Params{Audience: c.Audience})
	if err != nil {
		return fmt.Errorf("fetching a JWT-SVID: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+svid.Marshal())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("calling tuwunel-svid-sts: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("reading tuwunel-svid-sts's response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tuwunel-svid-sts answered %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decoding tuwunel-svid-sts's response: %w", err)
	}
	return nil
}
