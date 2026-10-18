package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	durable "github.com/Luo-root/pulse-durable"
	"github.com/Luo-root/pulse-durable/filestore"
	_ "modernc.org/sqlite"
)

// openBenchmarkSQLite 仅用于评测宿主。每次连接均启用 WAL/FULL；一个 DB
// 最多保留一个连接，允许并发调用但串行写入，不代表生产后端或 Temporal 的事务布局。
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
	// 让通过同一文件系统别名打开的 Store 尽量共享同一系统锁目录。
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return fail(err)
	}
	return db, absolute, nil
}

// blobStore 是 Eino CheckPointStore 共用的 SQLite blob 接缝。
// 每次 Set 原子替换完整 blob；不解码或改写 Eino 自己的检查点格式。
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
	// nil 也是存在的空 blob，不能被 SQLite 解释为 NULL 或缺失。
	if value == nil {
		value = []byte{}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO eino_checkpoints(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *blobStore) Close() error { return s.db.Close() }

// sqliteStore 是本机评测专用 Store。完整记录放 SQLite，独占会话仅借用官方
// filestore 的 Acquire/Close 系统文件锁，不调用它的 Load/Save，不写第二份记录。
// 锁在会话关闭或进程退出时释放；不提供跨机器租约、fencing 或任务派发。
type sqliteStore struct {
	db       *sql.DB
	locks    *filestore.Store
	mu       sync.Mutex
	closed   bool
	sessions map[*sqliteSession]struct{}
	once     sync.Once
	closeErr error
}

var _ durable.Store = (*sqliteStore)(nil)

func newSQLiteStore(path string) (*sqliteStore, error) {
	db, absolute, err := openBenchmarkSQLite(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS durable_runs (
		run_id TEXT PRIMARY KEY,
		revision TEXT NOT NULL,
		record BLOB NOT NULL
	) WITHOUT ROWID`); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	locks, err := filestore.New(absolute + ".locks")
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &sqliteStore{db: db, locks: locks, sessions: make(map[*sqliteSession]struct{})}, nil
}

func (s *sqliteStore) Acquire(ctx context.Context, runID string) (durable.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, os.ErrClosed
	}
	// 官方锁检查 context、非空 UTF-8 RunID；同 RunID 立即返回 ErrBusy。
	ownership, err := s.locks.Acquire(ctx, runID)
	if err != nil {
		return nil, err
	}
	session := &sqliteSession{store: s, runID: runID, ownership: ownership}
	s.sessions[session] = struct{}{}
	return session, nil
}

func (s *sqliteStore) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		sessions := s.sessions
		s.sessions = nil
		s.mu.Unlock()
		for session := range sessions {
			s.closeErr = errors.Join(s.closeErr, session.shutdown())
		}
		s.closeErr = errors.Join(s.closeErr, s.db.Close())
	})
	return s.closeErr
}

type sqliteSession struct {
	store     *sqliteStore
	runID     string
	ownership durable.Session
	mu        sync.Mutex
	closed    bool
	closeErr  error
}

var _ durable.Session = (*sqliteSession)(nil)

func (s *sqliteSession) Load(ctx context.Context) (durable.RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return durable.RunRecord{}, os.ErrClosed
	}
	if err := benchmarkSQLiteContext(ctx); err != nil {
		return durable.RunRecord{}, err
	}
	var data []byte
	var revision string
	err := s.store.db.QueryRowContext(ctx, "SELECT revision, record FROM durable_runs WHERE run_id = ?", s.runID).Scan(&revision, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return durable.RunRecord{}, durable.ErrNotFound
	}
	if err != nil {
		return durable.RunRecord{}, err
	}
	var record durable.RunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return durable.RunRecord{}, fmt.Errorf("benchmark sqlite: decode run: %w", err)
	}
	if record.RunID != s.runID || record.Revision == 0 || strconv.FormatUint(record.Revision, 10) != revision {
		return durable.RunRecord{}, fmt.Errorf("benchmark sqlite: corrupt run identity or revision: %w", durable.ErrConflict)
	}
	return record, nil
}

func (s *sqliteSession) Save(ctx context.Context, record durable.RunRecord, expected uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	if err := benchmarkSQLiteContext(ctx); err != nil {
		return err
	}
	if record.RunID != s.runID || expected == ^uint64(0) || record.Revision != expected+1 {
		return durable.ErrConflict
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	// Revision 用十进制 TEXT，保留 uint64 全范围；CAS 和整份 JSON 同一条
	// SQLite 自动提交事务中完成，失败不会留下半份记录，不自创增量优化。
	var result sql.Result
	if expected == 0 {
		result, err = s.store.db.ExecContext(ctx, `INSERT INTO durable_runs(run_id, revision, record) VALUES (?, ?, ?)
			ON CONFLICT(run_id) DO NOTHING`, s.runID, strconv.FormatUint(record.Revision, 10), data)
	} else {
		result, err = s.store.db.ExecContext(ctx, `UPDATE durable_runs SET revision = ?, record = ?
			WHERE run_id = ? AND revision = ?`, strconv.FormatUint(record.Revision, 10), data, s.runID, strconv.FormatUint(expected, 10))
	}
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return durable.ErrConflict
	}
	return nil
}

func (s *sqliteSession) shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.closeErr = s.ownership.Close()
	}
	return s.closeErr
}

func (s *sqliteSession) Close() error {
	err := s.shutdown()
	s.store.mu.Lock()
	delete(s.store.sessions, s)
	s.store.mu.Unlock()
	return err
}

func benchmarkSQLiteContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("benchmark sqlite: context is nil")
	}
	return ctx.Err()
}
