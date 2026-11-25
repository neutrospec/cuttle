package stats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure Go, no cgo (design 0002 D1)
)

// SchemaVersion is the user_version of the schema this binary writes
// (design 0005 D2).
const SchemaVersion = 1

// ErrNewerSchema means the database has a newer schema than this binary
// knows; it is left untouched.
var ErrNewerSchema = errors.New("statistics database has a newer schema")

// Retention batches (design 0005 D3).
const (
	pruneBatch  = 5000
	vacuumPages = 1000
)

const createTable = `CREATE TABLE host_hour (
  bucket        INTEGER NOT NULL,
  host          TEXT    NOT NULL,
  action        TEXT    NOT NULL CHECK (action IN ('direct', 'upstream', 'block')),
  upstream      TEXT    NOT NULL DEFAULT '',
  rule          TEXT    NOT NULL,
  source        TEXT    NOT NULL CHECK (source IN ('local', 'remote')),
  interface     TEXT    NOT NULL DEFAULT '',
  connects      INTEGER NOT NULL DEFAULT 0,
  http_requests INTEGER NOT NULL DEFAULT 0,
  failures      INTEGER NOT NULL DEFAULT 0,
  bytes_up      INTEGER NOT NULL DEFAULT 0,
  bytes_down    INTEGER NOT NULL DEFAULT 0,
  first_seen    INTEGER NOT NULL,
  last_seen     INTEGER NOT NULL,
  PRIMARY KEY (bucket, host, action, upstream, rule, source, interface)
) STRICT, WITHOUT ROWID`

const upsert = `INSERT INTO host_hour VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (bucket, host, action, upstream, rule, source, interface)
DO UPDATE SET
  connects      = connects      + excluded.connects,
  http_requests = http_requests + excluded.http_requests,
  failures      = failures      + excluded.failures,
  bytes_up      = bytes_up      + excluded.bytes_up,
  bytes_down    = bytes_down    + excluded.bytes_down,
  first_seen    = min(first_seen, excluded.first_seen),
  last_seen     = max(last_seen,  excluded.last_seen)`

var pruneRows = fmt.Sprintf(`DELETE FROM host_hour
WHERE (bucket, host, action, upstream, rule, source, interface) IN (
  SELECT bucket, host, action, upstream, rule, source, interface
  FROM host_hour WHERE bucket < ? LIMIT %d)`, pruneBatch)

// Store is the SQLite statistics database (design 0005 D2). It holds a
// single connection, the writer.
type Store struct {
	db   *sql.DB
	path string
}

// OpenStore opens or creates the database at path with mode 0600, sets the
// per-connection PRAGMAs, and creates or checks the schema. A database with
// a newer user_version gives ErrNewerSchema and is left untouched.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := prepareFile(path); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// prepareFile creates the database file with mode 0600 before SQLite opens
// it, so the -wal and -shm files, which SQLite creates with the database
// file's mode, are 0600 too. An existing file and its -wal and -shm files
// lose any group and other permissions (design 0006 D1).
func prepareFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(p, fi.Mode().Perm()&0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

// migrate creates the schema in a new database and refuses a newer one
// (design 0005 D2). WAL and incremental auto-vacuum persist in the file and
// are set when it is created, auto-vacuum before the first table.
func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case v > SchemaVersion:
		return fmt.Errorf("%w: version %d, this binary knows %d", ErrNewerSchema, v, SchemaVersion)
	case v == SchemaVersion:
		return nil
	}
	var tables int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	if tables != 0 {
		return errors.New("statistics database has tables but no schema version")
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return fmt.Errorf("set auto_vacuum: %w", err)
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
		return fmt.Errorf("set journal_mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("journal_mode is %q, not wal", mode)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Path returns the database path.
func (s *Store) Path() string { return s.path }

// Write upserts rows in one transaction (design 0005 D2, D4).
func (s *Store) Write(ctx context.Context, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, upsert)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.Bucket, r.Host, r.Action, r.Upstream, r.Rule, r.Source, r.Interface,
			r.Connects, r.HTTPRequests, r.Failures, r.BytesUp, r.BytesDown, r.FirstSeen, r.LastSeen); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prune deletes the rows whose bucket is before cutoff in batches of 5,000
// rows, then frees up to 1,000 pages (design 0005 D3). It returns the
// number of rows deleted.
func (s *Store) Prune(ctx context.Context, cutoff int64) (int64, error) {
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, pruneRows, cutoff)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < pruneBatch {
			break
		}
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", vacuumPages))
	return total, err
}

// Rows returns every row in key order.
func (s *Store) Rows(ctx context.Context) ([]Row, error) {
	rs, err := s.db.QueryContext(ctx, `SELECT bucket, host, action, upstream, rule, source, interface,
  connects, http_requests, failures, bytes_up, bytes_down, first_seen, last_seen
FROM host_hour ORDER BY bucket, host, action, upstream, rule, source, interface`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Row
	for rs.Next() {
		var r Row
		if err := rs.Scan(&r.Bucket, &r.Host, &r.Action, &r.Upstream, &r.Rule, &r.Source, &r.Interface,
			&r.Connects, &r.HTTPRequests, &r.Failures, &r.BytesUp, &r.BytesDown, &r.FirstSeen, &r.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
