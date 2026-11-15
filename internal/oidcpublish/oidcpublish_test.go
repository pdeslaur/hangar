package oidcpublish

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

var td = spiffeid.RequireTrustDomainFromString("example.com")

func ecKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func bundleOf(t *testing.T, keys map[string]crypto.PublicKey) *jwtbundle.Bundle {
	t.Helper()
	b := jwtbundle.New(td)
	for kid, key := range keys {
		if err := b.AddJWTAuthority(kid, key); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func TestDocuments(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	bundle := bundleOf(t, map[string]crypto.PublicKey{
		"b-p256": ecKey(t, elliptic.P256()).Public(),
		"a-rsa":  rsaKey.Public(),
		"c-p384": ecKey(t, elliptic.P384()).Public(),
	})

	discovery, jwks, err := Documents("https://oidc.example.com", "jwks", bundle)
	if err != nil {
		t.Fatal(err)
	}

	var set jose.JSONWebKeySet
	if err := json.Unmarshal(jwks, &set); err != nil {
		t.Fatal(err)
	}
	var got [][3]string
	for _, k := range set.Keys {
		got = append(got, [3]string{k.KeyID, k.Use, k.Algorithm})
	}
	want := [][3]string{{"a-rsa", "sig", "RS256"}, {"b-p256", "sig", "ES256"}, {"c-p384", "sig", "ES384"}}
	if !slices.Equal(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}

	var doc struct {
		Issuer  string   `json:"issuer"`
		JWKSURI string   `json:"jwks_uri"`
		Algs    []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := json.Unmarshal(discovery, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Issuer != "https://oidc.example.com" || doc.JWKSURI != "https://oidc.example.com/jwks" {
		t.Errorf("issuer, jwks_uri = %q, %q", doc.Issuer, doc.JWKSURI)
	}
	if want := []string{"ES256", "ES384", "RS256"}; !slices.Equal(doc.Algs, want) {
		t.Errorf("algorithms = %v, want %v", doc.Algs, want)
	}

	// The same bundle gives the same bytes, whatever the map's order.
	again, jwksAgain, err := Documents("https://oidc.example.com", "jwks", bundle)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(discovery) || string(jwksAgain) != string(jwks) {
		t.Error("documents differ for the same bundle")
	}
}

// TestDocumentsVerify checks that a token signed by a bundle key verifies with
// the published JWKS alone, as a relying party would.
func TestDocumentsVerify(t *testing.T) {
	key := ecKey(t, elliptic.P256())
	_, jwks, err := Documents("https://oidc.example.com", "jwks", bundleOf(t, map[string]crypto.PublicKey{"k1": key.Public()}))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(signer).Claims(jwt.Claims{Subject: "spiffe://example.com/backup/anki"}).Serialize()
	if err != nil {
		t.Fatal(err)
	}

	var set jose.JSONWebKeySet
	if err := json.Unmarshal(jwks, &set); err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var claims jwt.Claims
	if err := parsed.Claims(set.Key("k1")[0].Key, &claims); err != nil {
		t.Fatalf("verifying with the published key: %v", err)
	}
}

// TestDocumentsJWKSKey checks that discovery names the JWKS wherever the
// store holds it.
func TestDocumentsJWKSKey(t *testing.T) {
	discovery, _, err := Documents("https://oidc.example.com", "keys", bundleOf(t, map[string]crypto.PublicKey{"k1": ecKey(t, elliptic.P256()).Public()}))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(discovery, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.JWKSURI != "https://oidc.example.com/keys" {
		t.Errorf("jwks_uri = %q", doc.JWKSURI)
	}
}

func TestDocumentsEmpty(t *testing.T) {
	if _, _, err := Documents("https://oidc.example.com", "jwks", jwtbundle.New(td)); err == nil {
		t.Error("an empty bundle published")
	}
}

type fakeStore struct {
	puts []string
	fail error
}

func (s *fakeStore) Put(_ context.Context, key, contentType string, _ []byte) error {
	if s.fail != nil {
		return s.fail
	}
	if contentType != "application/json" {
		return errors.New("content type " + contentType)
	}
	s.puts = append(s.puts, key)
	return nil
}

func TestPublisher(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	p := &Publisher{Issuer: "https://oidc.example.com", JWKSKey: "jwks", Store: store}

	// Nothing to refresh before the first bundle.
	if err := p.Refresh(ctx); err != nil || len(store.puts) != 0 {
		t.Fatalf("refresh before a bundle: %v, %v", err, store.puts)
	}

	k1 := ecKey(t, elliptic.P256()).Public()
	first := bundleOf(t, map[string]crypto.PublicKey{"k1": k1})
	if err := p.Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	// The JWKS before the discovery document that points at it.
	if want := []string{"jwks", DiscoveryKey}; !slices.Equal(store.puts, want) {
		t.Errorf("first update wrote %v, want %v", store.puts, want)
	}

	// The same keys write nothing.
	store.puts = nil
	if err := p.Update(ctx, bundleOf(t, map[string]crypto.PublicKey{"k1": k1})); err != nil {
		t.Fatal(err)
	}
	if len(store.puts) != 0 {
		t.Errorf("unchanged bundle wrote %v", store.puts)
	}

	// A new key of the same type changes the JWKS alone.
	if err := p.Update(ctx, bundleOf(t, map[string]crypto.PublicKey{"k1": k1, "k2": ecKey(t, elliptic.P256()).Public()})); err != nil {
		t.Fatal(err)
	}
	if want := []string{"jwks"}; !slices.Equal(store.puts, want) {
		t.Errorf("rotation wrote %v, want %v", store.puts, want)
	}

	// A refresh writes both, changed or not.
	store.puts = nil
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if want := []string{"jwks", DiscoveryKey}; !slices.Equal(store.puts, want) {
		t.Errorf("refresh wrote %v, want %v", store.puts, want)
	}

	// A failed write is tried again by the next update, though the bundle
	// is the same.
	store.puts = nil
	k3 := bundleOf(t, map[string]crypto.PublicKey{"k3": ecKey(t, elliptic.P256()).Public()})
	store.fail = errors.New("unavailable")
	if err := p.Update(ctx, k3); err == nil {
		t.Fatal("failed write reported no error")
	}
	store.fail = nil
	if err := p.Update(ctx, k3); err != nil {
		t.Fatal(err)
	}
	if want := []string{"jwks"}; !slices.Equal(store.puts, want) {
		t.Errorf("retry wrote %v, want %v", store.puts, want)
	}
}
