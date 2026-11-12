package entries

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	entryv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/entry/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// fake is a server's entries, listed two to a page. Creating an ID in fail
// fails for that entry alone.
type fake struct {
	entryv1.EntryClient
	entries map[string]*types.Entry
	fail    map[string]bool
	calls   []string
}

func (f *fake) ListEntries(_ context.Context, r *entryv1.ListEntriesRequest, _ ...grpc.CallOption) (*entryv1.ListEntriesResponse, error) {
	ids := slices.Sorted(maps.Keys(f.entries))
	start := 0
	if r.PageToken != "" {
		start, _ = strconv.Atoi(r.PageToken)
	}
	end := min(start+2, len(ids))
	res := &entryv1.ListEntriesResponse{}
	for _, id := range ids[start:end] {
		res.Entries = append(res.Entries, f.entries[id])
	}
	if end < len(ids) {
		res.NextPageToken = strconv.Itoa(end)
	}
	return res, nil
}

func (f *fake) BatchCreateEntry(_ context.Context, r *entryv1.BatchCreateEntryRequest, _ ...grpc.CallOption) (*entryv1.BatchCreateEntryResponse, error) {
	res := &entryv1.BatchCreateEntryResponse{}
	for _, e := range r.Entries {
		f.calls = append(f.calls, "create "+e.Id)
		s := &types.Status{}
		switch {
		case f.fail[e.Id]:
			s = &types.Status{Code: int32(codes.InvalidArgument), Message: "rejected"}
		case f.entries[e.Id] != nil:
			s = &types.Status{Code: int32(codes.AlreadyExists), Message: "exists"}
		default:
			f.entries[e.Id] = e
		}
		res.Results = append(res.Results, &entryv1.BatchCreateEntryResponse_Result{Status: s, Entry: e})
	}
	return res, nil
}

func (f *fake) BatchUpdateEntry(_ context.Context, r *entryv1.BatchUpdateEntryRequest, _ ...grpc.CallOption) (*entryv1.BatchUpdateEntryResponse, error) {
	m := r.InputMask
	if !m.SpiffeId || !m.ParentId || !m.Selectors {
		return nil, fmt.Errorf("unexpected mask %v", m)
	}
	res := &entryv1.BatchUpdateEntryResponse{}
	for _, e := range r.Entries {
		f.calls = append(f.calls, "update "+e.Id)
		f.entries[e.Id] = e
		res.Results = append(res.Results, &entryv1.BatchUpdateEntryResponse_Result{Status: &types.Status{}, Entry: e})
	}
	return res, nil
}

func (f *fake) BatchDeleteEntry(_ context.Context, r *entryv1.BatchDeleteEntryRequest, _ ...grpc.CallOption) (*entryv1.BatchDeleteEntryResponse, error) {
	res := &entryv1.BatchDeleteEntryResponse{}
	for _, id := range r.Ids {
		f.calls = append(f.calls, "delete "+id)
		delete(f.entries, id)
		res.Results = append(res.Results, &entryv1.BatchDeleteEntryResponse_Result{Status: &types.Status{}, Id: id})
	}
	return res, nil
}

func entry(id, path, parent string, selectors ...string) Entry {
	return Entry{
		ID:        id,
		SPIFFEID:  "spiffe://example.org" + path,
		ParentID:  "spiffe://example.org/spire/agent/x509pop/" + parent,
		Selectors: selectors,
	}
}

func server(t *testing.T, es ...Entry) *fake {
	t.Helper()
	f := &fake{entries: map[string]*types.Entry{}}
	for _, e := range es {
		a, err := e.toAPI()
		if err != nil {
			t.Fatal(err)
		}
		f.entries[e.ID] = a
	}
	return f
}

