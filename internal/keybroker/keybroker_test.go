package keybroker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

var (
	td       = spiffeid.RequireTrustDomainFromString("example.org")
	otherTD  = spiffeid.RequireTrustDomainFromString("other.org")
	brokerID = spiffeid.RequireFromString("spiffe://example.org/svc/spire-server")
	appID    = spiffeid.RequireFromString("spiffe://example.org/svc/app")
	master   = bytes.Repeat([]byte{0x42}, MasterSize)
)

// ca issues X509-SVIDs for a trust domain.
type ca struct {
	td   spiffeid.TrustDomain
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T, td spiffeid.TrustDomain) *ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: td.Name()},
		URIs:                  []*url.URL{td.ID().URL()},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &ca{td: td, cert: cert, key: key}
}

func (c *ca) bundle() *x509bundle.Bundle {
	return x509bundle.FromX509Authorities(c.td, []*x509.Certificate{c.cert})
}

func (c *ca) svid(t *testing.T, id spiffeid.ID) *x509svid.SVID {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		URIs:         []*url.URL{id.URL()},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, key.Public(), c.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &x509svid.SVID{ID: id, Certificates: []*x509.Certificate{cert}, PrivateKey: key}
}

// startBroker serves master's secrets as brokerID, signed by ca, and returns
// its URL.
func startBroker(t *testing.T, ca *ca) string {
	t.Helper()
	srv := NewServer("", master, ca.svid(t, brokerID), ca.bundle(), td)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	return "https://" + ln.Addr().String()
}

// client returns a client of the broker at url, as svid, trusting ca.
func client(url string, ca *ca, svid *x509svid.SVID) *Client {
	return &Client{
		URL: url,
		HTTP: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsconfig.MTLSClientConfig(svid, ca.bundle(), tlsconfig.AuthorizeID(brokerID)),
			},
		},
	}
}

