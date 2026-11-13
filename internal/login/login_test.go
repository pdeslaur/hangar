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
	return a.svidFor(t, sub, jwt.Audience{aud}, exp)
}

// svidFor is svid for audiences.
func (a *authority) svidFor(t *testing.T, sub string, aud jwt.Audience, exp time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: a.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "kid"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Subject:  sub,
		Audience: aud,
		Expiry:   jwt.NewNumericDate(exp),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// fakeTuwunel checks JWTs the way tuwunel is configured to, by the file
// TuwunelConfig writes. It answers logins with the user's ID, an access token
// "token-" and the user's localpart, and the device, "ASSIGNED" when the
// login names none; whoami for those access tokens; and uploads of
// cross-signing keys with a JWT as auth, sending their requests to uploads.
func fakeTuwunel(t *testing.T, signer *Signer, uploads chan<- map[string]any) *httptest.Server {
	t.Helper()
	verify := func(token string) jwt.Claims {
		t.Helper()
		parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatalf("parsing JWT: %v", err)
		}
		var claims jwt.Claims
		if err := parsed.Claims(signer.Public(), &claims); err != nil {
			t.Fatalf("verifying JWT: %v", err)
		}
		if err := claims.Validate(jwt.Expected{Issuer: Issuer, AnyAudience: jwt.Audience{Audience}}); err != nil {
			t.Fatalf("validating JWT: %v", err)
		}
		if claims.Expiry == nil || claims.NotBefore == nil {
			t.Errorf("JWT lacks exp or nbf: %+v", claims)
		}
		return claims
	}
	user := func(r *http.Request) (string, bool) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		localpart, ok := strings.CutPrefix(token, "token-")
		return "@" + localpart + ":example.org", ok
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_matrix/client/v3/login", func(w http.ResponseWriter, r *http.Request) {
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
		claims := verify(body.Token)
		json.NewEncoder(w).Encode(map[string]string{
			"user_id":      "@" + claims.Subject + ":example.org",
			"access_token": "token-" + claims.Subject,
			"device_id":    body.DeviceID,
		})
	})
	mux.HandleFunc("GET /_matrix/client/v3/account/whoami", func(w http.ResponseWriter, r *http.Request) {
		user, ok := user(r)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"errcode": "M_UNKNOWN_TOKEN"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"user_id": user})
	})
	mux.HandleFunc("POST /_matrix/client/v3/keys/device_signing/upload", func(w http.ResponseWriter, r *http.Request) {
		user, ok := user(r)
		if !ok {
			t.Errorf("upload with authorization %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding upload request: %v", err)
		}
		auth, _ := body["auth"].(map[string]any)
		if auth["type"] != "org.matrix.login.jwt" {
			t.Errorf("upload with auth %v", auth)
		}
		token, _ := auth["token"].(string)
		if claims := verify(token); "@"+claims.Subject+":example.org" != user {
			t.Errorf("upload as %s with auth for %s", user, claims.Subject)
		}
		uploads <- body
		json.NewEncoder(w).Encode(map[string]string{})
	})
	return httptest.NewServer(mux)
}

func TestHandler(t *testing.T) {
	ca := newAuthority(t)
	other := newAuthority(t)
	signer, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	matrix := fakeTuwunel(t, signer, nil)
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
		name:   "other audiences too",
		method: http.MethodPost,
		auth:   "Bearer " + ca.svidFor(t, bot, jwt.Audience{testAudience, "elsewhere"}, later),
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

func TestCrossSigningKeys(t *testing.T) {
	ca := newAuthority(t)
	signer, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	uploads := make(chan map[string]any, 1)
	matrix := fakeTuwunel(t, signer, uploads)
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
	const keys = `"master_key": {"usage": ["master"]}, "self_signing_key": {"usage": ["self_signing"]}, "user_signing_key": {"usage": ["user_signing"]}`

	for _, tc := range []struct {
		name   string
		sub    string
		body   string
		status int
	}{{
		name:   "own access token",
		sub:    bot,
		body:   `{"access_token": "token-bot", ` + keys + `}`,
		status: http.StatusOK,
	}, {
		name:   "another user's access token",
		sub:    bot,
		body:   `{"access_token": "token-phil", ` + keys + `}`,
		status: http.StatusForbidden,
	}, {
		name:   "invalid access token",
		sub:    bot,
		body:   `{"access_token": "stolen", ` + keys + `}`,
		status: http.StatusUnauthorized,
	}, {
		name:   "no access token",
		sub:    bot,
		body:   `{` + keys + `}`,
		status: http.StatusBadRequest,
	}, {
		name:   "no master key",
		sub:    bot,
		body:   `{"access_token": "token-bot", "self_signing_key": {}}`,
		status: http.StatusBadRequest,
	}, {
		name:   "auth of its own",
		sub:    bot,
		body:   `{"access_token": "token-bot", "auth": {"type": "m.login.dummy"}, ` + keys + `}`,
		status: http.StatusBadRequest,
	}, {
		name:   "body too large",
		sub:    bot,
		body:   `{"access_token": "token-bot", "master_key": "` + strings.Repeat("A", maxCrossSigningKeys) + `"}`,
		status: http.StatusBadRequest,
	}, {
		name:   "unmapped SPIFFE ID",
		sub:    "spiffe://example.org/stranger",
		body:   `{"access_token": "token-stranger", ` + keys + `}`,
		status: http.StatusForbidden,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, CrossSigningKeysPath, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+ca.svid(t, tc.sub, testAudience, later))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.status != http.StatusOK {
				select {
				case upload := <-uploads:
					t.Errorf("uploaded %v", upload)
				default:
				}
				return
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control %q, want no-store", got)
			}
			upload := <-uploads
			for key, usage := range map[string]string{"master_key": "master", "self_signing_key": "self_signing", "user_signing_key": "user_signing"} {
				got, _ := upload[key].(map[string]any)
				if usages, _ := got["usage"].([]any); len(usages) != 1 || usages[0] != usage {
					t.Errorf("uploaded %s %v", key, upload[key])
				}
			}
			if strings.Contains(rec.Body.String(), "token") {
				t.Errorf("answered %s", rec.Body)
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/other", nil)
	req.Header.Set("Authorization", "Bearer "+ca.svid(t, bot, testAudience, later))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /other: status %d, want %d", rec.Code, http.StatusNotFound)
	}
}
