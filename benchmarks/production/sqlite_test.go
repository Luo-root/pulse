package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	durable "github.com/Luo-root/pulse-durable"
)

// 所有可见验收产物仅写到分配的验证目录，不与正式矩阵或系统临时目录混用。
func durableTestDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "validation-bench"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, strings.ReplaceAll(t.Name(), "/", "-")+"-")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBenchmarkBlobStore(t *testing.T) {
	ctx := context.Background()
	s, err := newBlobStore(filepath.Join(durableTestDir(t), "state #.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, found, err := s.Get(ctx, "missing"); err != nil || found {
		t.Fatalf("missing: found=%v err=%v", found, err)
	}
	value := []byte("checkpoint")
	if err := s.Set(ctx, "run", value); err != nil {
		t.Fatal(err)
	}
	value[0] = 'x'
	got, found, err := s.Get(ctx, "run")
	if err != nil || !found || string(got) != "checkpoint" {
		t.Fatalf("blob: %q found=%v err=%v", got, found, err)
	}
	got[0] = 'y'
	if fresh, _, err := s.Get(ctx, "run"); err != nil || string(fresh) != "checkpoint" {
		t.Fatalf("Get returned aliased storage: %q %v", fresh, err)
	}
	if err := s.Set(ctx, "run", nil); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.Get(ctx, "run"); err != nil || !found || len(got) != 0 {
		t.Fatalf("empty blob: %q found=%v err=%v", got, found, err)
	}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			key := strconv.Itoa(i)
			if err := s.Set(ctx, key, []byte(key)); err != nil {
				t.Error(err)
				return
			}
			if got, found, err := s.Get(ctx, key); err != nil || !found || string(got) != key {
				t.Errorf("concurrent blob %s: %q %v %v", key, got, found, err)
			}
		})
	}
	wg.Wait()
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("max connections=%d", got)
	}
	// 强制更换连接，确认 FULL 并非只在首个连接上设置。
	s.db.SetMaxIdleConns(0)
	var journal string
	var synchronous int
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" || synchronous != 2 {
		t.Fatalf("journal=%s synchronous=%d", journal, synchronous)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Set(cancelled, "cancelled", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Set: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, "run"); err == nil {
		t.Fatal("Get after close succeeded")
	}
	if err := s.Set(ctx, "run", nil); err == nil {
		t.Fatal("Set after close succeeded")
	}
}

func TestOfficialSQLiteSchemaAndLocks(t *testing.T) {
	path := filepath.Join(durableTestDir(t), "official.db")
	s, err := newSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	var journal string
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("official schema version=%d err=%v", version, err)
	}
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal=%s err=%v", journal, err)
	}
	var synchronous int
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("inspection synchronous=%d err=%v", synchronous, err)
	}
	// FULL 只对当前连接生效：这里回读的是独立读取连接的设置，
	// 不能证明后端私有连接的配置；后端的公开源码配置了 FULL。
	session, err := s.Acquire(context.Background(), "layout")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	sum := sha256.Sum256([]byte("layout"))
	for _, lock := range []string{path + ".init.lock", filepath.Join(path+".runlocks", fmt.Sprintf("%x.lock", sum))} {
		if _, err := os.Stat(lock); err != nil {
			t.Fatalf("official lock %s: %v", lock, err)
		}
	}
	// 历史评测文件虽然有类似的列，但并非正式 v1 格式。
	// 必须保留其历史格式，不能通过设置 user_version 将其纳入正式格式。
	legacyPath := filepath.Join(durableTestDir(t), "legacy.db")
	db, _, err := openBenchmarkSQLite(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE durable_runs (run_id TEXT PRIMARY KEY, revision TEXT NOT NULL, record BLOB NOT NULL) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}
	if legacy, err := newSQLiteStore(legacyPath); err == nil {
		legacy.Close()
		t.Fatal("legacy benchmark schema silently adopted")
	}
}

