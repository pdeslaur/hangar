package integration

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstall(t *testing.T) {
	dir := t.TempDir()

	changed, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("first install: changed = false, want true")
	}
	for _, name := range []string{"__init__.py", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(dir, "custom_components", Domain, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	changed, err = Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second install: changed = true, want false")
	}

	manifest := filepath.Join(dir, "custom_components", Domain, "manifest.json")
	if err := os.WriteFile(manifest, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("install over an edited file: changed = false, want true")
	}
	want, err := files.ReadFile(Domain + "/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(manifest); err != nil || string(got) != string(want) {
		t.Errorf("manifest.json not restored: %q, %v", got, err)
	}
}
