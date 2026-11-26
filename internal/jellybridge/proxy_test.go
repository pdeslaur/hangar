package jellybridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/pdeslaur/hangar/internal/jellyfin"
)

// received is what the fake Jellyfin got.
type received struct {
	Authorization string
	EmbyToken     string
	Query         string
	Host          string
	ForwardedFor  string
}

// echo is a fake Jellyfin answering with what it received.
func echo(t *testing.T) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(received{
			Authorization: r.Header.Get("Authorization"),
			EmbyToken:     r.Header.Get("X-Emby-Token"),
			Query:         r.URL.RawQuery,
			Host:          r.Host,
			ForwardedFor:  r.Header.Get("X-Forwarded-For"),
		})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u
}

func TestProxy(t *testing.T) {
	ctx := context.Background()
	s, c := openStore(t)
	session := newSession(t, s, Browser)
	token, err := s.IssueToken(ctx, session.ID, 8*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := s.IssueToken(ctx, session.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Hour)
	p := NewProxy(echo(t), s, nil)

	web := jellyfin.Device{Client: "Jellyfin Web", Name: "Firefox Nightly", ID: "browser-says", Version: "10.11.1"}
	for _, tc := range []struct {
		name    string
		url     string
		headers map[string]string
		want    received
	}{{
		name:    "a bridge token in authorization",
		url:     "/Users/Me",
		headers: map[string]string{"Authorization": web.Header(token)},
		// The session's device ID, the client's name and version.
		want: received{Authorization: jellyfin.Device{Client: "Jellyfin Web", Name: "Firefox Nightly", ID: "dev1", Version: "10.11.1"}.Header("real")},
	}, {
		name: "a bridge token in the query",
		url:  "/Videos/1/stream?static=true&ApiKey=" + token,
		want: received{Authorization: session.Device.Header("real"), Query: "static=true"},
	}, {
		name:    "a bridge token in a legacy header",
		url:     "/Users/Me",
		headers: map[string]string{"X-Emby-Token": token},
		want:    received{Authorization: session.Device.Header("real")},
	}, {
		name:    "a Jellyfin token",
		url:     "/Users/Me?api_key=real",
		headers: map[string]string{"Authorization": web.Header("real"), "X-Emby-Token": "real"},
		want:    received{Authorization: web.Header("")},
	}, {
		name:    "an expired bridge token",
		url:     "/Users/Me",
		headers: map[string]string{"Authorization": web.Header(expired)},
		want:    received{Authorization: web.Header("")},
	}, {
		name: "a bridge token twice",
		url:  "/Users/Me?ApiKey=" + token + "&ApiKey=" + token,
		want: received{},
	}, {
		name: "anonymous",
		url:  "/System/Info/Public",
		want: received{},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://jellyfin.example.com"+tc.url, nil)
			r.Header.Set("X-Forwarded-For", "203.0.113.7")
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var got received
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			tc.want.Host = "jellyfin.example.com"
			tc.want.ForwardedFor = "203.0.113.7"
			if got != tc.want {
				t.Errorf("Jellyfin received %+v\nwant %+v", got, tc.want)
			}
		})
	}
}
