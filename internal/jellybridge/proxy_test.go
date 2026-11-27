package jellybridge

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	p := NewProxy(echo(t), NewApps(s, nil))

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

func TestProxyAddsScript(t *testing.T) {
	const page = `<!DOCTYPE html><html><head><title>Jellyfin</title></head><body></body></html>`
	const headless = `<html><body></body></html>`
	// Like Jellyfin, it compresses what it can, and lets a browser keep its
	// copy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Etag", `"abc"`)
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		body := page
		if r.URL.Query().Has("headless") {
			body = headless
		}
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		io.WriteString(gz, body)
		gz.Close()
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	s, _ := openStore(t)
	p := NewProxy(target, NewApps(s, nil))

	withScript := `<!DOCTYPE html><html><head><title>Jellyfin</title>` + scriptTag + `</head><body></body></html>`
	for _, tc := range []struct {
		name, path, want string
		changed          bool
		encoding         string
		// cached is the ETag of a copy the browser holds.
		cached string
	}{
		{"page", "/web/", withScript, true, "", ""},
		{"index", "/web/index.html", withScript, true, "", ""},
		// A copy from before the bridge: the whole page again, with the
		// script.
		{"cached page", "/web/", withScript, true, "", `"abc"`},
		{"headless", "/web/index.html?headless", headless, false, "", ""},
		// Anything else passes as it came, compressed.
		{"another", "/web/other.html", page, false, "gzip", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.Header.Set("Accept-Encoding", "gzip")
			if tc.cached != "" {
				r.Header.Set("If-None-Match", tc.cached)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			resp := w.Result()
			if got := resp.Header.Get("Content-Encoding"); got != tc.encoding {
				t.Fatalf("Content-Encoding %q, want %q", got, tc.encoding)
			}
			var body io.Reader = resp.Body
			if tc.encoding == "gzip" {
				gz, err := gzip.NewReader(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				body = gz
			}
			b, _ := io.ReadAll(body)
			if string(b) != tc.want {
				t.Errorf("body %s, want %s", b, tc.want)
			}
			if got := resp.Header.Get("Content-Length"); tc.changed && got != fmt.Sprint(len(tc.want)) {
				t.Errorf("Content-Length %s, want %d", got, len(tc.want))
			}
			if etag := resp.Header.Get("Etag"); (etag == "") != tc.changed {
				t.Errorf("Etag %q, changed %v", etag, tc.changed)
			}
		})
	}
}

func TestProxyAddsMenuLink(t *testing.T) {
	configs := map[string]string{
		"/web/config.json":      `{"multiserver":false,"menuLinks":[{"name":"Wiki","url":"https://wiki.example"}],"plugins":["a"]}`,
		"/web/none/config.json": `{"multiserver":false}`,
	}
	// Like Jellyfin, it compresses what it can, and lets a browser keep its
	// copy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Etag", `"abc"`)
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		config := configs[r.URL.Path]
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			io.WriteString(w, config)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		io.WriteString(gz, config)
		gz.Close()
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	s, _ := openStore(t)
	p := NewProxy(target, NewApps(s, nil))

	get := func(path, cached string) (*http.Response, map[string]json.RawMessage) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Accept-Encoding", "gzip")
		if cached != "" {
			r.Header.Set("If-None-Match", cached)
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		resp := w.Result()
		var config map[string]json.RawMessage
		if resp.Header.Get("Content-Encoding") == "" {
			json.Unmarshal(w.Body.Bytes(), &config)
		}
		return resp, config
	}
	link := `{"icon":"devices","name":"My devices","url":"/sso/"}`
	for _, tc := range []struct {
		name, path, cached, links string
	}{
		{"with links", "/web/config.json", "", `[{"name":"Wiki","url":"https://wiki.example"},` + link + `]`},
		{"cached", "/web/config.json", `"abc"`, `[{"name":"Wiki","url":"https://wiki.example"},` + link + `]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, config := get(tc.path, tc.cached)
			if resp.StatusCode != http.StatusOK || string(config["menuLinks"]) != tc.links || string(config["plugins"]) != `["a"]` {
				t.Errorf("%d, config %s", resp.StatusCode, config)
			}
			if resp.Header.Get("Etag") != "" {
				t.Errorf("Etag %q on a changed config", resp.Header.Get("Etag"))
			}
		})
	}
	if _, config := get("/web/none/config.json", ""); config != nil {
		t.Errorf("a config.json elsewhere changed: %s", config)
	}
	if resp, _ := get("/web/none/config.json", ""); resp.Header.Get("Content-Encoding") != "gzip" {
		t.Error("a config.json elsewhere lost its encoding")
	}
}

func TestAddMenuLink(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"multiserver":false}`, `{"menuLinks":[{"icon":"devices","name":"My devices","url":"/sso/"}],"multiserver":false}`},
		{`{"menuLinks":null}`, `{"menuLinks":[{"icon":"devices","name":"My devices","url":"/sso/"}]}`},
		{`{"menuLinks":`, `{"menuLinks":`},
		{`[1]`, `[1]`},
	} {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(tc.in)),
			Request:    httptest.NewRequest(http.MethodGet, "/web/config.json", nil),
		}
		if err := addMenuLink(resp); err != nil {
			t.Fatal(err)
		}
		if b, _ := io.ReadAll(resp.Body); string(b) != tc.want {
			t.Errorf("addMenuLink(%s) = %s, want %s", tc.in, b, tc.want)
		}
	}
}
