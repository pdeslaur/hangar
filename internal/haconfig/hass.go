package haconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// What Hass reports on.
const (
	// KindDeploy is deploying the repository's commits to Home Assistant.
	KindDeploy = "deploy"
	// KindSync is syncing the UI's edits to the repository.
	KindSync = "sync"
)

// Reloads, the domains Hass.Reload takes.
const (
	ReloadAutomation = "automation"
	ReloadScript     = "script"
	ReloadScene      = "scene"
	// ReloadAll reloads every integration configured in YAML, for packages.
	ReloadAll = "all"
)

// Hass is the config_sync integration, inside Home Assistant, reached on its
// Unix socket in the configuration directory.
type Hass struct {
	http *http.Client
}

// NewHass returns the Hass serving on socket.
func NewHass(socket string) *Hass {
	var d net.Dialer
	return &Hass{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.DialContext(ctx, "unix", socket)
			},
		},
		// A check loads every integration's configuration.
		Timeout: 2 * time.Minute,
	}}
}

// Version returns Home Assistant's version, and fails until it serves.
func (h *Hass) Version(ctx context.Context) (string, error) {
	var resp struct {
		Version string `json:"version"`
	}
	err := h.do(ctx, http.MethodGet, "/version", nil, &resp)
	return resp.Version, err
}

// Check checks the configuration as it is on disk, and returns its errors,
// empty when it's valid.
func (h *Hass) Check(ctx context.Context) (string, error) {
	var resp struct {
		Errors string `json:"errors"`
	}
	err := h.do(ctx, http.MethodPost, "/check", struct{}{}, &resp)
	return resp.Errors, err
}

// Reload reloads domains, Reload constants, one after the other.
func (h *Hass) Reload(ctx context.Context, domains []string) error {
	return h.do(ctx, http.MethodPost, "/reload", struct {
		Domains []string `json:"domains"`
	}{domains}, nil)
}

// Failed raises kind's notification with message, and fires
// config_sync_failed.
func (h *Hass) Failed(ctx context.Context, kind, sha, message string) error {
	return h.do(ctx, http.MethodPost, "/failed", struct {
		Kind    string `json:"kind"`
		SHA     string `json:"sha"`
		Message string `json:"message"`
	}{kind, sha, message}, nil)
}

// Succeeded dismisses kind's notification, and fires config_sync_succeeded.
func (h *Hass) Succeeded(ctx context.Context, kind, sha string) error {
	return h.do(ctx, http.MethodPost, "/succeeded", struct {
		Kind string `json:"kind"`
		SHA  string `json:"sha"`
	}{kind, sha}, nil)
}

// maxResponse bounds the responses it reads; a check's errors are text.
const maxResponse = 1 << 20

func (h *Hass) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	// The host is ignored: the transport dials the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://config_sync"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("config_sync %s %s: %s", method, path, e.Error)
		}
		return fmt.Errorf("config_sync %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
