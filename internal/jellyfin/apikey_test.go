package jellyfin

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// apiKeysSchema is the ApiKeys table of Jellyfin 12.4's database.
const apiKeysSchema = `
CREATE TABLE "ApiKeys" (
    "Id" INTEGER NOT NULL CONSTRAINT "PK_ApiKeys" PRIMARY KEY AUTOINCREMENT,
    "DateCreated" TEXT NOT NULL,
    "DateLastActivity" TEXT NOT NULL,
    "Name" TEXT NOT NULL,
    "AccessToken" TEXT NOT NULL
);
CREATE UNIQUE INDEX "IX_ApiKeys_AccessToken" ON "ApiKeys" ("AccessToken");
`

// database creates a SQLite database at a temporary path with schema,
// returning its path and a connection to it.
func database(t *testing.T, schema string) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jellyfin.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return path, db
}

// apiKeys returns the database's API keys, as name=token.
func apiKeys(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT Name, AccessToken FROM ApiKeys ORDER BY Id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var name, token string
		if err := rows.Scan(&name, &token); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, name+"="+token)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestEnsureAPIKey(t *testing.T) {
	ctx := context.Background()
	path, db := database(t, apiKeysSchema)
	if _, err := db.Exec(`INSERT INTO ApiKeys (DateCreated, DateLastActivity, Name, AccessToken) VALUES ('2026-01-01 00:00:00', '2026-01-01 00:00:00', 'sonarr', 'aaaa')`); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		name, token string
		want        string
	}{
		{"bridge", "bbbb", "sonarr=aaaa bridge=bbbb"},
		// Again: nothing changes.
		{"bridge", "bbbb", "sonarr=aaaa bridge=bbbb"},
		// Another token replaces the key's.
		{"bridge", "cccc", "sonarr=aaaa bridge=cccc"},
	} {
		if err := EnsureAPIKey(ctx, path, step.name, step.token); err != nil {
			t.Fatalf("EnsureAPIKey(%q, %q): %v", step.name, step.token, err)
		}
		if got := strings.Join(apiKeys(t, db), " "); got != step.want {
			t.Errorf("after EnsureAPIKey(%q, %q): keys %q, want %q", step.name, step.token, got, step.want)
		}
	}

	var created string
	if err := db.QueryRow(`SELECT DateCreated FROM ApiKeys WHERE Name = 'bridge'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if len(created) < len("2006-01-02 15:04:05") || created[10] != ' ' {
		t.Errorf("DateCreated %q isn't in Jellyfin's format", created)
	}
}

func TestEnsureAPIKeyRefuses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, schema, want string
	}{
		{"no table", `CREATE TABLE Other (Id INTEGER)`, "no table ApiKeys"},
		{"other columns", `CREATE TABLE ApiKeys (Id INTEGER PRIMARY KEY, Name TEXT, AccessToken TEXT, Hash TEXT)`, "has columns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, _ := database(t, tc.schema)
			err := EnsureAPIKey(ctx, path, "bridge", "bbbb")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("EnsureAPIKey: %v, want an error containing %q", err, tc.want)
			}
		})
	}

	t.Run("no database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "jellyfin.db")
		if err := EnsureAPIKey(ctx, path, "bridge", "bbbb"); err == nil {
			t.Error("EnsureAPIKey created a database")
		}
	})
}
