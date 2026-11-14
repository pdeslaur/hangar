package oidcrelay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pdeslaur/hangar/internal/workloadtest"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const (
	id     = "spiffe://example.org/svc/app"
	client = "00000000-0000-0000-0000-000000000001"
)

// provider is a fake OpenID provider, recording the token requests it gets.
type provider struct {
	*httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	forms    []url.Values
	// status is what its token endpoint answers.
	status int
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{status: http.StatusOK}
	p.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case DiscoveryPath:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                p.URL,
				"authorization_endpoint":                p.URL + "/authorize",
				"token_endpoint":                        p.URL + "/api/oidc/token",
				"userinfo_endpoint":                     p.URL + "/api/oidc/userinfo",
				"jwks_uri":                              p.URL + "/.well-known/jwks.json",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/api/oidc/token":
			r.ParseForm()
			p.mu.Lock()
			p.requests = append(p.requests, r)
			p.forms = append(p.forms, r.PostForm)
			status := p.status
			p.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Other", "dropped")
			w.WriteHeader(status)
			if status == http.StatusOK {
				io.WriteString(w, `{"access_token":"at","id_token":"it","token_type":"Bearer"}`)
			} else {
				io.WriteString(w, `{"error":"invalid_grant"}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *provider) tokenRequests() ([]*http.Request, []url.Values) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests), slices.Clone(p.forms)
}

// relay serves the relay in front of p, with JWT-SVIDs from api.
func relay(t *testing.T, p *provider, api *workloadtest.API) *httptest.Server {
	t.Helper()
	svids, err := workloadapi.New(t.Context(), workloadapi.WithAddr(api.Addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svids.Close() })
	srv := httptest.NewServer(&Handler{Issuer: p.URL, SVIDs: svids, Client: p.Client()})
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscovery(t *testing.T) {
	p := newProvider(t)
	srv := relay(t, p, workloadtest.Serve(t, id))

	resp, err := http.Get(srv.URL + DiscoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%s, Content-Type %q", resp.Status, resp.Header.Get("Content-Type"))
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if got, want := doc["token_endpoint"], srv.URL+TokenPath; got != want {
		t.Errorf("token_endpoint %v, want %s", got, want)
	}
	// The rest is the issuer's.
	for k, want := range map[string]string{
		"issuer":                 p.URL,
		"authorization_endpoint": p.URL + "/authorize",
		"userinfo_endpoint":      p.URL + "/api/oidc/userinfo",
		"jwks_uri":               p.URL + "/.well-known/jwks.json",
	} {
		if doc[k] != want {
			t.Errorf("%s %v, want %s", k, doc[k], want)
		}
	}
	if algs, _ := doc["id_token_signing_alg_values_supported"].([]any); len(algs) != 1 || algs[0] != "RS256" {
		t.Errorf("id_token_signing_alg_values_supported %v", doc["id_token_signing_alg_values_supported"])
	}
}

func TestDiscoveryIssuerUnreachable(t *testing.T) {
	p := newProvider(t)
	srv := relay(t, p, workloadtest.Serve(t, id))
	p.Close()

	resp, err := http.Get(srv.URL + DiscoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("%s, want 502", resp.Status)
	}
}

func TestToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		// basic sends the client's ID and secret with Basic auth, as
		// client_secret_basic does, rather than in the form.
		basic bool
	}{
		{name: "client_secret_post"},
		{name: "client_secret_basic", basic: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t)
			api := workloadtest.Serve(t, id)
			srv := relay(t, p, api)

			form := url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {"the-code"},
				"redirect_uri":  {"https://app.example.org/callback"},
				"code_verifier": {"the-verifier"},
			}
			if !tc.basic {
				form.Set("client_id", client)
				form.Set("client_secret", "placeholder")
			}
			req, _ := http.NewRequest(http.MethodPost, srv.URL+TokenPath, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.basic {
				req.SetBasicAuth(url.QueryEscape(client), "placeholder")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || string(body) != `{"access_token":"at","id_token":"it","token_type":"Bearer"}` {
				t.Fatalf("%s: %s", resp.Status, body)
			}
			if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Other") != "" {
				t.Errorf("headers %v", resp.Header)
			}

			requests, forms := p.tokenRequests()
			if len(forms) != 1 {
				t.Fatalf("%d token requests, want 1", len(forms))
			}
			if a := requests[0].Header.Get("Authorization"); a != "" {
				t.Errorf("forwarded Authorization %q", a)
			}
			got := forms[0]
			if got.Has("client_secret") {
				t.Error("forwarded client_secret")
			}
			if got.Get("client_assertion_type") != clientAssertionType || got.Get("client_assertion") == "" {
				t.Errorf("client assertion %q %q", got.Get("client_assertion_type"), got.Get("client_assertion"))
			}
			for _, k := range []string{"grant_type", "code", "redirect_uri", "code_verifier"} {
				if got.Get(k) != form.Get(k) {
					t.Errorf("%s %q, want %q", k, got.Get(k), form.Get(k))
				}
			}
			if got.Get("client_id") != client {
				t.Errorf("client_id %q, want %s", got.Get("client_id"), client)
			}
			if aud := api.Audiences(); !slices.Equal(aud, []string{p.URL}) {
				t.Errorf("SVID audiences %v, want the issuer", aud)
			}
		})
	}
}

// post posts form to the relay's token endpoint, returning the response's
// status and its error code.
func post(t *testing.T, srv *httptest.Server, form url.Values, basic string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+TokenPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic != "" {
		req.SetBasicAuth(basic, "placeholder")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&e)
	return resp.StatusCode, e.Error
}

func TestTokenRefused(t *testing.T) {
	for _, tc := range []struct {
		name       string
		form       url.Values
		basic      string
		status     int
		code       string
		forwarded  bool
		unready    bool
		issuerDown bool
	}{{
		name:   "without a client_id",
		form:   url.Values{"grant_type": {"authorization_code"}, "code": {"c"}},
		status: http.StatusBadRequest,
		code:   "invalid_request",
	}, {
		name:   "with another client_id than Basic's",
		form:   url.Values{"grant_type": {"authorization_code"}, "client_id": {"other"}},
		basic:  client,
		status: http.StatusBadRequest,
		code:   "invalid_request",
	}, {
		name:    "while the agent has no SVID",
		form:    url.Values{"grant_type": {"authorization_code"}, "client_id": {client}},
		unready: true,
		status:  http.StatusServiceUnavailable,
		code:    "temporarily_unavailable",
	}, {
		name:       "while the issuer is unreachable",
		form:       url.Values{"grant_type": {"authorization_code"}, "client_id": {client}},
		issuerDown: true,
		status:     http.StatusBadGateway,
		code:       "temporarily_unavailable",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t)
			api := workloadtest.Serve(t, id)
			srv := relay(t, p, api)
			if tc.unready {
				api.Unready(1000)
			}
			if tc.issuerDown {
				p.Close()
			}
			status, code := post(t, srv, tc.form, tc.basic)
			if status != tc.status || code != tc.code {
				t.Errorf("%d %q, want %d %q", status, code, tc.status, tc.code)
			}
			if _, forms := p.tokenRequests(); len(forms) != 0 {
				t.Errorf("forwarded %d token requests", len(forms))
			}
		})
	}
}

// The provider's errors reach the app as they are.
func TestTokenProviderError(t *testing.T) {
	p := newProvider(t)
	p.status = http.StatusBadRequest
	srv := relay(t, p, workloadtest.Serve(t, id))

	status, code := post(t, srv, url.Values{"grant_type": {"authorization_code"}, "client_id": {client}}, "")
	if status != http.StatusBadRequest || code != "invalid_grant" {
		t.Errorf("%d %q, want 400 invalid_grant", status, code)
	}
}

func TestMethodsAndPaths(t *testing.T) {
	p := newProvider(t)
	srv := relay(t, p, workloadtest.Serve(t, id))
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, DiscoveryPath, http.StatusMethodNotAllowed},
		{http.MethodGet, TokenPath, http.StatusMethodNotAllowed},
		{http.MethodGet, "/authorize", http.StatusNotFound},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s %s: %s, want %d", tc.method, tc.path, resp.Status, tc.status)
		}
	}
}