func mustDerive(t *testing.T, id spiffeid.ID, name string, version, size int) []byte {
	t.Helper()
	key, err := Derive(master, id, name, version, size)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestDeriveIsStable(t *testing.T) {
	// Pins the derivation: a change here changes every service's secrets.
	// The value comes from OpenSSL's HKDF:
	//
	//	openssl kdf -keylen 32 -kdfopt digest:SHA256 \
	//	    -kdfopt hexkey:$(printf '42%.0s' {1..32}) -kdfopt salt:example.org \
	//	    -kdfopt hexinfo:$(printf 'spiffe://example.org/svc/app\0ENCRYPTION_KEY\0001' | xxd -p | tr -d '\n') \
	//	    HKDF
	got := hex.EncodeToString(mustDerive(t, appID, "ENCRYPTION_KEY", 1, 32))
	const want = "c5694d503d26b4cfa4c903901faf368110cf48a38f2c189beb5ab147fd75a148"
	if got != want {
		t.Errorf("Derive = %s, want %s", got, want)
	}
}

func TestDeriveSeparates(t *testing.T) {
	base := mustDerive(t, appID, "KEY", 1, 32)
	if again := mustDerive(t, appID, "KEY", 1, 32); !bytes.Equal(base, again) {
		t.Error("same inputs gave different keys")
	}
	for name, key := range map[string][]byte{
		"ID":      mustDerive(t, spiffeid.RequireFromString("spiffe://example.org/svc/other"), "KEY", 1, 32),
		"name":    mustDerive(t, appID, "KEY2", 1, 32),
		"version": mustDerive(t, appID, "KEY", 2, 32),
	} {
		if bytes.Equal(base, key) {
			t.Errorf("a different %s gave the same key", name)
		}
	}
	// HKDF's longer outputs start with its shorter ones: the size isn't in
	// info, so a service asking for more bytes learns only more of its own
	// secret.
	if long := mustDerive(t, appID, "KEY", 1, 64); !bytes.Equal(long[:32], base) {
		t.Error("64 bytes don't start with the 32")
	}
}

func TestDeriveRefuses(t *testing.T) {
	for _, tc := range []struct {
		name          string
		version, size int
	}{
		{"", 1, 32},
		{"a/../b", 1, 32},
		{"a//b", 1, 32},
		{"/a", 1, 32},
		{"a\x00b", 1, 32},
		{"a b", 1, 32},
		{"KEY", 0, 32},
		{"KEY", 1, MinBytes - 1},
		{"KEY", 1, MaxBytes + 1},
	} {
		if _, err := Derive(master, appID, tc.name, tc.version, tc.size); err == nil {
			t.Errorf("Derive(%q, %d, %d) succeeded", tc.name, tc.version, tc.size)
		}
	}
}

func TestLoadMaster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master")
	created, err := LoadMaster(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != MasterSize {
		t.Fatalf("created %d bytes", len(created))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %o, want 600", mode)
	}
	loaded, err := LoadMaster(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(created, loaded) {
		t.Error("loaded a different master")
	}

	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMaster(path); err == nil {
		t.Error("loaded a short master")
	}
	if got, _ := os.ReadFile(path); string(got) != "short" {
		t.Error("replaced a short master")
	}
}

func TestFetch(t *testing.T) {
	ca := newCA(t, td)
	url := startBroker(t, ca)
	c := client(url, ca, ca.svid(t, appID))
	ctx := context.Background()

	got, err := c.Fetch(ctx, Secret{Name: "KEY", Version: 1, Bytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	if want := mustDerive(t, appID, "KEY", 1, 32); !bytes.Equal(got, want) {
		t.Error("broker's key differs from Derive's")
	}

	other := client(url, ca, ca.svid(t, spiffeid.RequireFromString("spiffe://example.org/svc/other")))
	otherKey, err := other.Fetch(ctx, Secret{Name: "KEY", Version: 1, Bytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, otherKey) {
		t.Error("two services got the same key")
	}

	_, err = c.Fetch(ctx, Secret{Name: "KEY", Version: 0, Bytes: 32})
	var se *StatusError
	if !errors.As(err, &se) || !se.Permanent() {
		t.Errorf("version 0: got %v, want a permanent StatusError", err)
	}
}

func TestFetchRefusesClients(t *testing.T) {
	ca := newCA(t, td)
	url := startBroker(t, ca)
	ctx := context.Background()

	outside := client(url, ca, ca.svid(t, spiffeid.RequireFromString("spiffe://example.org/host/rasp01")))
	if _, err := outside.Fetch(ctx, Secret{Name: "KEY", Version: 1, Bytes: 32}); err == nil {
		t.Error("an ID outside /svc/ fetched a secret")
	}

	// A client of another trust domain, which trusts the broker's.
	foreign := newCA(t, otherTD)
	svid := foreign.svid(t, spiffeid.RequireFromString("spiffe://other.org/svc/app"))
	c := client(url, ca, svid)
	if _, err := c.Fetch(ctx, Secret{Name: "KEY", Version: 1, Bytes: 32}); err == nil {
		t.Error("another trust domain's ID fetched a secret")
	}

	plain := &http.Client{Transport: &http.Transport{
		TLSClientConfig: tlsconfig.TLSClientConfig(ca.bundle(), tlsconfig.AuthorizeID(brokerID)),
	}}
	if resp, err := plain.Get(url + "/v1/secrets/KEY"); err == nil {
		resp.Body.Close()
		t.Error("a client without a certificate fetched a secret")
	}
}

func TestRenderAndWrite(t *testing.T) {
	ca := newCA(t, td)
	c := client(startBroker(t, ca), ca, ca.svid(t, appID))
	files := Files{
		{Name: "KEY.env", Variable: "KEY", Parts: []Part{{Derive: &Secret{Name: "KEY"}}}},
		{Name: "PEPPER-HBOX_PEPPER.env", Variable: "HBOX_PEPPER", Parts: []Part{{Derive: &Secret{Name: "PEPPER", Bytes: 16, Encoding: "base64"}}}},
		{Name: "URL.env", Variable: "URL", Parts: []Part{
			{Text: "postgresql://app:"},
			{Derive: &Secret{Name: "KEY"}},
			{Text: "@127.0.0.1:5432/app"},
		}},
	}
	if err := files.Validate(); err != nil {
		t.Fatal(err)
	}
	contents, err := c.Render(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "STALE.env"), []byte("STALE=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFiles(dir, contents); err != nil {
		t.Fatal(err)
	}

	key := hex.EncodeToString(mustDerive(t, appID, "KEY", 1, 32))
	pepper := mustDerive(t, appID, "PEPPER", 1, 16)
	for name, want := range map[string]string{
		"KEY.env":                "KEY=" + key + "\n",
		"PEPPER-HBOX_PEPPER.env": "HBOX_PEPPER=" + base64Std(pepper) + "\n",
		"URL.env":                "URL=postgresql://app:" + key + "@127.0.0.1:5432/app\n",
	} {
		path := filepath.Join(dir, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s: mode %o, want 600", name, mode)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "STALE.env")); !errors.Is(err, os.ErrNotExist) {
		t.Error("left a file no longer listed")
	}
}

func base64Std(b []byte) string {
	return encode(b, "base64")
}

func TestValidateRefuses(t *testing.T) {
	key := []Part{{Derive: &Secret{Name: "KEY"}}}
	for name, files := range map[string]Files{
		"path in name":    {{Name: "../KEY.env", Variable: "KEY", Parts: key}},
		"not .env":        {{Name: "KEY", Variable: "KEY", Parts: key}},
		"listed twice":    {{Name: "KEY.env", Variable: "KEY", Parts: key}, {Name: "KEY.env", Variable: "KEY", Parts: key}},
		"bad variable":    {{Name: "KEY.env", Variable: "1KEY", Parts: key}},
		"empty part":      {{Name: "KEY.env", Variable: "KEY", Parts: []Part{{}}}},
		"text and secret": {{Name: "KEY.env", Variable: "KEY", Parts: []Part{{Text: "x", Derive: &Secret{Name: "KEY"}}}}},
		"newline":         {{Name: "KEY.env", Variable: "KEY", Parts: []Part{{Text: "a\nB=c"}}}},
		"encoding":        {{Name: "KEY.env", Variable: "KEY", Parts: []Part{{Derive: &Secret{Name: "KEY", Encoding: "base32"}}}}},
	} {
		if err := files.Validate(); err == nil {
			t.Errorf("%s: Validate succeeded", name)
		}
	}
}
