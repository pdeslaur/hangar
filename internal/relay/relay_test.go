package relay

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPickleKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pickle.key")
	first, err := pickleKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Errorf("generated %d bytes, want 32", len(first))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %v, want 0600", mode)
	}
	again, err := pickleKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, first) {
		t.Error("read another key than it generated")
	}

	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pickleKey(path); err == nil || !strings.Contains(err.Error(), "holds 5 bytes") {
		t.Errorf("err = %v for a short key", err)
	}
}

// TestSendRefuses checks the requests the relay refuses before calling the
// homeserver.
func TestSendRefuses(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	for _, tc := range []struct {
		name   string
		method string
		body   string
		ready  chan struct{}
		status int
	}{{
		name:   "GET",
		method: http.MethodGet,
		ready:  ready,
		status: http.StatusMethodNotAllowed,
	}, {
		name:   "not synced",
		method: http.MethodPost,
		body:   `{"room": "!a:example.org", "body": "hi"}`,
		ready:  make(chan struct{}),
		status: http.StatusServiceUnavailable,
	}, {
		name:   "not JSON",
		method: http.MethodPost,
		body:   "hi",
		ready:  ready,
		status: http.StatusBadRequest,
	}, {
		name:   "unknown field",
		method: http.MethodPost,
		body:   `{"room": "!a:example.org", "body": "hi", "format": "html"}`,
		ready:  ready,
		status: http.StatusBadRequest,
	}, {
		name:   "empty body",
		method: http.MethodPost,
		body:   `{"room": "!a:example.org"}`,
		ready:  ready,
		status: http.StatusBadRequest,
	}, {
		name:   "user as room",
		method: http.MethodPost,
		body:   `{"room": "@phil:example.org", "body": "hi"}`,
		ready:  ready,
		status: http.StatusBadRequest,
	}, {
		name:   "too large",
		method: http.MethodPost,
		body:   `{"room": "!a:example.org", "body": "` + strings.Repeat("a", maxSend) + `"}`,
		ready:  ready,
		status: http.StatusBadRequest,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Relay{ready: tc.ready}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Errorf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
		})
	}
}
