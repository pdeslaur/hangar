// Package caddydns is a Caddy DNS provider, dns.providers.cloudflare_sts,
// that answers ACME DNS-01 challenges through cloudflare-sts with the
// workload's JWT-SVID, so Caddy holds no DNS token. The broker writes each
// challenge's TXT record itself, only for the names the workload's acme
// profile lists.
//
//	tls {
//		dns cloudflare_sts {
//			url     https://cloudflare-sts.example.com
//			profile spiffe://example.com/svc/app:acme
//		}
//	}
//
// It fetches JWT-SVIDs from the Workload API at SPIFFE_ENDPOINT_SOCKET.
package caddydns

import (
	"context"
	"fmt"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/libdns/libdns"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"github.com/pdeslaur/hangar/internal/cloudflarests"
)

func init() {
	caddy.RegisterModule(Provider{})
}

// Provider answers the challenges of the names its profile lists.
type Provider struct {
	// URL is the broker's, like https://cloudflare-sts.example.com.
	URL string `json:"url,omitempty"`
	// Profile is the acme profile to answer as.
	Profile string `json:"profile,omitempty"`

	broker *cloudflarests.Client
	svids  *workloadapi.Client
}

// CaddyModule returns the Caddy module information.
func (Provider) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "dns.providers.cloudflare_sts",
		New: func() caddy.Module { return new(Provider) },
	}
}

// Provision fills in placeholders such as {env.BROKER_URL}, and opens the
// Workload API.
func (p *Provider) Provision(ctx caddy.Context) error {
	repl := caddy.NewReplacer()
	p.URL = repl.ReplaceAll(p.URL, "")
	p.Profile = repl.ReplaceAll(p.Profile, "")
	if p.URL == "" || p.Profile == "" {
		return fmt.Errorf("cloudflare_sts needs a url and a profile")
	}
	svids, err := workloadapi.New(ctx)
	if err != nil {
		return fmt.Errorf("opening the Workload API: %w", err)
	}
	p.svids = svids
	p.broker = &cloudflarests.Client{URL: p.URL, SVIDs: svids, HTTP: cloudflarests.NewHTTPClient()}
	return nil
}

// Cleanup closes the Workload API.
func (p *Provider) Cleanup() error {
	if p.svids == nil {
		return nil
	}
	return p.svids.Close()
}

// UnmarshalCaddyfile sets up the provider from Caddyfile tokens:
//
//	cloudflare_sts {
//		url     <broker URL>
//		profile <acme profile>
//	}
func (p *Provider) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the provider's name
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		var field *string
		switch d.Val() {
		case "url":
			field = &p.URL
		case "profile":
			field = &p.Profile
		default:
			return d.Errf("unrecognized subdirective %q", d.Val())
		}
		if !d.NextArg() {
			return d.ArgErr()
		}
		*field = d.Val()
		if d.NextArg() {
			return d.ArgErr()
		}
	}
	return nil
}

// AppendRecords has the broker add recs, each a challenge's TXT record, in
// zone.
func (p *Provider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	for _, rec := range recs {
		name, value, err := challenge(zone, rec)
		if err != nil {
			return nil, err
		}
		if err := p.broker.PresentChallenge(ctx, p.Profile, name, value); err != nil {
			return nil, err
		}
	}
	return recs, nil
}

// DeleteRecords has the broker remove recs, which AppendRecords added.
func (p *Provider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	for _, rec := range recs {
		name, value, err := challenge(zone, rec)
		if err != nil {
			return nil, err
		}
		if err := p.broker.CleanUpChallenge(ctx, p.Profile, name, value); err != nil {
			return nil, err
		}
	}
	return recs, nil
}

// challenge is rec's full name, without the trailing dot, and its value. The
// broker writes only TXT records.
func challenge(zone string, rec libdns.Record) (name, value string, err error) {
	rr := rec.RR()
	if rr.Type != "TXT" {
		return "", "", fmt.Errorf("cloudflare_sts writes only TXT records, not %s", rr.Type)
	}
	return strings.TrimSuffix(libdns.AbsoluteName(rr.Name, zone), "."), rr.Data, nil
}

var (
	_ caddy.Provisioner     = (*Provider)(nil)
	_ caddy.CleanerUpper    = (*Provider)(nil)
	_ caddyfile.Unmarshaler = (*Provider)(nil)
	_ libdns.RecordAppender = (*Provider)(nil)
	_ libdns.RecordDeleter  = (*Provider)(nil)
)
