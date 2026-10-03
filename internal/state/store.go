package state

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
}

// ErrNoProject means the current directory is not bound to a project yet.
var ErrNoProject = errors.New("no project is bound to this directory")

// Open creates or opens the store at path, applying any pending migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One writer avoids SQLITE_BUSY on a store that a cron'd reap may also touch.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(migrations[0]); err != nil {
		return fmt.Errorf("state: creating meta table: %w", err)
	}
	var have int
	row := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`)
	var v string
	switch err := row.Scan(&v); {
	case err == sql.ErrNoRows:
		have = 0
	case err != nil:
		return err
	default:
		have, _ = strconv.Atoi(v)
	}
	if have >= schemaVersion {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range migrations[1:] {
		if _, err := tx.Exec(m); err != nil {
			return fmt.Errorf("state: migration failed: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Tx runs fn in a transaction, so a multi-table change is all or nothing.
func (s *Store) Tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) DB() *sql.DB { return s.db }
