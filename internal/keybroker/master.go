package keybroker

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// MasterSize is the master secret's size.
const MasterSize = 32

// LoadMaster reads the master secret from path. When it's missing, it
// creates it, readable by its owner only, if create is set, and fails
// otherwise. It refuses a file that isn't MasterSize bytes: every derived
// secret depends on the master, so it never replaces one.
func LoadMaster(path string, create bool) ([]byte, error) {
	master, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		return createMaster(path)
	}
	if err != nil {
		return nil, err
	}
	if len(master) != MasterSize {
		return nil, fmt.Errorf("%s: %d bytes, want %d", path, len(master), MasterSize)
	}
	return master, nil
}

// createMaster writes a random master to a temporary file and links it to
// path, so that path either doesn't exist or holds a whole master, and a
// master created concurrently is never overwritten.
func createMaster(path string) ([]byte, error) {
	master := make([]byte, MasterSize)
	rand.Read(master)

	tmp, err := os.CreateTemp(filepath.Dir(path), ".master-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(master); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		return nil, err
	}
	return master, nil
}
