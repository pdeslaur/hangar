package caddydns

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/libdns/libdns"

	"github.com/pdeslaur/hangar/internal/workloadtest"
)

const (
	id      = "spiffe://example.org/svc/app"
	profile = id + ":acme"
	digest  = "LoqXcYV8q5ONbJQxbmR7SCTNo3tiAXDfowyjxAjEuX0"
)

// call is a challenge the broker was asked to add or remove.
type call struct {
	Method, Profile, Name, Value string
}

// provisioned is a Provider for a fake broker that records its calls, with
// JWT-SVIDs from a fake Workload API.
func provisioned(t *testing.T) (*Provider, *[]call) {
	t.Helper()
	var calls []call
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c call
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Error(err)
		}
		c.Method = r.Method
		calls = append(calls, c)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(broker.Close)
	api := workloadtest.Serve(t, id)
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", api.Addr)

	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	t.Cleanup(cancel)
	p := &Provider{URL: broker.URL, Profile: profile}
	if err := p.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Cleanup() })
	return p, &calls
}

func TestAnswersAChallenge(t *testing.T) {
	p, calls := provisioned(t)
	rec := libdns.TXT{Name: "_acme-challenge.app", TTL: time.Minute, Text: digest}

	added, err := p.AppendRecords(t.Context(), "example.org.", []libdns.Record{rec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeleteRecords(t.Context(), "example.org.", added); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{http.MethodPut, profile, "_acme-challenge.app.example.org", digest},
		{http.MethodDelete, profile, "_acme-challenge.app.example.org", digest},
	}
	if !slices.Equal(*calls, want) {
		t.Errorf("calls %+v, want %+v", *calls, want)
	}
}

func TestWritesOnlyTXT(t *testing.T) {
	p, calls := provisioned(t)
	rec := libdns.RR{Name: "app", Type: "CNAME", Data: "elsewhere.example.com."}
	if _, err := p.AppendRecords(t.Context(), "example.org.", []libdns.Record{rec}); err == nil {
		t.Error("added a CNAME")
	}
	if len(*calls) != 0 {
		t.Errorf("called the broker: %+v", *calls)
	}
}

func TestProvisionNeedsURLAndProfile(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	defer cancel()
	for _, p := range []*Provider{{URL: "https://sts.example.org"}, {Profile: profile}} {
		if err := p.Provision(ctx); err == nil {
			t.Errorf("provisioned %+v", p)
		}
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	var p Provider
	d := caddyfile.NewTestDispenser(`cloudflare_sts {
		url https://sts.example.org
		profile ` + profile + `
	}`)
	if err := p.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://sts.example.org" || p.Profile != profile {
		t.Errorf("got %+v", p)
	}

	for _, bad := range []string{
		"cloudflare_sts token",
		"cloudflare_sts {\n url\n}",
		"cloudflare_sts {\n url a b\n}",
		"cloudflare_sts {\n api_token x\n}",
	} {
		var p Provider
		if err := p.UnmarshalCaddyfile(caddyfile.NewTestDispenser(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
