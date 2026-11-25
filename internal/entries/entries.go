// Package entries reconciles a SPIRE server's registration entries with a
// declared set: it creates the missing ones, updates the changed ones, and
// deletes the ones it owns that the set no longer has. It owns the entries
// whose IDs start with a prefix, and leaves every other entry alone.
package entries

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	entryv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/entry/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc/codes"
)

// Entry is a registration entry as the file declares it.
type Entry struct {
	// ID is the entry's ID; it starts with the owned prefix.
	ID string `json:"id"`
	// SPIFFEID is the identity the entry issues, e.g. spiffe://example.org/web.
	SPIFFEID string `json:"spiffeID"`
	// ParentID is the agent, or entry, that may obtain it.
	ParentID string `json:"parentID"`
	// Selectors are "type:value", e.g. "unix:uid:1000"; a workload must match
	// them all.
	Selectors []string `json:"selectors"`
	// DNSNames are the X.509-SVID's DNS SANs, for a client checking the
	// server's name; the first is also its subject's common name. Optional.
	DNSNames []string `json:"dnsNames,omitempty"`
}

// Load reads a JSON array of entries and checks that each is well formed,
// has an ID starting with prefix, and that no two share an ID.
func Load(r io.Reader, prefix string) ([]Entry, error) {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	var es []Entry
	if err := d.Decode(&es); err != nil {
		return nil, fmt.Errorf("decoding entries: %w", err)
	}
	seen := map[string]bool{}
	for _, e := range es {
		if !strings.HasPrefix(e.ID, prefix) || e.ID == prefix {
			return nil, fmt.Errorf("entry %q: ID must start with %q", e.ID, prefix)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("entry %q: declared twice", e.ID)
		}
		seen[e.ID] = true
		if _, err := e.toAPI(); err != nil {
			return nil, fmt.Errorf("entry %q: %w", e.ID, err)
		}
	}
	return es, nil
}

// toAPI converts e to the server's representation.
func (e Entry) toAPI() (*types.Entry, error) {
	id, err := parseID(e.SPIFFEID)
	if err != nil {
		return nil, fmt.Errorf("spiffeID: %w", err)
	}
	parent, err := parseID(e.ParentID)
	if err != nil {
		return nil, fmt.Errorf("parentID: %w", err)
	}
	if len(e.Selectors) == 0 {
		return nil, errors.New("no selectors")
	}
	var ss []*types.Selector
	for _, s := range e.Selectors {
		typ, value, ok := strings.Cut(s, ":")
		if !ok || typ == "" || value == "" {
			return nil, fmt.Errorf("selector %q isn't type:value", s)
		}
		ss = append(ss, &types.Selector{Type: typ, Value: value})
	}
	if slices.Contains(e.DNSNames, "") {
		return nil, errors.New("empty DNS name")
	}
	return &types.Entry{Id: e.ID, SpiffeId: id, ParentId: parent, Selectors: ss, DnsNames: e.DNSNames}, nil
}

func parseID(s string) (*types.SPIFFEID, error) {
	id, err := spiffeid.FromString(s)
	if err != nil {
		return nil, err
	}
	return &types.SPIFFEID{TrustDomain: id.TrustDomain().Name(), Path: id.Path()}, nil
}

// Result names the entries a reconciliation changed.
type Result struct {
	Created, Updated, Deleted []string
}

// Reconcile makes the server's entries owned by prefix match want, which
// Load has checked. It deletes first, so that an entry moving to another ID
// doesn't collide with its old self, then updates, then creates. A failure
// for one entry doesn't stop the others; the error joins them all.
func Reconcile(ctx context.Context, c entryv1.EntryClient, prefix string, want []Entry) (Result, error) {
	have, err := list(ctx, c, prefix)
	if err != nil {
		return Result{}, err
	}

	var create, update []*types.Entry
	wanted := map[string]bool{}
	for _, e := range want {
		wanted[e.ID] = true
		w, err := e.toAPI()
		if err != nil {
			return Result{}, fmt.Errorf("entry %q: %w", e.ID, err)
		}
		switch h, ok := have[e.ID]; {
		case !ok:
			create = append(create, w)
		case !equal(h, w):
			update = append(update, w)
		}
	}
	var del []string
	for id := range have {
		if !wanted[id] {
			del = append(del, id)
		}
	}
	slices.Sort(del)

	var res Result
	var errs []error
	if len(del) > 0 {
		r, err := c.BatchDeleteEntry(ctx, &entryv1.BatchDeleteEntryRequest{Ids: del})
		if err != nil {
			return res, fmt.Errorf("deleting entries: %w", err)
		}
		for _, r := range r.Results {
			res.Deleted, errs = collect(res.Deleted, errs, "deleting", r.Id, r.Status)
		}
	}
	if len(update) > 0 {
		r, err := c.BatchUpdateEntry(ctx, &entryv1.BatchUpdateEntryRequest{
			Entries:   update,
			InputMask: &types.EntryMask{SpiffeId: true, ParentId: true, Selectors: true, DnsNames: true},
		})
		if err != nil {
			return res, errors.Join(append(errs, fmt.Errorf("updating entries: %w", err))...)
		}
		for i, r := range r.Results {
			res.Updated, errs = collect(res.Updated, errs, "updating", update[i].Id, r.Status)
		}
	}
	if len(create) > 0 {
		r, err := c.BatchCreateEntry(ctx, &entryv1.BatchCreateEntryRequest{Entries: create})
		if err != nil {
			return res, errors.Join(append(errs, fmt.Errorf("creating entries: %w", err))...)
		}
		for i, r := range r.Results {
			res.Created, errs = collect(res.Created, errs, "creating", create[i].Id, r.Status)
		}
	}
	return res, errors.Join(errs...)
}

// list returns the server's entries whose IDs start with prefix, by ID.
func list(ctx context.Context, c entryv1.EntryClient, prefix string) (map[string]*types.Entry, error) {
	have := map[string]*types.Entry{}
	req := &entryv1.ListEntriesRequest{
		OutputMask: &types.EntryMask{SpiffeId: true, ParentId: true, Selectors: true, DnsNames: true},
	}
	for {
		r, err := c.ListEntries(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("listing entries: %w", err)
		}
		for _, e := range r.Entries {
			if strings.HasPrefix(e.Id, prefix) {
				have[e.Id] = e
			}
		}
		if r.NextPageToken == "" {
			return have, nil
		}
		req.PageToken = r.NextPageToken
	}
}

// collect records id as done, or its failure.
func collect(done []string, errs []error, verb, id string, s *types.Status) ([]string, []error) {
	if c := codes.Code(s.GetCode()); c != codes.OK {
		return done, append(errs, fmt.Errorf("%s entry %q: %s: %s", verb, id, c, s.GetMessage()))
	}
	return append(done, id), errs
}

// equal reports whether a and b have the same SPIFFE ID, parent, selectors,
// in any order, and DNS names, in order: the first is the common name.
func equal(a, b *types.Entry) bool {
	return sameID(a.SpiffeId, b.SpiffeId) && sameID(a.ParentId, b.ParentId) &&
		slices.Equal(selectors(a), selectors(b)) && slices.Equal(a.DnsNames, b.DnsNames)
}

func sameID(a, b *types.SPIFFEID) bool {
	return a.GetTrustDomain() == b.GetTrustDomain() && a.GetPath() == b.GetPath()
}

func selectors(e *types.Entry) []string {
	var ss []string
	for _, s := range e.Selectors {
		ss = append(ss, s.Type+":"+s.Value)
	}
	slices.Sort(ss)
	return ss
}
