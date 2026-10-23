package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Luo-root/pulse-durable/sqlitestore"
	_ "modernc.org/sqlite"
)

// openBenchmarkSQLite belongs to the Eino blob adapter and independent inspection,
// not Durable persistence. Each connection uses WAL/FULL and serialized operations.
func openBenchmarkSQLite(path string) (*sql.DB, string, error) {
	if path == "" || path == ":memory:" {
		return nil, "", errors.New("benchmark sqlite: a persistent file path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, "", err
	}
	uriPath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, "", err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(err error) (*sql.DB, string, error) {
		return nil, "", errors.Join(err, db.Close())
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return fail(err)
	}
	var synchronous int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		return fail(err)
	}
	if mode != "wal" || synchronous != 2 {
		return fail(fmt.Errorf("benchmark sqlite: journal_mode=%s synchronous=%d, want wal/2", mode, synchronous))
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return fail(err)
	}
	return db, absolute, nil
}

// blobStore is Eino's independent checkpoint blob adapter; it never changes Eino's format.
type blobStore struct{ db *sql.DB }

func newBlobStore(path string) (*blobStore, error) {
	db, _, err := openBenchmarkSQLite(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS eino_checkpoints (
		key TEXT PRIMARY KEY,
		value BLOB NOT NULL
	) WITHOUT ROWID`); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &blobStore{db: db}, nil
}

func (s *blobStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := benchmarkSQLiteContext(ctx); err != nil {
		return nil, false, err
	}
	var value []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM eino_checkpoints WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return value, err == nil, err
}

func (s *blobStore) Set(ctx context.Context, key string, value []byte) error {
	if err := benchmarkSQLiteContext(ctx); err != nil {
		return err
	}
	if value == nil {
		value = []byte{}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO eino_checkpoints(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *blobStore) Close() error { return s.db.Close() }

// benchmarkDurableStore delegates ALL persistence and ownership to the maintained
// public backend. db is an independent inspection/fault-injection connection only;
// it is never used by Runtime Load/Save and holds no duplicate record or schema.
type benchmarkDurableStore struct {
	*sqlitestore.Store
	db       *sql.DB
	once     sync.Once
	closeErr error
}

func newSQLiteStore(path string) (*benchmarkDurableStore, error) {
	store, err := sqlitestore.New(path)
	if err != nil {
		return nil, err
	}
	db, _, err := openBenchmarkSQLite(path)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return &benchmarkDurableStore{Store: store, db: db}, nil
}

func (s *benchmarkDurableStore) Close() error {
	s.once.Do(func() { s.closeErr = errors.Join(s.Store.Close(), s.db.Close()) })
	return s.closeErr
}

func benchmarkSQLiteContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("benchmark sqlite: context is nil")
	}
	return ctx.Err()
}