func TestBenchmarkSQLiteSessionCAS(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(durableTestDir(t), "runs.db")
	s, err := newSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := newSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	session, err := s.Acquire(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Acquire(ctx, "run"); !errors.Is(err, durable.ErrBusy) {
		t.Fatalf("second Store Acquire: %v", err)
	}
	if _, err := session.Load(ctx); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("new session Load: %v", err)
	}
	if err := session.Save(ctx, durable.RunRecord{RunID: "another", Revision: 1}, 0); !errors.Is(err, durable.ErrConflict) {
		t.Fatalf("wrong RunID: %v", err)
	}
	if err := session.Save(ctx, durable.RunRecord{RunID: "run", Revision: 2}, 0); !errors.Is(err, durable.ErrConflict) {
		t.Fatalf("wrong first revision: %v", err)
	}
	record := durable.RunRecord{RunID: "run", Revision: 1, Scopes: map[string]durable.ScopeRecord{
		"root": {Nodes: map[string]durable.NodeRecord{"n0": {NodeID: "n0", Outputs: []durable.ValueRecord{{Data: []byte("18446744073709551615")}}}}},
	}}
	if err := session.Save(ctx, record, 0); err != nil {
		t.Fatal(err)
	}
	record.Scopes["root"].Nodes["n0"].Outputs[0].Data[0] = '0'
	loaded, err := session.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.Scopes["root"].Nodes["n0"].Outputs[0].Data) != "18446744073709551615" {
		t.Fatal("Save retained mutable input")
	}
	loaded.Scopes["root"].Nodes["n0"].Outputs[0].Data[0] = '0'
	loaded, err = session.Load(ctx)
	if err != nil || loaded.Scopes["root"].Nodes["n0"].Outputs[0].Data[0] != '1' {
		t.Fatalf("Load retained mutable output: %v", err)
	}
	if err := session.Save(ctx, durable.RunRecord{RunID: "run", Revision: 1}, 0); !errors.Is(err, durable.ErrConflict) {
		t.Fatalf("duplicate creation: %v", err)
	}
	if err := session.Save(ctx, durable.RunRecord{RunID: "run", Revision: 3}, 2); !errors.Is(err, durable.ErrConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wg.Go(func() {
			results <- session.Save(ctx, durable.RunRecord{RunID: "run", Revision: 2, Version: strconv.Itoa(i)}, 1)
		})
	}
	wg.Wait()
	close(results)
	var succeeded, conflicts int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, durable.ErrConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicts != 1 {
		t.Fatalf("CAS results: succeeded=%d conflicts=%d", succeeded, conflicts)
	}
	// 另一 RunID 不被本会话阻塞。
	independent, err := other.Acquire(ctx, "independent")
	if err != nil {
		t.Fatal(err)
	}
	if err := independent.Save(ctx, durable.RunRecord{RunID: "independent", Revision: 1}, 0); err != nil {
		t.Fatal(err)
	}
	if err := independent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Load(ctx); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Load after Close: %v", err)
	}
	if err := session.Save(ctx, record, 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Save after Close: %v", err)
	}
	resumed, err := other.Acquire(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = resumed.Load(ctx)
	if err != nil || loaded.Revision != 2 {
		t.Fatalf("persistent revision: %d %v", loaded.Revision, err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Load(ctx); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("active session after Store.Close: %v", err)
	}
	if _, err := other.Acquire(ctx, "run"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Acquire after Store.Close: %v", err)
	}
	unlocked, err := s.Acquire(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if err := unlocked.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBenchmarkSQLiteSaveAtomicFailure(t *testing.T) {
	ctx := context.Background()
	s, err := newSQLiteStore(filepath.Join(durableTestDir(t), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	session, err := s.Acquire(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Save(ctx, durable.RunRecord{RunID: "run", Revision: 1, Version: "old"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_save BEFORE UPDATE ON durable_runs
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := session.Save(ctx, durable.RunRecord{RunID: "run", Revision: 2, Version: "new"}, 1); err == nil {
		t.Fatal("injected failure did not fail")
	}
	loaded, err := session.Load(ctx)
	if err != nil || loaded.Revision != 1 || loaded.Version != "old" {
		t.Fatalf("partial Save: %+v %v", loaded, err)
	}
}

func TestBenchmarkSQLiteIdentityAndUint64Revision(t *testing.T) {
	ctx := context.Background()
	s, err := newSQLiteStore(filepath.Join(durableTestDir(t), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, record := range []durable.RunRecord{
		{RunID: "wrong", Revision: 1}, {RunID: "run", Revision: 0}, {RunID: "run", Revision: 2},
	} {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO durable_runs(run_id, revision, record) VALUES ('run', '1', ?)
			ON CONFLICT(run_id) DO UPDATE SET record = excluded.record`, data); err != nil {
			t.Fatal(err)
		}
		session, err := s.Acquire(ctx, "run")
		if err != nil {
			t.Fatal(err)
		}
		_, loadErr := session.Load(ctx)
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		// 正式后端将存储身份损坏报告为存储错误，
		// 而不是合法竞争写入所使用的 CAS 冲突哨兵错误。
		if loadErr == nil {
			t.Fatalf("corrupt identity accepted: %+v", record)
		}
	}
	maximum := ^uint64(0)
	record := durable.RunRecord{RunID: "large", Revision: maximum - 1}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO durable_runs(run_id, revision, record) VALUES (?, ?, ?)", "large", strconv.FormatUint(record.Revision, 10), data); err != nil {
		t.Fatal(err)
	}
	session, err := s.Acquire(ctx, "large")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	record.Revision = maximum
	if err := session.Save(ctx, record, maximum-1); err != nil {
		t.Fatal(err)
	}
	if loaded, err := session.Load(ctx); err != nil || loaded.Revision != maximum {
		t.Fatalf("uint64 revision: %d %v", loaded.Revision, err)
	}
	record.Revision = 0
	if err := session.Save(ctx, record, maximum); !errors.Is(err, durable.ErrConflict) {
		t.Fatalf("revision overflow: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Acquire(cancelled, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Acquire: %v", err)
	}
	for _, id := range []string{"", string([]byte{0xff})} {
		if _, err := s.Acquire(ctx, id); err == nil {
			t.Errorf("invalid run ID %q accepted", id)
		}
	}
	if _, err := newSQLiteStore(":memory:"); err == nil {
		t.Fatal("nonpersistent SQLite accepted")
	}
	if _, err := newBlobStore(""); err == nil {
		t.Fatal(fmt.Errorf("empty path accepted"))
	}
}