func TestReconcile(t *testing.T) {
	web := entry("m-web", "/web", "a", "unix:uid:1001")
	db := entry("m-db", "/db", "a", "unix:uid:1002")
	cache := entry("m-cache", "/cache", "b", "unix:uid:1003")
	manual := entry("other", "/manual", "a", "unix:uid:1004")
	// db moved to another host; cache is gone; queue is new.
	dbMoved := entry("m-db", "/db", "b", "unix:uid:1002")
	queue := entry("m-queue", "/queue", "b", "unix:uid:1005", "unix:gid:1005")

	f := server(t, web, db, cache, manual)
	res, err := Reconcile(t.Context(), f, "m-", []Entry{web, dbMoved, queue})
	if err != nil {
		t.Fatal(err)
	}
	want := Result{Created: []string{"m-queue"}, Updated: []string{"m-db"}, Deleted: []string{"m-cache"}}
	if !slices.Equal(res.Created, want.Created) || !slices.Equal(res.Updated, want.Updated) || !slices.Equal(res.Deleted, want.Deleted) {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if got, want := f.calls, []string{"delete m-cache", "update m-db", "create m-queue"}; !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if got, want := slices.Sorted(maps.Keys(f.entries)), []string{"m-db", "m-queue", "m-web", "other"}; !slices.Equal(got, want) {
		t.Errorf("entries = %q, want %q", got, want)
	}
	if got := f.entries["m-db"].ParentId.Path; got != "/spire/agent/x509pop/b" {
		t.Errorf("m-db parent = %q, want b's agent", got)
	}

	// Reconciling again changes nothing, whatever the selectors' order.
	f.calls = nil
	queue.Selectors = []string{"unix:gid:1005", "unix:uid:1005"}
	if _, err := Reconcile(t.Context(), f, "m-", []Entry{web, dbMoved, queue}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) > 0 {
		t.Errorf("second reconcile made calls %q", f.calls)
	}
}

func TestReconcileEmptyDeletesOwned(t *testing.T) {
	f := server(t, entry("m-web", "/web", "a", "unix:uid:1001"), entry("other", "/manual", "a", "unix:uid:1004"))
	if _, err := Reconcile(t.Context(), f, "m-", nil); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(f.entries)); !slices.Equal(got, []string{"other"}) {
		t.Errorf("entries = %q, want only the unowned one", got)
	}
}

func TestReconcileReportsEachFailure(t *testing.T) {
	f := server(t)
	f.fail = map[string]bool{"m-bad": true}
	res, err := Reconcile(t.Context(), f, "m-", []Entry{
		entry("m-bad", "/bad", "a", "unix:uid:1001"),
		entry("m-good", "/good", "a", "unix:uid:1002"),
	})
	if err == nil || !strings.Contains(err.Error(), `creating entry "m-bad": InvalidArgument: rejected`) {
		t.Errorf("err = %v, want m-bad's failure", err)
	}
	if !slices.Equal(res.Created, []string{"m-good"}) {
		t.Errorf("created = %q, want m-good despite m-bad", res.Created)
	}
}

func TestLoad(t *testing.T) {
	es, err := Load(strings.NewReader(`[{"id": "m-web", "spiffeID": "spiffe://example.org/web",
		"parentID": "spiffe://example.org/spire/agent/x509pop/a", "selectors": ["systemd:id:user@1001.service"]}]`), "m-")
	if err != nil {
		t.Fatal(err)
	}
	a, err := es[0].toAPI()
	if err != nil {
		t.Fatal(err)
	}
	if s := a.Selectors[0]; s.Type != "systemd" || s.Value != "id:user@1001.service" {
		t.Errorf("selector = %v, want type systemd, value id:user@1001.service", s)
	}

	for name, tc := range map[string]struct{ json, err string }{
		"prefix":   {`[{"id": "web", "spiffeID": "spiffe://example.org/web", "parentID": "spiffe://example.org/a", "selectors": ["unix:uid:1"]}]`, "must start with"},
		"bare":     {`[{"id": "m-", "spiffeID": "spiffe://example.org/web", "parentID": "spiffe://example.org/a", "selectors": ["unix:uid:1"]}]`, "must start with"},
		"twice":    {`[{"id": "m-web", "spiffeID": "spiffe://example.org/web", "parentID": "spiffe://example.org/a", "selectors": ["unix:uid:1"]}, {"id": "m-web", "spiffeID": "spiffe://example.org/web2", "parentID": "spiffe://example.org/a", "selectors": ["unix:uid:2"]}]`, "declared twice"},
		"spiffeID": {`[{"id": "m-web", "spiffeID": "https://example.org/web", "parentID": "spiffe://example.org/a", "selectors": ["unix:uid:1"]}]`, "spiffeID"},
		"parentID": {`[{"id": "m-web", "spiffeID": "spiffe://example.org/web", "parentID": "", "selectors": ["unix:uid:1"]}]`, "parentID"},
		"selector": {`[{"id": "m-web", "spiffeID": "spiffe://example.org/web", "parentID": "spiffe://example.org/a", "selectors": ["uid"]}]`, "isn't type:value"},
		"none":     {`[{"id": "m-web", "spiffeID": "spiffe://example.org/web", "parentID": "spiffe://example.org/a", "selectors": []}]`, "no selectors"},
		"unknown":  {`[{"id": "m-web", "ttl": 60}]`, "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tc.json), "m-")
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("err = %v, want one containing %q", err, tc.err)
			}
		})
	}
}
