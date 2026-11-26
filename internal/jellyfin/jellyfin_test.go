package jellyfin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeviceHeader(t *testing.T) {
	d := Device{Client: "Jellyfin Web", Name: `Fire"fox`, ID: "abc=", Version: "10.11.0"}
	want := `MediaBrowser Client="Jellyfin+Web", Device="Fire%22fox", DeviceId="abc%3D", Version="10.11.0"`
	if got := d.Header(""); got != want {
		t.Errorf("Header(\"\") = %s, want %s", got, want)
	}
	want += `, Token="t0k"`
	if got := d.Header("t0k"); got != want {
		t.Errorf("Header(t0k) = %s, want %s", got, want)
	}
}

func TestLogout(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{
		{http.StatusNoContent, false},
		{http.StatusUnauthorized, false},
		{http.StatusInternalServerError, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/Sessions/Logout" {
				t.Errorf("request %s %s", r.Method, r.URL)
			}
			if got, want := r.Header.Get("Authorization"), `MediaBrowser Token="t0k"`; got != want {
				t.Errorf("Authorization %s, want %s", got, want)
			}
			w.WriteHeader(tc.status)
		}))
		c := &Client{URL: srv.URL, Key: "key", HTTP: srv.Client()}
		err := c.Logout(context.Background(), "t0k")
		if (err != nil) != tc.wantErr {
			t.Errorf("status %d: Logout: %v", tc.status, err)
		}
		srv.Close()
	}
}

func TestLoginNeedsDevice(t *testing.T) {
	c := &Client{URL: "http://127.0.0.1:1", Key: "key", HTTP: http.DefaultClient}
	if _, err := c.Login(context.Background(), "user", Device{Client: "c", Name: "n", Version: "1"}); err == nil {
		t.Error("Login without a device ID succeeded")
	}
}

func TestStatusErrorHidesQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, Key: "key", HTTP: srv.Client()}
	err := c.Do(context.Background(), http.MethodPost, "/QuickConnect/Authorize?code=123456", "", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || se.Path != "/QuickConnect/Authorize" {
		t.Errorf("do: %v, want a 403 StatusError for /QuickConnect/Authorize", err)
	}
}
