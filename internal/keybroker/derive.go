// Package keybroker derives each workload's secrets from one master secret
// and the workload's SPIFFE ID, and fetches them for a workload into env
// files.
package keybroker

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const (
	// MinBytes and MaxBytes bound a derived secret's size.
	MinBytes = 16
	MaxBytes = 64
	// maxName bounds a secret's name.
	maxName = 128
)

// Derive returns the secret called name, at version, of the workload id: an
// HKDF-SHA256 of master, salted with id's trust domain name, with the info
// id, name and version separated by NUL bytes, which none of them contains,
// so that two different triples never give the same info.
func Derive(master []byte, id spiffeid.ID, name string, version, size int) ([]byte, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if version < 1 {
		return nil, fmt.Errorf("version %d: must be at least 1", version)
	}
	if size < MinBytes || size > MaxBytes {
		return nil, fmt.Errorf("bytes %d: must be between %d and %d", size, MinBytes, MaxBytes)
	}
	info := id.String() + "\x00" + name + "\x00" + strconv.Itoa(version)
	return hkdf.Key(sha256.New, master, []byte(id.TrustDomain().Name()), info, size)
}

// validName accepts names of letters, digits, "_", "-" and ".", in segments
// separated by "/", none of them empty, "." or "..", so that a name is the
// same in a URL path as it is in the derivation.
func validName(name string) error {
	if name == "" || len(name) > maxName {
		return fmt.Errorf("name %q: must have 1 to %d characters", name, maxName)
	}
	for seg := range strings.SplitSeq(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("name %q: empty, \".\" or \"..\" segment", name)
		}
		for _, c := range seg {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
				return fmt.Errorf("name %q: invalid character %q", name, c)
			}
		}
	}
	return nil
}
