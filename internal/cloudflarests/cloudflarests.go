// Package cloudflarests calls cloudflare-sts, the broker in the Cloudflare
// account that acts for a workload with its JWT-SVID: it hands a tunnel fresh
// credentials, sends email from the workload's address, or answers the ACME
// DNS-01 challenges for its names.
package cloudflarests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
)

// SVIDSource fetches JWT-SVIDs, like the Workload API's workloadapi.Client.
type SVIDSource interface {
	FetchJWTSVID(ctx context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error)
}

// Client calls cloudflare-sts, with a JWT-SVID fetched for each call.
type Client struct {
	// URL is the broker's, like https://cloudflare-sts.example.com: also the
	// audience of the JWT-SVIDs it takes.
	URL   string
	SVIDs SVIDSource
	HTTP  *http.Client
}

// NewHTTPClient is the HTTP client to call the broker with. It follows no
// redirect, which would send the JWT-SVID, and the message, elsewhere.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Error is the broker's answer to a call it refused, or couldn't complete.
type Error struct {
	Status      int
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("cloudflare-sts answered %d %s: %s", e.Status, e.Code, e.Description)
}

// Temporary reports whether retrying the call that returned err can help:
// the broker answered 503, or a gateway in front of it 502 or 504, or it
// didn't answer, or no JWT-SVID could be fetched to call it with.
func Temporary(err error) bool {
	if errors.As(err, new(unavailable)) {
		return true
	}
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// unavailable is an error reaching the broker, or the Workload API.
type unavailable struct{ error }

func (u unavailable) Unwrap() error { return u.error }

// TunnelCredentials are a tunnel's credentials, as cloudflared reads them
// from its credentials file.
type TunnelCredentials struct {
	AccountTag   string `json:"AccountTag"`
	TunnelID     string `json:"TunnelID"`
	TunnelSecret string `json:"TunnelSecret"`
}

// maxResponse bounds the response it reads.
const maxResponse = 64 << 10

// Tunnel exchanges a JWT-SVID for the credentials of profile's tunnel. The
// broker sets a new secret on the tunnel each time: the credentials it
// returned before no longer register connections.
func (c *Client) Tunnel(ctx context.Context, profile string) (TunnelCredentials, error) {
	svid, err := c.svid(ctx)
	if err != nil {
		return TunnelCredentials{}, err
	}
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
		"subject_token":      {svid},
		"scope":              {profile},
	}
	var answer struct {
		Tunnel struct {
			AccountTag   string `json:"account_tag"`
			TunnelID     string `json:"tunnel_id"`
			TunnelSecret string `json:"tunnel_secret"`
		} `json:"tunnel"`
	}
	err = c.call(ctx, http.MethodPost, "oauth/token", nil, "", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), &answer)
	if err != nil {
		return TunnelCredentials{}, err
	}
	creds := TunnelCredentials(answer.Tunnel)
	if creds.AccountTag == "" || creds.TunnelID == "" || creds.TunnelSecret == "" {
		return TunnelCredentials{}, fmt.Errorf("cloudflare-sts answered no tunnel credentials for %q", profile)
	}
	return creds, nil
}

// SendEmail sends message, a raw RFC 5322 message whose one From is from,
// to each of to, as profile allows. It returns the message IDs, one per
// recipient. When it fails, the message may have reached some recipients
// already.
func (c *Client) SendEmail(ctx context.Context, profile, from string, to []string, message io.Reader) ([]string, error) {
	svid, err := c.svid(ctx)
	if err != nil {
		return nil, err
	}
	query := url.Values{"profile": {profile}, "from": {from}, "to": to}
	var answer struct {
		MessageIDs []string `json:"message_ids"`
	}
	if err := c.call(ctx, http.MethodPost, "email", query, svid, "message/rfc822", message, &answer); err != nil {
		return nil, err
	}
	return answer.MessageIDs, nil
}

// PresentChallenge has the broker add the TXT record of an ACME DNS-01
// challenge, as profile allows: value, the challenge's digest, at name, the
// record's full name, _acme-challenge.<domain>. Adding one that's there
// already does nothing.
func (c *Client) PresentChallenge(ctx context.Context, profile, name, value string) error {
	return c.challenge(ctx, http.MethodPut, profile, name, value)
}

// CleanUpChallenge has the broker remove the record PresentChallenge added.
// Removing one that's gone does nothing.
func (c *Client) CleanUpChallenge(ctx context.Context, profile, name, value string) error {
	return c.challenge(ctx, http.MethodDelete, profile, name, value)
}

func (c *Client) challenge(ctx context.Context, method, profile, name, value string) error {
	svid, err := c.svid(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"profile": profile, "name": name, "value": value})
	if err != nil {
		return err
	}
	return c.call(ctx, method, "acme", nil, svid, "application/json", bytes.NewReader(body), nil)
}

func (c *Client) svid(ctx context.Context) (string, error) {
	svid, err := c.SVIDs.FetchJWTSVID(ctx, jwtsvid.Params{Audience: c.URL})
	if err != nil {
		return "", unavailable{fmt.Errorf("fetching a JWT-SVID: %w", err)}
	}
	return svid.Marshal(), nil
}

// call sends body to path under the broker's URL with method, with svid as
// a bearer token unless it's empty, decoding the answer into v unless it's
// nil.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, svid, contentType string, body io.Reader, v any) error {
	u, err := url.JoinPath(c.URL, path)
	if err != nil {
		return err
	}
	if query != nil {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if svid != "" {
		req.Header.Set("Authorization", "Bearer "+svid)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return unavailable{fmt.Errorf("calling cloudflare-sts: %w", err)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return unavailable{fmt.Errorf("reading cloudflare-sts's response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(b, e) != nil || e.Code == "" {
			e.Code, e.Description = resp.Status, string(bytes.TrimSpace(b))
		}
		return e
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decoding cloudflare-sts's response: %w", err)
	}
	return nil
}
