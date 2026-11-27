package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMoveFormerState(t *testing.T) {
	ctx := context.Background()
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T, path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return "<none>"
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	for _, tc := range []struct {
		name string
		// files are the directory's before, by name, and want after.
		files, want map[string]string
	}{{
		name:  "moves the database with its log",
		files: map[string]string{"old.db": "db", "old.db-wal": "wal", "old.db-shm": "shm"},
		want:  map[string]string{"new.db": "db", "new.db-wal": "wal", "new.db-shm": "shm", "old.db": "<none>", "old.db-wal": "<none>"},
	}, {
		name:  "moves a database without a log",
		files: map[string]string{"old.db": "db"},
		want:  map[string]string{"new.db": "db", "new.db-wal": "<none>", "old.db": "<none>"},
	}, {
		name:  "finishes a move cut short",
		files: map[string]string{"old.db": "db", "new.db-wal": "wal"},
		want:  map[string]string{"new.db": "db", "new.db-wal": "wal", "old.db": "<none>"},
	}, {
		name:  "keeps a database under the new name",
		files: map[string]string{"old.db": "stale", "new.db": "db"},
		want:  map[string]string{"new.db": "db", "old.db": "stale"},
	}, {
		name:  "starts afresh without either",
		files: map[string]string{},
		want:  map[string]string{"new.db": "<none>"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				write(t, filepath.Join(dir, name), content)
			}
			if err := moveFormerState(ctx, filepath.Join(dir, "old.db"), filepath.Join(dir, "new.db")); err != nil {
				t.Fatal(err)
			}
			for name, want := range tc.want {
				if got := read(t, filepath.Join(dir, name)); got != want {
					t.Errorf("%s: %q, want %q", name, got, want)
				}
			}
		})
	}
}
