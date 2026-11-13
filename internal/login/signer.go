package login

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	// Issuer is the iss of the JWTs it signs, which tuwunel requires.
	Issuer = "tuwunel-svid-sts"
	// Audience is the aud of the JWTs it signs, which tuwunel requires.
	Audience = "tuwunel"
	// tokenTTL bounds how long a signed JWT is accepted. It's only presented
	// once, right after it's signed.
	tokenTTL = time.Minute
)

// Signer signs the JWTs tuwunel exchanges for access tokens, with an ES256
// key that exists only in memory: tuwunel trusts its public key, written to
// a config file at startup, so each start of the process needs tuwunel to
// read the file again.
type Signer struct {
	key    *ecdsa.PrivateKey
	signer jose.Signer
}

// NewSigner generates a key.
func NewSigner() (*Signer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return nil, err
	}
	return &Signer{key: key, signer: signer}, nil
}

// Sign returns a JWT whose sub is the Matrix localpart to log in as.
func (s *Signer) Sign(localpart string, now time.Time) (string, error) {
	return jwt.Signed(s.signer).Claims(jwt.Claims{
		Subject:   localpart,
		Issuer:    Issuer,
		Audience:  jwt.Audience{Audience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		Expiry:    jwt.NewNumericDate(now.Add(tokenTTL)),
	}).Serialize()
}

// Public returns the public key that verifies its JWTs.
func (s *Signer) Public() *ecdsa.PublicKey {
	return &s.key.PublicKey
}

// TuwunelConfig returns a tuwunel config file enabling JWT login with its
// public key, for existing users only.
func (s *Signer) TuwunelConfig() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	if err != nil {
		return nil, err
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return fmt.Appendf(nil, `[global.jwt]
enable = true
format = "ECDSA"
algorithm = "ES256"
key = """
%s"""
issuer = [%q]
audience = [%q]
require_exp = true
require_nbf = true
register_user = false
expire_access_token = true
`, key, Issuer, Audience), nil
}

// WriteTuwunelConfig writes TuwunelConfig to path, replacing it atomically
// so tuwunel never reads a partial file.
func (s *Signer) WriteTuwunelConfig(path string) error {
	b, err := s.TuwunelConfig()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tuwunel-jwt-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
