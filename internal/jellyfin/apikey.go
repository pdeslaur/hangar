package jellyfin

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"slices"
	"time"

	_ "modernc.org/sqlite"
)

// apiKeyColumns are the columns of Jellyfin's ApiKeys table that
// EnsureAPIKey knows, in order.
var apiKeyColumns = []string{"Id", "DateCreated", "DateLastActivity", "Name", "AccessToken"}

// dateFormat is how Jellyfin's Entity Framework stores a DateTime in SQLite,
// "yyyy-MM-dd HH:mm:ss.FFFFFFF", in UTC.
const dateFormat = "2006-01-02 15:04:05.9999999"

// EnsureAPIKey makes the API key called name, in the Jellyfin database at
// path, be token, adding it or replacing the one it had. Jellyfin reads API
// keys from its database on each request, so it takes the key at once,
// without a restart.
//
// It refuses a database whose ApiKeys table has other columns than the ones
// it knows, which a Jellyfin release could change, and one Jellyfin hasn't
// created yet. Jellyfin keeps its devices in memory, so nothing but API keys
// may be written to its database this way.
//
// It deletes the keys called any of formerNames, as a renamed key's.
func EnsureAPIKey(ctx context.Context, path, name, token string, formerNames ...string) error {
	if name == "" || len(name) > 64 || token == "" {
		return fmt.Errorf("jellyfin: an API key needs a name of 1 to 64 bytes and a token")
	}
	// Jellyfin holds the database open in WAL mode: wait for its locks,
	// and never create the file.
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "mode=rw&_pragma=busy_timeout(10000)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	columns, err := tableColumns(ctx, db, "ApiKeys")
	if err != nil {
		return err
	}
	if !slices.Equal(columns, apiKeyColumns) {
		return fmt.Errorf("jellyfin: ApiKeys has columns %v, not %v", columns, apiKeyColumns)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, former := range formerNames {
		if former == name {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ApiKeys WHERE Name = ?`, former); err != nil {
			return err
		}
	}
	var present bool
	err = tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM ApiKeys WHERE Name = ? AND AccessToken = ?)`,
		name, token).Scan(&present)
	if err != nil {
		return err
	}
	if present {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ApiKeys WHERE Name = ?`, name); err != nil {
		return err
	}
	now := time.Now().UTC().Format(dateFormat)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO ApiKeys (DateCreated, DateLastActivity, Name, AccessToken) VALUES (?, ?, ?, ?)`,
		now, now, name, token)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// tableColumns returns the names of table's columns, in order, and an error
// if it doesn't exist.
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		columns = append(columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("jellyfin: no table %s: has Jellyfin set up its database?", table)
	}
	return columns, nil
}
