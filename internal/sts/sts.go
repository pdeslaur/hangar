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
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
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
	svid, err := c.SVIDs.FetchJWTSVID(ctx, jwtsvid.Params{Audience: c.Audience})
	if err != nil {
		return Session{}, fmt.Errorf("fetching a JWT-SVID: %w", err)
	}
	body, err := json.Marshal(struct {
		DeviceID id.DeviceID `json:"device_id,omitempty"`
	}{device})
	if err != nil {
		return Session{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("Authorization", "Bearer "+svid.Marshal())
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Session{}, fmt.Errorf("calling tuwunel-svid-sts: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return Session{}, fmt.Errorf("reading tuwunel-svid-sts's response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Session{}, fmt.Errorf("tuwunel-svid-sts answered %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return Session{}, fmt.Errorf("decoding tuwunel-svid-sts's response: %w", err)
	}
	if s.UserID == "" || s.AccessToken == "" || s.DeviceID == "" {
		return Session{}, fmt.Errorf("tuwunel-svid-sts answered an incomplete session for %q on %q", s.UserID, s.DeviceID)
	}
	if device != "" && s.DeviceID != device {
		return Session{}, fmt.Errorf("logged in on %s, not %s", s.DeviceID, device)
	}
	return s, nil
}
