package login

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const (
	testAudience = "tuwunel-svid-sts"
	bot          = "spiffe://example.org/bot"
)

// authority issues JWT-SVIDs for example.org, standing in for a SPIRE server.
type authority struct {
	key    *ecdsa.PrivateKey
	bundle *jwtbundle.Bundle
}

func newAuthority(t *testing.T) *authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := jwtbundle.New(spiffeid.RequireTrustDomainFromString("example.org"))
	if err := bundle.AddJWTAuthority("kid", key.Public()); err != nil {
		t.Fatal(err)
	}
	return &authority{key: key, bundle: bundle}
}

func (a *authority) svid(t *testing.T, sub, aud string, exp time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: a.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "kid"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Subject:  sub,
		Audience: jwt.Audience{aud},
		Expiry:   jwt.NewNumericDate(exp),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// fakeTuwunel checks JWT logins the way tuwunel is configured to, by the
// file TuwunelConfig writes, and answers with the user's ID and the device,
// "ASSIGNED" when the login names none.
func fakeTuwunel(t *testing.T, signer *Signer) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/_matrix/client/v3/login" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Type     string `json:"type"`
			Token    string `json:"token"`
			DeviceID string `json:"device_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding login request: %v", err)
		}
		if body.Type != "org.matrix.login.jwt" {
			t.Errorf("login request of type %q", body.Type)
		}
		if body.DeviceID == "" {
			body.DeviceID = "ASSIGNED"
		}
		token, err := jwt.ParseSigned(body.Token, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatalf("parsing JWT: %v", err)
		}
		var claims jwt.Claims
		if err := token.Claims(signer.Public(), &claims); err != nil {
			t.Fatalf("verifying JWT: %v", err)
		}
		if err := claims.Validate(jwt.Expected{Issuer: Issuer, AnyAudience: jwt.Audience{Audience}}); err != nil {
			t.Fatalf("validating JWT: %v", err)
		}
		if claims.Expiry == nil || claims.NotBefore == nil {
			t.Errorf("JWT lacks exp or nbf: %+v", claims)
		}
		json.NewEncoder(w).Encode(map[string]string{
			"user_id":      "@" + claims.Subject + ":example.org",
			"access_token": "token-" + claims.Subject,
			"device_id":    body.DeviceID,
		})
	}))
}

func TestHandler(t *testing.T) {
	ca := newAuthority(t)
	other := newAuthority(t)
	signer, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	matrix := fakeTuwunel(t, signer)
	defer matrix.Close()
	h := &Handler{
		Bundles:   ca.bundle,
		Audience:  testAudience,
		Users:     map[string]string{bot: "bot"},
		Signer:    signer,
		MatrixURL: matrix.URL,
		Client:    matrix.Client(),
	}
	later := time.Now().Add(5 * time.Minute)

	for _, tc := range []struct {
		name   string
		method string
		auth   string
		body   string
		status int
		user   string
		device string
	}{{
		name:   "mapped SPIFFE ID",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svid(t, bot, testAudience, later),
		status: http.StatusOK,
		user:   "@bot:example.org",
		device: "ASSIGNED",
	}, {
		name:   "device",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svid(t, bot, testAudience, later),
		body:   `{"device_id": "RELAY"}`,
		status: http.StatusOK,
		user:   "@bot:example.org",
		device: "RELAY",
	}, {
		name:   "invalid device",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svid(t, bot, testAudience, later),
		body:   `{"device_id": "RELAY\nlogged in"}`,
		status: http.StatusBadRequest,
	}, {
		name:   "unknown field",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svid(t, bot, testAudience, later),
		body:   `{"user": "admin"}`,
		status: http.StatusBadRequest,
	}, {
		name:   "body too large",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svid(t, bot, testAudience, later),
		body:   `{"device_id": "` + strings.Repeat("A", maxRequest) + `"}`,
		status: http.StatusBadRequest,
	}, {
		name:   "body of an unmapped SPIFFE ID",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svid(t, "spiffe://example.org/stranger", testAudience, later),
		body:   "not JSON",
		status: http.StatusForbidden,
	}, {
		name:   "unmapped SPIFFE ID",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svid(t, "spiffe://example.org/stranger", testAudience, later),
		status: http.StatusForbidden,
	}, {
		name:   "other audience",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svid(t, bot, "elsewhere", later),
		status: http.StatusUnauthorized,
	}, {
		name:   "expired",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svid(t, bot, testAudience, time.Now().Add(-time.Minute)),
		status: http.StatusUnauthorized,
	}, {
		name:   "untrusted signer",
		method: http.MethodPost,
		auth:   "Bearer " + other.svid(t, bot, testAudience, later),
		status: http.StatusUnauthorized,
	}, {
		name:   "no token",
		method: http.MethodPost,
		status: http.StatusUnauthorized,
	}, {
		name:   "GET",
		method: http.MethodGet,
		auth:   "Bearer " + ca.svid(t, bot, testAudience, later),
		status: http.StatusMethodNotAllowed,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.user == "" {
				return
			}
			var resp struct {
				UserID   string `json:"user_id"`
				DeviceID string `json:"device_id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.UserID != tc.user {
				t.Errorf("user %q, want %q", resp.UserID, tc.user)
			}
			if resp.DeviceID != tc.device {
				t.Errorf("device %q, want %q", resp.DeviceID, tc.device)
			}
		})
	}
}

func TestTuwunelConfig(t *testing.T) {
	signer, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	b, err := signer.TuwunelConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"[global.jwt]\n",
		"format = \"ECDSA\"\n",
		"algorithm = \"ES256\"\n",
		"key = \"\"\"\n-----BEGIN PUBLIC KEY-----\n",
		"-----END PUBLIC KEY-----\n\"\"\"\n",
		"issuer = [\"tuwunel-svid-sts\"]\n",
		"audience = [\"tuwunel\"]\n",
		"register_user = false\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config lacks %q:\n%s", want, got)
		}
	}
}
