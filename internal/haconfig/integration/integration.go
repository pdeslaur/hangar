// Package integration holds config_sync, the Home Assistant integration that
// ha-config-sync talks to, and installs it into a configuration directory.
package integration

import (
	"bytes"
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Domain is the integration's, the key that enables it in
// configuration.yaml.
const Domain = "config_sync"

//go:embed config_sync/__init__.py config_sync/manifest.json
var files embed.FS

// Install writes the integration into dir's custom_components, and reports
// whether any file changed: Home Assistant loads an integration's code only
// as it starts.
func Install(dir string) (changed bool, err error) {
	dst := filepath.Join(dir, "custom_components", Domain)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return false, err
	}
	err = fs.WalkDir(files, Domain, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		name := filepath.Join(dst, d.Name())
		have, err := os.ReadFile(name)
		if err == nil && bytes.Equal(have, want) {
			return nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		changed = true
		return writeFile(name, want)
	})
	return changed, err
}

// writeFile replaces name with data through a rename, so that Home Assistant
// never reads half a file.
func writeFile(name string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}
