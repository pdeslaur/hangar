package alertrelay

import (
	"context"
	"database/sql"
	"errors"
	"time"

	// database/sql's "sqlite" driver, in Go, without cgo.
	_ "modernc.org/sqlite"
)

// Store keeps the alerts that are firing in a SQLite database, so that a
// restart, of the relay or of the rule engine resending everything it has
// firing, sends nothing twice. An alert is keyed by its labels.
type Store struct {
	db *sql.DB
}

// Stored is an alert the store holds as firing.
type Stored struct {
	// Labels is the alert's labels as JSON, its key.
	Labels string
	// Annotations is the alert's annotations as JSON.
	Annotations string
	// EndsAt is when the alert stops firing unless the rule engine sends it
	// again.
	EndsAt time.Time
}

const schema = `CREATE TABLE IF NOT EXISTS firing (
	labels      TEXT PRIMARY KEY,
	annotations TEXT NOT NULL,
	ends_at     INTEGER NOT NULL
)`

// OpenStore opens the database at path, creating it and its table if they
// don't exist.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Firing reports whether the alert with labels is firing.
func (s *Store) Firing(ctx context.Context, labels string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM firing WHERE labels = ?`, labels).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Fire records the alert with labels as firing until endsAt, or moves its
// end to endsAt if it's firing already.
func (s *Store) Fire(ctx context.Context, labels, annotations string, endsAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO firing (labels, annotations, ends_at) VALUES (?, ?, ?)
		ON CONFLICT (labels) DO UPDATE SET annotations = excluded.annotations, ends_at = excluded.ends_at`,
		labels, annotations, endsAt.Unix())
	return err
}

// Resolve forgets the alert with labels.
func (s *Store) Resolve(ctx context.Context, labels string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM firing WHERE labels = ?`, labels)
	return err
}

// Expired returns the alerts whose end is at or before now.
func (s *Store) Expired(ctx context.Context, now time.Time) ([]Stored, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT labels, annotations, ends_at FROM firing WHERE ends_at <= ? ORDER BY ends_at`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var expired []Stored
	for rows.Next() {
		var a Stored
		var endsAt int64
		if err := rows.Scan(&a.Labels, &a.Annotations, &endsAt); err != nil {
			return nil, err
		}
		a.EndsAt = time.Unix(endsAt, 0)
		expired = append(expired, a)
	}
	return expired, rows.Err()
}
