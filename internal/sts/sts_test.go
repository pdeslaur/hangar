package sts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	mxid "maunium.net/go/mautrix/id"
)

const audience = "tuwunel-svid-sts"

// svids hands out a JWT-SVID for the audience it's asked, or err.
type svids struct{ err error }

func (s svids) FetchJWTSVID(_ context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error) {
	if s.err != nil {
		return nil, s.err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, nil)
	if err != nil {
		return nil, err
	}
	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Subject:  "spiffe://example.org/bot",
		Audience: jwt.Audience{params.Audience},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}).Serialize()
	if err != nil {
		return nil, err
	}
	return jwtsvid.ParseInsecure(token, []string{params.Audience})
}

func TestLogin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		svids  svids
		device string
		// answer is the STS's answer to a request on device.
		answer func(w http.ResponseWriter, device string)
		want   Session
		err    string
	}{{
		name: "assigned device",
		answer: func(w http.ResponseWriter, device string) {
			if device != "" {
				http.Error(w, "unexpected device "+device, http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"user_id": "@bot:example.org", "access_token": "token", "device_id": "ASSIGNED", "expires_in_ms": 60000}`))
		},
		want: Session{UserID: "@bot:example.org", AccessToken: "token", DeviceID: "ASSIGNED", ExpiresInMS: 60000},
	}, {
		name:   "device",
		device: "RELAY",
		answer: func(w http.ResponseWriter, device string) {
			w.Write([]byte(`{"user_id": "@bot:example.org", "access_token": "token", "device_id": "` + device + `"}`))
		},
		want: Session{UserID: "@bot:example.org", AccessToken: "token", DeviceID: "RELAY"},
	}, {
		name:   "other device",
		device: "RELAY",
		answer: func(w http.ResponseWriter, _ string) {
			w.Write([]byte(`{"user_id": "@bot:example.org", "access_token": "token", "device_id": "OTHER"}`))
		},
		err: "logged in on OTHER, not RELAY",
	}, {
		name: "refused",
		answer: func(w http.ResponseWriter, _ string) {
			http.Error(w, "forbidden", http.StatusForbidden)
		},
		err: "403 Forbidden: forbidden",
	}, {
		name: "Matrix error",
		answer: func(w http.ResponseWriter, _ string) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errcode": "M_NOT_FOUND"}`))
		},
		err: "M_NOT_FOUND",
	}, {
		name: "incomplete session",
		answer: func(w http.ResponseWriter, _ string) {
			w.Write([]byte(`{"user_id": "@bot:example.org", "device_id": "ASSIGNED"}`))
		},
		err: "incomplete session",
	}, {
		name:  "no SVID",
		svids: svids{err: errors.New("no identity issued")},
		answer: func(w http.ResponseWriter, _ string) {
			t.Error("called without an SVID")
		},
		err: "no identity issued",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svid, err := jwtsvid.ParseInsecure(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), []string{audience})
				if err != nil {
					t.Errorf("JWT-SVID for the audience %s: %v", audience, err)
				} else if svid.ID.String() != "spiffe://example.org/bot" {
					t.Errorf("JWT-SVID of %s", svid.ID)
				}
				var body struct {
					DeviceID string `json:"device_id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decoding request: %v", err)
				}
				tc.answer(w, body.DeviceID)
			}))
			defer sts.Close()
			c := &Client{URL: sts.URL, Audience: audience, SVIDs: tc.svids, HTTP: sts.Client()}

			got, err := c.Login(t.Context(), mxid.DeviceID(tc.device))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestUIA(t *testing.T) {
	for _, tc := range []struct {
		name   string
		url    string
		answer string
		status int
		want   UIA
		err    string
	}{{
		name:   "UIA",
		answer: `{"type": "org.matrix.login.jwt", "token": "jwt"}`,
		want:   UIA{Type: "org.matrix.login.jwt", Token: "jwt"},
	}, {
		name:   "URL with a slash",
		url:    "/",
		answer: `{"type": "org.matrix.login.jwt", "token": "jwt"}`,
		want:   UIA{Type: "org.matrix.login.jwt", Token: "jwt"},
	}, {
		name:   "refused",
		answer: "forbidden",
		status: http.StatusForbidden,
		err:    "403 Forbidden: forbidden",
	}, {
		name:   "incomplete",
		answer: `{"type": "org.matrix.login.jwt"}`,
		err:    "incomplete UIA",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/uia" {
					t.Errorf("%s %s, want POST /uia", r.Method, r.URL.Path)
				}
				if _, err := jwtsvid.ParseInsecure(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), []string{audience}); err != nil {
					t.Errorf("JWT-SVID for the audience %s: %v", audience, err)
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				w.Write([]byte(tc.answer))
			}))
			defer sts.Close()
			c := &Client{URL: sts.URL + tc.url, Audience: audience, SVIDs: svids{}, HTTP: sts.Client()}

			got, err := c.UIA(t.Context())
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
