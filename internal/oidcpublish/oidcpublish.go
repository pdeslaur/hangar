// Package oidcpublish turns a SPIFFE trust domain's JWT bundle into an OIDC
// issuer's static documents, a discovery document and a JWKS, and writes them
// to a store whenever they change.
package oidcpublish

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/go-jose/go-jose/v4"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
)

// The objects' keys in the store, relative to the issuer's URL.
const (
	JWKSKey      = "jwks"
	DiscoveryKey = ".well-known/openid-configuration"
)

// Documents returns the issuer's discovery document and JWKS for bundle. Each
// key is marked for signatures and given the algorithm its type implies, as
// verifiers like GCP's STS require: the bundle's own JWKS, in SPIFFE's format,
// marks them jwt-svid and names no algorithm. Keys are sorted by ID, so the
// same bundle always gives the same bytes.
func Documents(issuer string, bundle *jwtbundle.Bundle) (discovery, jwks []byte, err error) {
	authorities := bundle.JWTAuthorities()
	if len(authorities) == 0 {
		return nil, nil, fmt.Errorf("the bundle of %s holds no JWT authority", bundle.TrustDomain())
	}
	set := jose.JSONWebKeySet{}
	algs := map[string]bool{}
	for _, kid := range slices.Sorted(maps.Keys(authorities)) {
		alg, err := algorithm(authorities[kid])
		if err != nil {
			return nil, nil, fmt.Errorf("key %s: %w", kid, err)
		}
		algs[alg] = true
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: authorities[kid], KeyID: kid, Use: "sig", Algorithm: alg})
	}
	if jwks, err = json.MarshalIndent(set, "", "  "); err != nil {
		return nil, nil, err
	}
	discovery, err = json.MarshalIndent(map[string]any{
		"issuer":                                issuer,
		"jwks_uri":                              issuer + "/" + JWKSKey,
		"id_token_signing_alg_values_supported": slices.Sorted(maps.Keys(algs)),
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
	}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return append(discovery, '\n'), append(jwks, '\n'), nil
}

// algorithm returns the JWS algorithm SPIRE signs with for a key of this type.
func algorithm(key crypto.PublicKey) (string, error) {
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256():
			return "ES256", nil
		case elliptic.P384():
			return "ES384", nil
		case elliptic.P521():
			return "ES512", nil
		}
		return "", fmt.Errorf("unsupported curve %s", k.Curve.Params().Name)
	case *rsa.PublicKey:
		return "RS256", nil
	case ed25519.PublicKey:
		return "EdDSA", nil
	}
	return "", fmt.Errorf("unsupported key type %T", key)
}

// Store holds the issuer's documents, like a bucket served on the issuer's
// URL.
type Store interface {
	Put(ctx context.Context, key, contentType string, body []byte) error
}

// Publisher writes the documents of the latest bundle to Store.
type Publisher struct {
	Issuer string
	Store  Store

	mu      sync.Mutex
	bundle  *jwtbundle.Bundle
	written map[string][]byte
}

// Update publishes bundle's documents, writing only those that changed since
// the last successful write. The JWKS goes first, so the discovery document
// never points at keys that aren't there yet.
func (p *Publisher) Update(ctx context.Context, bundle *jwtbundle.Bundle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bundle = bundle
	return p.publish(ctx, false)
}

// Refresh writes the latest bundle's documents again, changed or not, so that
// an object deleted or overwritten in the store comes back. It does nothing
// before the first bundle.
func (p *Publisher) Refresh(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bundle == nil {
		return nil
	}
	return p.publish(ctx, true)
}

func (p *Publisher) publish(ctx context.Context, force bool) error {
	discovery, jwks, err := Documents(p.Issuer, p.bundle)
	if err != nil {
		return err
	}
	if p.written == nil {
		p.written = map[string][]byte{}
	}
	for _, doc := range []struct {
		key  string
		body []byte
	}{{JWKSKey, jwks}, {DiscoveryKey, discovery}} {
		if !force && bytes.Equal(p.written[doc.key], doc.body) {
			continue
		}
		if err := p.Store.Put(ctx, doc.key, "application/json", doc.body); err != nil {
			return fmt.Errorf("writing %s: %w", doc.key, err)
		}
		p.written[doc.key] = doc.body
	}
	return nil
}
