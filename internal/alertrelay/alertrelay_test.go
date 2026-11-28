package alertrelay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

// fixture is a relay over a database in a test's directory, recording what
// it sends, at a time the test sets.
type fixture struct {
	t     *testing.T
	path  string
	relay *Relay
	now   time.Time
	sent  []string
	fail  bool
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "alerts.db"), now: t0}
	f.open()
	return f
}

// open opens the database at f.path, as a restart would.
func (f *fixture) open() {
	f.t.Helper()
	store, err := OpenStore(f.t.Context(), f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { store.Close() })
	f.relay = &Relay{
		Store: store,
		Send: func(_ context.Context, body string) error {
			if f.fail {
				return errors.New("matrix-relay is down")
			}
			f.sent = append(f.sent, body)
			return nil
		},
		Now: func() time.Time { return f.now },
	}
}

// post posts body to the relay, returning the response's status.
func (f *fixture) post(body string) int {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.relay.ServeHTTP(rec, req)
	return rec.Code
}

// postAlerts posts alerts, failing the test unless the relay answers 200.
func (f *fixture) postAlerts(alerts ...Alert) {
	f.t.Helper()
	body, err := json.Marshal(alerts)
	if err != nil {
		f.t.Fatal(err)
	}
	if code := f.post(string(body)); code != http.StatusOK {
		f.t.Fatalf("posting alerts answered %d, want 200", code)
	}
}

// takeSent returns the messages sent since the last call.
func (f *fixture) takeSent() []string {
	sent := f.sent
	f.sent = nil
	return sent
}

func (f *fixture) wantSent(want ...string) {
	f.t.Helper()
	if got := f.takeSent(); !slices.Equal(got, want) {
		f.t.Errorf("sent %q, want %q", got, want)
	}
}

// firing returns an alert firing at f.now, as vmalert sends it: ending
// four evaluations later unless sent again.
func (f *fixture) firing(name string, annotations map[string]string) Alert {
	return Alert{
		Labels:      map[string]string{"alertname": name, "host": "web01"},
		Annotations: annotations,
		StartsAt:    t0,
		EndsAt:      f.now.Add(4 * time.Minute),
	}
}

// resolved returns a firing alert resolved at f.now.
func (f *fixture) resolved(a Alert) Alert {
	a.EndsAt = f.now
	return a
}

func TestFiresOnceAndResolvesOnce(t *testing.T) {
	f := newFixture(t)
	a := f.firing("HostLogsSilent", map[string]string{"summary": "web01 sent no logs for 15 minutes"})

	f.postAlerts(a)
	f.wantSent("FIRING HostLogsSilent: web01 sent no logs for 15 minutes")

	f.now = f.now.Add(time.Minute)
	f.postAlerts(f.firing("HostLogsSilent", a.Annotations))
	f.wantSent()

	f.now = f.now.Add(time.Minute)
	f.postAlerts(f.resolved(a))
	f.wantSent("RESOLVED HostLogsSilent: web01 sent no logs for 15 minutes")

	// vmalert sends a resolution more than once.
	f.postAlerts(f.resolved(a))
	f.wantSent()

	// Firing again is news again.
	f.postAlerts(f.firing("HostLogsSilent", a.Annotations))
	f.wantSent("FIRING HostLogsSilent: web01 sent no logs for 15 minutes")
}

func TestResolvedNeverFiring(t *testing.T) {
	f := newFixture(t)
	f.postAlerts(f.resolved(f.firing("HostLogsSilent", nil)))
	f.wantSent()
}

func TestNotifyResolvedFalse(t *testing.T) {
	f := newFixture(t)
	a := f.firing("SSHWithoutSecurityKey", map[string]string{"notify_resolved": "false"})
	f.postAlerts(a)
	f.wantSent("FIRING SSHWithoutSecurityKey {host=web01}")
	f.postAlerts(f.resolved(a))
	f.wantSent()
	if firing, err := f.relay.Store.Firing(t.Context(), `{"alertname":"SSHWithoutSecurityKey","host":"web01"}`); err != nil || firing {
		t.Errorf("firing %v (%v) after resolving, want forgotten", firing, err)
	}
}

func TestFailedSendIsRetried(t *testing.T) {
	f := newFixture(t)
	a := f.firing("HostLogsSilent", nil)

	f.fail = true
	if code := f.post(mustJSON(t, []Alert{a})); code != http.StatusBadGateway {
		t.Fatalf("answered %d while sending fails, want 502", code)
	}
	f.fail = false
	f.postAlerts(a)
	f.wantSent("FIRING HostLogsSilent {host=web01}")

	f.fail = true
	if code := f.post(mustJSON(t, []Alert{f.resolved(a)})); code != http.StatusBadGateway {
		t.Fatalf("answered %d while sending fails, want 502", code)
	}
	f.fail = false
	f.postAlerts(f.resolved(a))
	f.wantSent("RESOLVED HostLogsSilent {host=web01}")
}

func TestSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	a := f.firing("HostLogsSilent", nil)
	f.postAlerts(a)
	f.takeSent()

	f.open()
	f.now = f.now.Add(time.Minute)
	f.postAlerts(f.firing("HostLogsSilent", nil))
	f.wantSent()
	f.postAlerts(f.resolved(a))
	f.wantSent("RESOLVED HostLogsSilent {host=web01}")
}

func TestSameLabelsInAnyOrder(t *testing.T) {
	f := newFixture(t)
	end := t0.Add(4 * time.Minute).Format(time.RFC3339)
	if code := f.post(`[{"labels":{"alertname":"A","host":"web01"},"endsAt":"` + end + `"}]`); code != http.StatusOK {
		t.Fatalf("answered %d", code)
	}
	if code := f.post(`[{"labels":{"host":"web01","alertname":"A"},"endsAt":"` + end + `"}]`); code != http.StatusOK {
		t.Fatalf("answered %d", code)
	}
	f.wantSent("FIRING A {host=web01}")
}

func TestSweep(t *testing.T) {
	f := newFixture(t)
	silent := f.firing("HostLogsSilent", map[string]string{"summary": "web01 sent no logs"})
	event := f.firing("SSHWithoutSecurityKey", map[string]string{"notify_resolved": "false"})
	f.postAlerts(silent, event)
	f.takeSent()

	// Not yet expired.
	f.now = t0.Add(3 * time.Minute)
	if err := f.relay.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantSent()

	// Expired, while sending fails: kept for the next sweep.
	f.now = t0.Add(5 * time.Minute)
	f.fail = true
	if err := f.relay.Sweep(t.Context()); err == nil {
		t.Fatal("sweep succeeded while sending fails")
	}
	f.fail = false
	if err := f.relay.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantSent("RESOLVED (no update since 2026-11-28T12:04:00Z) HostLogsSilent: web01 sent no logs")

	if err := f.relay.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantSent()

	// Swept, firing again is news again.
	f.postAlerts(f.firing("HostLogsSilent", silent.Annotations))
	f.wantSent("FIRING HostLogsSilent: web01 sent no logs")
}

func TestRefusesRequests(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name   string
		method string
		header string
		body   string
		want   int
	}{
		{name: "GET", method: http.MethodGet, body: "[]", want: http.StatusMethodNotAllowed},
		{name: "browser Origin", method: http.MethodPost, header: "Origin", body: "[]", want: http.StatusForbidden},
		{name: "browser Sec-Fetch-Site", method: http.MethodPost, header: "Sec-Fetch-Site", body: "[]", want: http.StatusForbidden},
		{name: "not JSON", method: http.MethodPost, body: "alert!", want: http.StatusBadRequest},
		{name: "too big", method: http.MethodPost, body: `["` + strings.Repeat("a", maxAlerts) + `"]`, want: http.StatusBadRequest},
		{name: "empty", method: http.MethodPost, body: "[]", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v2/alerts", strings.NewReader(tc.body))
			if tc.header != "" {
				req.Header.Set(tc.header, "https://evil.example")
			}
			rec := httptest.NewRecorder()
			f.relay.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("answered %d, want %d", rec.Code, tc.want)
			}
		})
	}
	f.wantSent()
}

func TestFormat(t *testing.T) {
	for _, tc := range []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		want        string
	}{
		{
			name:        "summary",
			labels:      map[string]string{"alertname": "A", "host": "web01"},
			annotations: map[string]string{"summary": "something happened"},
			want:        "FIRING A: something happened",
		},
		{
			name:   "labels without a summary, the engine's left out",
			labels: map[string]string{"alertname": "A", "alertgroup": "ssh", "stats_result": "logins", "src": "192.0.2.10", "host": "web01"},
			want:   "FIRING A {host=web01, src=192.0.2.10}",
		},
		{
			name:        "description",
			labels:      map[string]string{"alertname": "A"},
			annotations: map[string]string{"summary": "s", "description": "line one\nline two"},
			want:        "FIRING A: s\nline one\nline two",
		},
		{
			name: "no name",
			want: "FIRING alert",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := format("FIRING", tc.labels, tc.annotations); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSender(t *testing.T) {
	var got map[string]any
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/send" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("got %s %s with Content-Type %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Error(err)
		}
		if status != http.StatusOK {
			http.Error(w, "not in the room", status)
			return
		}
		w.Write([]byte(`{"event_id":"$abc"}`))
	}))
	defer srv.Close()
	s := &Sender{URL: srv.URL + "/send", Room: "#alerts:example.org", HTTP: srv.Client()}

	if err := s.Send(t.Context(), "FIRING A"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"room": "#alerts:example.org", "body": "FIRING A", "notice": true}
	if mustJSON(t, got) != mustJSON(t, want) {
		t.Errorf("sent %v, want %v", got, want)
	}

	status = http.StatusForbidden
	if err := s.Send(t.Context(), "FIRING A"); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "not in the room") {
		t.Errorf("error %v, want the status and the answer", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
