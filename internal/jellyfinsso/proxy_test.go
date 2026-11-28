package jellyfinsso

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
		name:    "a jellyfin-sso token in authorization",
		url:     "/Users/Me",
		headers: map[string]string{"Authorization": web.Header(token)},
		// The session's device ID, the client's name and version.
		want: received{Authorization: jellyfin.Device{Client: "Jellyfin Web", Name: "Firefox Nightly", ID: "dev1", Version: "10.11.1"}.Header("real")},
	}, {
		name: "a jellyfin-sso token in the query",
		url:  "/Videos/1/stream?static=true&ApiKey=" + token,
		want: received{Authorization: session.Device.Header("real"), Query: "static=true"},
	}, {
		name:    "a jellyfin-sso token in a legacy header",
		url:     "/Users/Me",
		headers: map[string]string{"X-Emby-Token": token},
		want:    received{Authorization: session.Device.Header("real")},
	}, {
		name:    "a Jellyfin token",
		url:     "/Users/Me?api_key=real",
		headers: map[string]string{"Authorization": web.Header("real"), "X-Emby-Token": "real"},
		want:    received{Authorization: web.Header("")},
	}, {
		name:    "an expired jellyfin-sso token",
		url:     "/Users/Me",
		headers: map[string]string{"Authorization": web.Header(expired)},
		want:    received{Authorization: web.Header("")},
	}, {
		name: "a jellyfin-sso token twice",
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
		// A copy from before jellyfin-sso: the whole page again, with the
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
			if cc := resp.Header.Get("Cache-Control"); (cc == "no-cache") != tc.changed {
				t.Errorf("Cache-Control %q, changed %v", cc, tc.changed)
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
			if resp.Header.Get("Etag") != "" || resp.Header.Get("Cache-Control") != "no-cache" {
				t.Errorf("Etag %q, Cache-Control %q on a changed config", resp.Header.Get("Etag"), resp.Header.Get("Cache-Control"))
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

func TestProxyHidesTokenInPlaybackInfo(t *testing.T) {
	// Like Jellyfin, it writes the request's token into TranscodingUrl, and
	// compresses what the client accepts. With ?br, it claims an encoding
	// Go's Transport leaves as it is.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := parseAuthorization(r.Header.Get("Authorization"))["Token"]
		body := `{"MediaSources":[{"Id":"abc","TranscodingUrl":"/videos/1/master.m3u8?DeviceId=dev1&MediaSourceId=abc&ApiKey=` + tok + `"}],"PlaySessionId":"p1"}`
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Has("huge") {
			body += strings.Repeat(" ", maxPlaybackInfo)
		}
		switch {
		case r.URL.Query().Has("br"):
			w.Header().Set("Content-Encoding", "br")
			io.WriteString(w, body)
		case strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"):
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			io.WriteString(gz, body)
			gz.Close()
		default:
			io.WriteString(w, body)
		}
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	ctx := context.Background()
	s, _ := openStore(t)
	session := newSession(t, s, Browser)
	token, err := s.IssueToken(ctx, session.ID, 8*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(target, NewApps(s, nil))

	for _, tc := range []struct {
		name, method, path string
		// header sends the token in Authorization rather than the query.
		header bool
		code   int
		// apiKey is TranscodingUrl's ApiKey.
		apiKey string
	}{
		{"get", http.MethodGet, "/Items/1/PlaybackInfo", true, http.StatusOK, token},
		{"post", http.MethodPost, "/Items/1/PlaybackInfo", true, http.StatusOK, token},
		{"query token", http.MethodPost, "/items/1/playbackinfo/?ApiKey=" + token, false, http.StatusOK, token},
		{"live stream", http.MethodPost, "/LiveStreams/Open", true, http.StatusOK, token},
		// Not a PlaybackInfo: forwarded as it came.
		{"another", http.MethodGet, "/Items/1/Other", true, http.StatusOK, "real"},
		{"encoded", http.MethodPost, "/Items/1/PlaybackInfo?br", true, http.StatusBadGateway, ""},
		{"huge", http.MethodPost, "/Items/1/PlaybackInfo?huge", true, http.StatusBadGateway, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Accept-Encoding", "gzip")
			if tc.header {
				r.Header.Set("Authorization", session.Device.Header(token))
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			resp := w.Result()
			if resp.StatusCode != tc.code {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.code, w.Body)
			}
			if tc.code != http.StatusOK {
				return
			}
			var body io.Reader = resp.Body
			if resp.Header.Get("Content-Encoding") == "gzip" {
				gz, err := gzip.NewReader(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				body = gz
			}
			raw, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Header.Get("Content-Length"); tc.apiKey == token && got != fmt.Sprint(len(raw)) {
				t.Errorf("Content-Length %s, want %d", got, len(raw))
			}
			var info struct {
				MediaSources []struct{ TranscodingUrl string }
			}
			if err := json.Unmarshal(raw, &info); err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(info.MediaSources[0].TranscodingUrl)
			if err != nil {
				t.Fatal(err)
			}
			if got := u.Query().Get("ApiKey"); got != tc.apiKey {
				t.Errorf("TranscodingUrl's ApiKey %q, want %q", got, tc.apiKey)
			}
		})
	}
}
