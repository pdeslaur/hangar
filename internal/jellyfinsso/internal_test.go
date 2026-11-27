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
		name, body string
		want       int
	}{
		{"approved", `{"Code": " 123456 ", "User": "phil"}`, http.StatusNoContent},
		{"an unknown user", `{"Code": "123456", "User": "mallory"}`, http.StatusForbidden},
		{"no user", `{"Code": "123456"}`, http.StatusForbidden},
		{"a Jellyfin username", `{"Code": "123456", "User": "alice"}`, http.StatusForbidden},
		{"no such code", `{"Code": "000000", "User": "phil"}`, http.StatusNotFound},
		{"Jellyfin failing", `{"Code": "999999", "User": "phil"}`, http.StatusBadGateway},
		{"not JSON", `code=123456`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/internal/quickconnect", strings.NewReader(tc.body))
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
