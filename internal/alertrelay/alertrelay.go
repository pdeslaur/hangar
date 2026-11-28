// Package alertrelay takes a rule engine's alerts, through the endpoint of
// Alertmanager's API that engines like vmalert and Prometheus post them to,
// and sends each one as a message once when it fires and once when it
// resolves: an engine sends a firing alert again at every evaluation.
package alertrelay

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chainguard-dev/clog"
)

// maxAlerts bounds a request of alerts.
const maxAlerts = 1 << 20

// Alert is one of the alerts Alertmanager's API takes.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	// EndsAt is when the alert resolved, if it's past, or when it stops
	// firing unless the engine sends it again.
	EndsAt       time.Time `json:"endsAt"`
	GeneratorURL string    `json:"generatorURL"`
}

// Relay sends the alerts it's posted through Send, remembering what's firing
// in Store.
type Relay struct {
	Store *Store
	// Send sends a message's plain text.
	Send func(ctx context.Context, body string) error
	// Now is the current time; time.Now when nil.
	Now func() time.Time

	mu sync.Mutex
}

func (r *Relay) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// ServeHTTP takes a POST of alerts. It answers 502 when sending one fails,
// so that the engine, sending them again at its next evaluation, retries:
// an alert is recorded only once its message is sent.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Browsers send these on POSTs, and rule engines don't: refusing them
	// keeps web pages, cross-origin or through DNS rebinding, from posting.
	if req.Header.Get("Origin") != "" || req.Header.Get("Sec-Fetch-Site") != "" {
		http.Error(w, "browsers can't post alerts", http.StatusForbidden)
		return
	}
	var alerts []Alert
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxAlerts)).Decode(&alerts); err != nil {
		http.Error(w, "invalid alerts: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range alerts {
		if err := r.handle(ctx, a); err != nil {
			clog.ErrorContext(ctx, "relaying an alert", "alertname", a.Labels["alertname"], "error", err)
			http.Error(w, "relaying failed", http.StatusBadGateway)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// handle sends a message for an alert that's newly firing or newly
// resolved, and records it; a repeat only moves a firing alert's end.
func (r *Relay) handle(ctx context.Context, a Alert) error {
	labels, err := json.Marshal(a.Labels) // keys sorted, so the same labels give the same key
	if err != nil {
		return err
	}
	annotations, err := json.Marshal(a.Annotations)
	if err != nil {
		return err
	}
	firing, err := r.Store.Firing(ctx, string(labels))
	if err != nil {
		return err
	}
	resolved := !a.EndsAt.IsZero() && !a.EndsAt.After(r.now())
	switch {
	case resolved && !firing:
		return nil
	case resolved:
		if notifyResolved(a.Annotations) {
			if err := r.Send(ctx, format("RESOLVED", a.Labels, a.Annotations)); err != nil {
				return err
			}
		}
		return r.Store.Resolve(ctx, string(labels))
	case !firing:
		if err := r.Send(ctx, format("FIRING", a.Labels, a.Annotations)); err != nil {
			return err
		}
	}
	return r.Store.Fire(ctx, string(labels), string(annotations), a.EndsAt)
}

// Sweep resolves the alerts whose end passed without the engine sending
// them again, or sending their resolution: it was down, or this relay was.
// It says so, as it can't tell whether they resolved.
func (r *Relay) Sweep(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	expired, err := r.Store.Expired(ctx, r.now())
	if err != nil {
		return err
	}
	for _, s := range expired {
		var labels, annotations map[string]string
		if err := json.Unmarshal([]byte(s.Labels), &labels); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(s.Annotations), &annotations); err != nil {
			return err
		}
		if notifyResolved(annotations) {
			status := "RESOLVED (no update since " + s.EndsAt.UTC().Format(time.RFC3339) + ")"
			if err := r.Send(ctx, format(status, labels, annotations)); err != nil {
				return err
			}
		}
		if err := r.Store.Resolve(ctx, s.Labels); err != nil {
			return err
		}
	}
	return nil
}

// notifyResolved reports whether an alert's resolution gets a message: all
// but those annotated notify_resolved: "false", such as a rule counting
// one-off events, whose alert resolves once they age out of its window.
func notifyResolved(annotations map[string]string) bool {
	return annotations["notify_resolved"] != "false"
}

// format returns an alert's message: its status and name, then its summary,
// or its other labels without one, and its description on the lines after.
func format(status string, labels, annotations map[string]string) string {
	msg := status + " " + cmp.Or(labels["alertname"], "alert")
	if summary := annotations["summary"]; summary != "" {
		msg += ": " + summary
	} else {
		var pairs []string
		for _, k := range slices.Sorted(maps.Keys(labels)) {
			if k != "alertname" && k != "alertgroup" {
				pairs = append(pairs, k+"="+labels[k])
			}
		}
		if len(pairs) > 0 {
			msg += " {" + strings.Join(pairs, ", ") + "}"
		}
	}
	if description := annotations["description"]; description != "" {
		msg += "\n" + description
	}
	return msg
}

// Sender posts messages to a room through matrix-relay's /send, as notices.
type Sender struct {
	URL  string
	Room string
	HTTP *http.Client
}

// Send posts body.
func (s *Sender) Send(ctx context.Context, body string) error {
	msg, err := json.Marshal(map[string]any{"room": s.Room, "body": body, "notice": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s answered %s: %s", s.URL, resp.Status, bytes.TrimSpace(detail))
	}
	return nil
}
