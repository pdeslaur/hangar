package jellyfinsso

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pdeslaur/hangar/internal/jellyfin"
)

func TestInternalQuickConnect(t *testing.T) {
	// A Jellyfin knowing the user alice and the code 123456.
	var approved []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			io.WriteString(w, `[{"Name": "alice", "Id": "u1"}]`)
		case "/QuickConnect/Authorize":
			switch r.URL.Query().Get("code") {
			case "123456":
				approved = append(approved, r.URL.Query().Get("userId"))
				io.WriteString(w, "true")
			case "999999":
				http.Error(w, "broken", http.StatusInternalServerError)
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	store, _ := openStore(t)
	s := &SSO{
		Apps:       NewApps(store, &jellyfin.Client{URL: srv.URL, HTTP: srv.Client()}),
		UserHeader: userHeader,
		Users:      map[string]string{"phil": "alice"},
	}
	h := s.Internal()

	for _, tc := range []struct {
		name, contentType, body string
		want                    int
	}{
		{"approved", "application/json", `{"Code": " 123456 ", "User": "phil"}`, http.StatusNoContent},
		{"an unknown user", "application/json", `{"Code": "123456", "User": "mallory"}`, http.StatusForbidden},
		{"no user", "application/json", `{"Code": "123456"}`, http.StatusForbidden},
		{"a Jellyfin username", "application/json", `{"Code": "123456", "User": "alice"}`, http.StatusForbidden},
		{"no such code", "application/json; charset=utf-8", `{"Code": "000000", "User": "phil"}`, http.StatusNotFound},
		{"Jellyfin failing", "application/json", `{"Code": "999999", "User": "phil"}`, http.StatusBadGateway},
		{"not JSON", "application/json", `code=123456`, http.StatusBadRequest},
		// A form a page has a browser post, its body made JSON.
		{"a text/plain form", "text/plain", `{"Code": "123456", "User": "phil", "a": "="}`, http.StatusUnsupportedMediaType},
		{"a urlencoded form", "application/x-www-form-urlencoded", `{"Code": "123456", "User": "phil"}`, http.StatusUnsupportedMediaType},
		{"no content type", "", `{"Code": "123456", "User": "phil"}`, http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/internal/quickconnect", strings.NewReader(tc.body))
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			// The user header counts for nothing here.
			r.Header.Set(userHeader, "phil")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
	if len(approved) != 1 || approved[0] != "u1" {
		t.Errorf("approved for %v, want [u1]", approved)
	}

	for _, path := range []string{"/sso/", "/sso/login", "/internal/other"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d", path, w.Code)
		}
	}
}
