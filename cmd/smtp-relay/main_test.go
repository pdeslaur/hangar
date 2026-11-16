package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
	"github.com/pdeslaur/hangar/cloudflarests"
	"github.com/pdeslaur/hangar/internal/workloadtest"
	"github.com/sethvargo/go-envconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const (
	id      = "spiffe://example.org/svc/app"
	profile = id + ":email"
	message = "From: app@example.org\r\nTo: you@example.com\r\nSubject: Hi\r\n\r\nHello\r\n"
)

// relay serves the relay in front of broker, with JWT-SVIDs from api, and
// returns its address.
func relay(t *testing.T, broker http.Handler, api *workloadtest.API) string {
	t.Helper()
	srv := httptest.NewServer(broker)
	t.Cleanup(srv.Close)
	svids, err := workloadapi.New(t.Context(), workloadapi.WithAddr(api.Addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svids.Close() })
	client := &cloudflarests.Client{URL: srv.URL, SVIDs: svids, HTTP: cloudflarests.NewHTTPClient()}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(t.Context(), client, profile)
	go s.Serve(lis)
	t.Cleanup(func() { s.Close() })
	return lis.Addr().String()
}

func TestSend(t *testing.T) {
	var to []string
	addr := relay(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/email" || q.Get("profile") != profile || q.Get("from") != "app@example.org" {
			t.Errorf("%s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("Authorization %q, want a bearer token", r.Header.Get("Authorization"))
		}
		if body, _ := io.ReadAll(r.Body); string(body) != message {
			t.Errorf("body %q, want %q", body, message)
		}
		to = q["to"]
		io.WriteString(w, `{"message_ids": ["<1@example.org>", "<2@example.org>"]}`)
	}), workloadtest.Serve(t, id))

	// them@example.com is in the envelope alone, as a Bcc is.
	err := send(t, addr, []string{"you@example.com", "them@example.com"}, strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"you@example.com", "them@example.com"}; !slices.Equal(to, want) {
		t.Errorf("sent to %q, want %q", to, want)
	}
}

func TestSendFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		unready bool
		code    int
	}{{
		name:   "refused",
		status: http.StatusForbidden,
		body:   `{"error": "insufficient_scope", "error_description": "not allowed"}`,
		code:   554,
	}, {
		name:   "unavailable",
		status: http.StatusServiceUnavailable,
		body:   `{"error": "temporarily_unavailable", "error_description": "Cloudflare: down"}`,
		code:   451,
	}, {
		name:    "no SVID",
		unready: true,
		code:    451,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			api := workloadtest.Serve(t, id)
			if tc.unready {
				api.Unready(1)
			}
			addr := relay(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.unready {
					t.Error("called the broker without a JWT-SVID")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}), api)

			err := send(t, addr, []string{"you@example.com"}, strings.NewReader(message))
			var e *smtp.SMTPError
			if !errors.As(err, &e) || e.Code != tc.code {
				t.Errorf("error %v, want a %d", err, tc.code)
			}
		})
	}
}

func TestConfigAddrDefaultsToLoopback(t *testing.T) {
	var cfg config
	err := envconfig.ProcessWith(t.Context(), &envconfig.Config{
		Target: &cfg,
		Lookuper: envconfig.MapLookuper(map[string]string{
			"BROKER_URL": "https://cloudflare-sts.example.com",
			"PROFILE":    profile,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "127.0.0.1:2525"; cfg.Addr != want {
		t.Errorf("addr %q, want %q", cfg.Addr, want)
	}
}

// send sends message to the relay at addr, in the clear, as an app in its pod
// does.
func send(t *testing.T, addr string, to []string, message io.Reader) error {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.SendMail("app@example.org", to, message)
}

func TestClosesOnHTTP(t *testing.T) {
	addr := relay(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("sent a message from an HTTP request's body")
	}), workloadtest.Serve(t, id))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// A browser's request, its body SMTP commands.
	body := "EHLO example.com\r\nMAIL FROM:<app@example.org>\r\nRCPT TO:<you@example.com>\r\nDATA\r\n" + message + ".\r\nQUIT\r\n"
	io.WriteString(conn, "POST / HTTP/1.1\r\n"+
		"Host: 127.0.0.1:2525\r\n"+
		"User-Agent: Mozilla/5.0\r\n"+
		"Origin: https://example.com\r\n"+
		"Content-Type: text/plain\r\n"+
		"Content-Length: "+strconv.Itoa(len(body))+"\r\n"+
		"\r\n"+body)
	replies, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(replies), "\r\n250 ") {
		t.Errorf("accepted the body's commands:\n%s", replies)
	}
}
