// Package jellyfin calls a Jellyfin server with an API key, to give its users
// access tokens without their passwords and to end them, and writes that API
// key into the server's database.
//
// A Jellyfin access token is an opaque string the server looks up, among its
// devices and then its API keys; nothing outside it can mint one. A device's
// token is made by a login, and Quick Connect logs a device in as any user an
// administrator names, which an API key's requests are.
package jellyfin

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
)

// maxResponse bounds how much of the server's responses it reads.
const maxResponse = 4 << 20

// maxErrorBody bounds how much of an error response a StatusError keeps:
// while it starts, Jellyfin answers with a whole HTML page.
const maxErrorBody = 256

// Client calls a Jellyfin server as an API key.
type Client struct {
	// URL is the server's base URL, without a trailing slash.
	URL string
	// Key is the API key.
	Key string
	// HTTP sends the requests.
	HTTP *http.Client
}

// Device is a client device as Jellyfin knows it, from the Authorization
// header its requests carry. A token belongs to one user's device: logging
// the same user in on the same ID ends that device's earlier tokens.
type Device struct {
	// Client is the app's name, like "Jellyfin Web".
	Client string
	// Name is the device's, like "Firefox".
	Name string
	// ID identifies the device across logins.
	ID string
	// Version is the app's.
	Version string
}

// Header is the Authorization header naming d, with token if it isn't empty.
// Jellyfin URL-decodes each value.
func (d Device) Header(token string) string {
	params := []string{
		param("Client", d.Client),
		param("Device", d.Name),
		param("DeviceId", d.ID),
		param("Version", d.Version),
	}
	if token != "" {
		params = append(params, param("Token", token))
	}
	return "MediaBrowser " + strings.Join(params, ", ")
}

func param(key, value string) string {
	return key + `="` + url.QueryEscape(value) + `"`
}

func (d Device) validate() error {
	if d.Client == "" || d.Name == "" || d.ID == "" || d.Version == "" {
		return errors.New("a device needs its client, name, ID and version")
	}
	return nil
}

// StatusError is a response the server answered with a status other than
// the one expected.
type StatusError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("jellyfin: %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// UserID returns the ID of the user called name, compared without case, as
// Jellyfin compares usernames.
func (c *Client) UserID(ctx context.Context, name string) (string, error) {
	var users []struct {
		Name string
		ID   string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodGet, "/Users", c.keyHeader(), nil, &users); err != nil {
		return "", err
	}
	for _, u := range users {
		if strings.EqualFold(u.Name, name) {
			return u.ID, nil
		}
	}
	return "", fmt.Errorf("jellyfin: no user %q", name)
}

// Ready returns nil once Jellyfin has started. While it starts, a setup
// server answers /System/Info/Public and 503 to everything else.
func (c *Client) Ready(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/Branding/Configuration", "", nil, nil)
}

// ServerID returns the server's ID, which clients' credentials name.
func (c *Client) ServerID(ctx context.Context) (string, error) {
	var info struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodGet, "/System/Info/Public", "", nil, &info); err != nil {
		return "", err
	}
	if info.ID == "" {
		return "", errors.New("jellyfin: no server ID")
	}
	return info.ID, nil
}

// AuthenticationResult is a login's result: the user, the device's session
// and its access token.
type AuthenticationResult struct {
	AccessToken string
	UserID      string
	Username    string
	Device      Device
	// Raw is the result as Jellyfin answered it, which apps read.
	Raw json.RawMessage
}

// ParseAuthenticationResult parses an AuthenticationResult as Jellyfin
// answers a login.
func ParseAuthenticationResult(raw []byte) (*AuthenticationResult, error) {
	var result struct {
		AccessToken string
		User        struct {
			ID   string `json:"Id"`
			Name string
		}
		SessionInfo struct {
			Client             string
			DeviceName         string
			DeviceID           string `json:"DeviceId"`
			ApplicationVersion string
		}
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("jellyfin: decoding an authentication result: %w", err)
	}
	if result.AccessToken == "" || result.User.ID == "" {
		return nil, errors.New("jellyfin: an authentication result without its token or user")
	}
	return &AuthenticationResult{
		AccessToken: result.AccessToken,
		UserID:      result.User.ID,
		Username:    result.User.Name,
		Device: Device{
			Client:  result.SessionInfo.Client,
			Name:    result.SessionInfo.DeviceName,
			ID:      result.SessionInfo.DeviceID,
			Version: result.SessionInfo.ApplicationVersion,
		},
		Raw: raw,
	}, nil
}

// Login logs device in as the user userID with Quick Connect, which has to be
// enabled. It ends the tokens the same user had on the same device ID.
func (c *Client) Login(ctx context.Context, userID string, device Device) (*AuthenticationResult, error) {
	if err := device.validate(); err != nil {
		return nil, err
	}
	var request struct {
		Secret string
		Code   string
	}
	if err := c.do(ctx, http.MethodPost, "/QuickConnect/Initiate", device.Header(""), nil, &request); err != nil {
		return nil, err
	}
	if err := c.AuthorizeQuickConnect(ctx, request.Code, userID); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	body := map[string]string{"Secret": request.Secret}
	if err := c.do(ctx, http.MethodPost, "/Users/AuthenticateWithQuickConnect", device.Header(""), body, &raw); err != nil {
		return nil, err
	}
	return ParseAuthenticationResult(raw)
}

// AuthorizeQuickConnect logs the device showing the Quick Connect code in as
// the user userID.
func (c *Client) AuthorizeQuickConnect(ctx context.Context, code, userID string) error {
	path := "/QuickConnect/Authorize?" + url.Values{"code": {code}, "userId": {userID}}.Encode()
	return c.do(ctx, http.MethodPost, path, c.keyHeader(), nil, nil)
}

// Logout ends token, logging out with it. A token already ended is no error.
//
// It ends that token alone, where deleting its device through the API would
// end every user's tokens on the same device ID: one browser shared by two
// people.
func (c *Client) Logout(ctx context.Context, token string) error {
	err := c.do(ctx, http.MethodPost, "/Sessions/Logout", tokenHeader(token), nil, nil)
	if se := (*StatusError)(nil); errors.As(err, &se) && se.Status == http.StatusUnauthorized {
		return nil
	}
	return err
}

func (c *Client) keyHeader() string {
	return tokenHeader(c.Key)
}

func tokenHeader(token string) string {
	return "MediaBrowser " + param("Token", token)
}

// do sends a request with authorization as its Authorization header and in,
// if not nil, as its JSON body, and decodes a JSON response into out, if not
// nil. Any status but 2xx is a *StatusError.
func (c *Client) do(ctx context.Context, method, path, authorization string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The path's query can hold a Quick Connect code: leave it out.
		p, _, _ := strings.Cut(path, "?")
		if len(b) > maxErrorBody {
			b = b[:maxErrorBody]
		}
		return &StatusError{Method: method, Path: p, Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("jellyfin: decoding %s %s: %w", method, path, err)
	}
	return nil
}
