package jellyfinsso

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pdeslaur/hangar/internal/jellyfin"
)

// TestParseParams has Jellyfin's own cases for GetParts
// (tests/Jellyfin.Api.Tests/Auth/DefaultAuthorizationPolicy), and some of
// its clients' headers.
func TestParseParams(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want map[string]string
	}{
		{`x="123,123",y="123"`, map[string]string{"x": "123,123", "y": "123"}},
		{`x="123,123",         y="123",z="'hi'"`, map[string]string{"x": "123,123", "y": "123", "z": "'hi'"}},
		{`x="ab"`, map[string]string{"x": "ab"}},
		{`param=Hörbücher`, map[string]string{"param": "Hörbücher"}},
		{`param=%22%Hörbücher`, map[string]string{"param": `"%Hörbücher`}},
		{`Client="Jellyfin Web", Device="Firefox", DeviceId="TW96aWxsYTEx", Version="10.11.0", Token="abc"`,
			map[string]string{"Client": "Jellyfin Web", "Device": "Firefox", "DeviceId": "TW96aWxsYTEx", "Version": "10.11.0", "Token": "abc"}},
		{`Client="Infuse", Device="Apple+TV", Token="abc", Token="def"`,
			map[string]string{"Client": "Infuse", "Device": "Apple TV", "Token": "def"}},
	} {
		if got := parseParams(tc.in); !maps.Equal(got, tc.want) {
			t.Errorf("parseParams(%s) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTakeCredentials(t *testing.T) {
	web := jellyfin.Device{Client: "Jellyfin Web", Name: "Firefox", ID: "dev1", Version: "10.11.0"}
	for _, tc := range []struct {
		name      string
		url       string
		headers   map[string]string
		want      credentials
		wantQuery string
	}{{
		name:    "authorization",
		url:     "/Users/Me",
		headers: map[string]string{"Authorization": web.Header("abc")},
		want:    credentials{device: web, token: "abc"},
	}, {
		name:    "emby scheme",
		url:     "/Users/Me",
		headers: map[string]string{"Authorization": `Emby Token="abc"`},
		want:    credentials{token: "abc"},
	}, {
		name:    "another scheme",
		url:     "/Users/Me",
		headers: map[string]string{"Authorization": `Bearer abc`},
	}, {
		name:    "x-emby-authorization without authorization",
		url:     "/Users/Me",
		headers: map[string]string{"X-Emby-Authorization": web.Header("abc")},
		want:    credentials{device: web, token: "abc"},
	}, {
		name:    "authorization before x-emby-authorization",
		url:     "/Users/Me",
		headers: map[string]string{"Authorization": web.Header("abc"), "X-Emby-Authorization": web.Header("def")},
		want:    credentials{device: web, token: "abc"},
	}, {
		name:    "authorization's token first",
		url:     "/Users/Me?api_key=ghi",
		headers: map[string]string{"Authorization": web.Header("abc"), "X-Emby-Token": "def"},
		want:    credentials{device: web, token: "abc"},
	}, {
		name:    "x-emby-token",
		url:     "/Users/Me",
		headers: map[string]string{"Authorization": web.Header(""), "X-Emby-Token": "def", "X-MediaBrowser-Token": "ghi"},
		want:    credentials{device: web, token: "def"},
	}, {
		name:    "x-mediabrowser-token",
		url:     "/Users/Me",
		headers: map[string]string{"X-MediaBrowser-Token": "ghi"},
		want:    credentials{token: "ghi"},
	}, {
		name:      "apikey in the query, others kept as they were",
		url:       "/Videos/1/stream?static=true&ApiKey=abc&Tag=a%2Bb",
		want:      credentials{token: "abc"},
		wantQuery: "static=true&Tag=a%2Bb",
	}, {
		name:      "apikey in any case, before api_key",
		url:       "/socket?api_key=def&APIKEY=abc&deviceId=x",
		want:      credentials{token: "abc"},
		wantQuery: "deviceId=x",
	}, {
		name:      "an encoded name",
		url:       "/socket?Api%4Bey=abc",
		want:      credentials{token: "abc"},
		wantQuery: "",
	}, {
		name:      "twice, joined as ASP.NET joins them",
		url:       "/Users/Me?ApiKey=abc&ApiKey=def",
		want:      credentials{token: "abc,def"},
		wantQuery: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.url, nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			got := takeCredentials(r)
			if got != tc.want {
				t.Errorf("takeCredentials = %+v, want %+v", got, tc.want)
			}
			for _, h := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token"} {
				if v := r.Header.Get(h); v != "" {
					t.Errorf("%s left: %s", h, v)
				}
			}
			if r.URL.RawQuery != tc.wantQuery {
				t.Errorf("query left %q, want %q", r.URL.RawQuery, tc.wantQuery)
			}
		})
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		"abc":       "abc",
		"a+b":       "a b",
		"a%2Bb":     "a+b",
		"%e2%9c%93": "✓",
		"100%":      "100%",
		"%4":        "%4",
		"%zz":       "%zz",
	} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}
