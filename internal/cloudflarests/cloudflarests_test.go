package cloudflarests

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/pdeslaur/hangar/internal/workloadtest"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const id = "spiffe://example.org/svc/app"

// client calls broker with JWT-SVIDs from a fake Workload API.
func client(t *testing.T, broker *httptest.Server) (*Client, *workloadtest.API) {
	t.Helper()
	api := workloadtest.Serve(t, id)
	svids, err := workloadapi.New(t.Context(), workloadapi.WithAddr(api.Addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svids.Close() })
	return &Client{URL: broker.URL, SVIDs: svids, HTTP: NewHTTPClient()}, api
}

// subject is the SPIFFE ID of token, a JWT-SVID for audience.
func subject(t *testing.T, token, audience string) string {
	t.Helper()
	svid, err := jwtsvid.ParseInsecure(token, []string{audience})
	if err != nil {
		t.Errorf("parsing the JWT-SVID: %v", err)
		return ""
	}
	return svid.ID.String()
}

func TestTunnel(t *testing.T) {
	var audience string
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization %q, want none", r.Header.Get("Authorization"))
		}
		for field, want := range map[string]string{
			"grant_type":         "urn:ietf:params:oauth:grant-type:token-exchange",
			"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
			"scope":              id + ":tunnel",
		} {
			if got := r.PostFormValue(field); got != want {
				t.Errorf("%s %q, want %q", field, got, want)
			}
		}
		if sub := subject(t, r.PostFormValue("subject_token"), audience); sub != id {
			t.Errorf("subject_token for %q, want %q", sub, id)
		}
		io.WriteString(w, `{
			"issued_token_type": "urn:cloudflare-sts:params:oauth:token-type:tunnel_credentials",
			"token_type": "N_A",
			"scope": "`+id+`:tunnel",
			"account_id": "account",
			"tunnel": {"account_tag": "account", "tunnel_id": "tunnel", "tunnel_secret": "c2VjcmV0"}
		}`)
	}))
	defer broker.Close()
	audience = broker.URL
	c, api := client(t, broker)

	creds, err := c.Tunnel(t.Context(), id+":tunnel")
	if err != nil {
		t.Fatal(err)
	}
	want := TunnelCredentials{AccountTag: "account", TunnelID: "tunnel", TunnelSecret: "c2VjcmV0"}
	if creds != want {
		t.Errorf("credentials %+v, want %+v", creds, want)
	}
	if got := api.Audiences(); !slices.Equal(got, []string{broker.URL}) {
		t.Errorf("JWT-SVIDs for %q, want one for %q", got, broker.URL)
	}
}

func TestTunnelWithoutCredentials(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token": "token", "token_type": "Bearer"}`)
	}))
	defer broker.Close()
	c, _ := client(t, broker)

	if _, err := c.Tunnel(t.Context(), id+":token"); err == nil || Temporary(err) {
		t.Errorf("error %v, want a lasting one", err)
	}
}

func TestSendEmail(t *testing.T) {
	const message = "From: app@example.org\r\nTo: you@example.com\r\nSubject: Hi\r\n\r\nHello\r\n"
	var audience string
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/email" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("profile") != id+":email" || q.Get("from") != "app@example.org" || !slices.Equal(q["to"], []string{"you@example.com", "them@example.com"}) {
			t.Errorf("query %v", q)
		}
		if got := r.Header.Get("Content-Type"); got != "message/rfc822" {
			t.Errorf("Content-Type %q, want message/rfc822", got)
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			t.Errorf("Authorization %q, want a bearer token", r.Header.Get("Authorization"))
		}
		if sub := subject(t, token, audience); sub != id {
			t.Errorf("bearer token for %q, want %q", sub, id)
		}
		if body, _ := io.ReadAll(r.Body); string(body) != message {
			t.Errorf("body %q, want %q", body, message)
		}
		io.WriteString(w, `{"message_ids": ["<1@example.org>", "<2@example.org>"]}`)
	}))
	defer broker.Close()
	audience = broker.URL
	c, _ := client(t, broker)

	ids, err := c.SendEmail(t.Context(), id+":email", "app@example.org", []string{"you@example.com", "them@example.com"}, strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"<1@example.org>", "<2@example.org>"}; !slices.Equal(ids, want) {
		t.Errorf("message IDs %q, want %q", ids, want)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		want      Error
		temporary bool
	}{{
		name:   "refused",
		status: http.StatusForbidden,
		body:   `{"error": "insufficient_scope", "error_description": "not allowed"}`,
		want:   Error{Status: http.StatusForbidden, Code: "insufficient_scope", Description: "not allowed"},
	}, {
		name:      "unavailable",
		status:    http.StatusServiceUnavailable,
		body:      `{"error": "temporarily_unavailable", "error_description": "Cloudflare: down"}`,
		want:      Error{Status: http.StatusServiceUnavailable, Code: "temporarily_unavailable", Description: "Cloudflare: down"},
		temporary: true,
	}, {
		name:      "gateway",
		status:    http.StatusBadGateway,
		body:      "bad gateway\n",
		want:      Error{Status: http.StatusBadGateway, Code: "502 Bad Gateway", Description: "bad gateway"},
		temporary: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer broker.Close()
			c, _ := client(t, broker)

			_, err := c.SendEmail(t.Context(), id+":email", "app@example.org", []string{"you@example.com"}, strings.NewReader("x"))
			var e *Error
			if !errors.As(err, &e) || *e != tc.want {
				t.Fatalf("error %v, want %v", err, &tc.want)
			}
			if Temporary(err) != tc.temporary {
				t.Errorf("temporary %t, want %t", !tc.temporary, tc.temporary)
			}
		})
	}
}

func TestTemporaryWithoutSVID(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("called the broker without a JWT-SVID")
	}))
	defer broker.Close()
	c, api := client(t, broker)
	api.Unready(1)

	if _, err := c.Tunnel(t.Context(), id+":tunnel"); !Temporary(err) {
		t.Errorf("error %v, want a temporary one", err)
	}
}

func TestFollowsNoRedirect(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect to %s", r.URL)
	}))
	defer elsewhere.Close()
	broker := httptest.NewServer(http.RedirectHandler(elsewhere.URL, http.StatusTemporaryRedirect))
	defer broker.Close()
	c, _ := client(t, broker)

	_, err := c.Tunnel(t.Context(), id+":tunnel")
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusTemporaryRedirect {
		t.Errorf("error %v, want the redirect", err)
	}
}
