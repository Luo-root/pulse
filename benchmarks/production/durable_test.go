package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	durable "github.com/Luo-root/pulse-durable"
)

func TestDurableCheckpointGraphs(t *testing.T) {
	for _, shape := range []string{"chain", "wide", "diamonds"} {
		t.Run(shape, func(t *testing.T) {
			g, err := newGraph(shape, 7)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{Shape: shape, Nodes: 7, Rounds: 3, Parallelism: 3, Mode: "checkpoint", RunID: "same-prefix", StorePath: filepath.Join(durableTestDir(t), "runs.db")}
			p := newProbe(c.Nodes)
			built, err := buildDurable(context.Background(), g, c, p)
			if err != nil {
				t.Fatal(err)
			}
			r := built.(*durableRunner)
			defer r.close()
			want, checksum := g.reference(c.Rounds)
			for range 2 {
				got, err := r.run(context.Background())
				if err != nil || got != checksum {
					t.Fatalf("checksum=%d want=%d err=%v", got, checksum, err)
				}
				if err := p.check(want, c.Parallelism); err != nil {
					t.Fatal(err)
				}
				p.reset()
			}
			var runs int
			if err := r.store.db.QueryRow("SELECT count(*) FROM durable_runs").Scan(&runs); err != nil || runs != 2 {
				t.Fatalf("unique RunIDs: runs=%d err=%v", runs, err)
			}
			if _, err := os.Stat(c.StorePath + ".calls.jsonl"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("checkpoint sampling created recovery audit: %v", err)
			}
		})
	}
}

func readDurableRecord(db *sql.DB, runID string) (durable.RunRecord, error) {
	var data []byte
	if err := db.QueryRow("SELECT record FROM durable_runs WHERE run_id = ?", runID).Scan(&data); err != nil {
		return durable.RunRecord{}, err
	}
	var record durable.RunRecord
	err := json.Unmarshal(data, &record)
	return record, err
}

func readDurableCalls(path string) ([]int, error) {
	data, err := os.ReadFile(path + ".calls.jsonl")
	if err != nil {
		return nil, err
	}
	var calls []int
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var call struct {
			Node int `json:"node"`
		}
		if err := json.Unmarshal(line, &call); err != nil {
			return nil, err
		}
		calls = append(calls, call.Node)
	}
	return calls, nil
}

func waitDurablePrefix(t *testing.T, db *sql.DB, c Config) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		record, err := readDurableRecord(db, c.RunID)
		calls, auditErr := readDurableCalls(c.StorePath)
		if err == nil && auditErr == nil && len(record.Scopes[record.RootScope].Nodes) == c.CheckpointAt && len(calls) == c.CheckpointAt {
			for i, node := range calls {
				if node != i {
					t.Fatalf("prepare audit=%v", calls)
				}
			}
			for i := range c.CheckpointAt {
				node, ok := record.Scopes[record.RootScope].Nodes[fmt.Sprintf("n%d", i)]
				if !ok || record.Attempts[node.Attempt].State != durable.AttemptConfirmed {
					t.Fatalf("prefix node %d was not confirmed", i)
				}
			}
			return
		}
		lastErr = errors.Join(err, auditErr)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("confirmed/audited prefix did not reach %d: %v", c.CheckpointAt, lastErr)
}

func TestDurableRecoveryCancelledPrefix(t *testing.T) {
	c := Config{Shape: "chain", Nodes: 6, Rounds: 4, Parallelism: 2, Mode: "recover-prepare", CheckpointAt: 3, RunID: "stable", StorePath: filepath.Join(durableTestDir(t), "recovery.db")}
	g, err := newGraph(c.Shape, c.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	p := newProbe(c.Nodes)
	built, err := buildDurable(context.Background(), g, c, p)
	if err != nil {
		t.Fatal(err)
	}
	r := built.(*durableRunner)
	defer r.close()
	reader, err := newSQLiteStore(c.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := r.run(ctx); done <- err }()
	waitDurablePrefix(t, reader.db, c)
	if p.counts[c.CheckpointAt].Load() != 0 {
		t.Fatal("blocked node entered business")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("prepare cancellation: %v", err)
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	c.Mode = "recover-resume"
	g, err = newGraph(c.Shape, c.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	resumedProbe := newProbe(c.Nodes)
	built, err = buildDurable(context.Background(), g, c, resumedProbe)
	if err != nil {
		t.Fatal(err)
	}
	defer built.close()
	want, checksum := g.reference(c.Rounds)
	if got, err := built.run(context.Background()); err != nil || got != checksum {
		t.Fatalf("resumed checksum=%d want=%d err=%v", got, checksum, err)
	}
	for i := range c.Nodes {
		count := int32(1)
		if i < c.CheckpointAt {
			count = 0
		}
		if got := resumedProbe.counts[i].Load(); got != count {
			t.Errorf("resumed node %d ran %d times, want %d", i, got, count)
		}
		if count == 1 && resumedProbe.values[i].Load() != want[i] {
			t.Errorf("resumed node %d used wrong predecessor value", i)
		}
	}
	calls, err := readDurableCalls(c.StorePath)
	if err != nil || fmt.Sprint(calls) != "[0 1 2 3 4 5]" {
		t.Fatalf("complete recovery audit=%v err=%v", calls, err)
	}
	// 已确认结果再次 Resume 只读取原记录，不再执行任何业务。
	if _, err := durableRecovery(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	calls, err = readDurableCalls(c.StorePath)
	if err != nil || len(calls) != c.Nodes {
		t.Fatalf("completed Resume re-executed: %v %v", calls, err)
	}
	c.Rounds++
	if _, err := durableRecovery(context.Background(), c); !errors.Is(err, durable.ErrConflict) {
		t.Fatalf("changed business version accepted: %v", err)
	}
}

func TestDurableRecoveryProcessExit(t *testing.T) {
	path := filepath.Join(durableTestDir(t), "process.db")
	c := Config{Shape: "chain", Nodes: 6, Rounds: 4, Parallelism: 2, CheckpointAt: 3, RunID: "process-stable", StorePath: path}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := func(mode string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestDurableRecoveryChildProcess$")
		cmd.Env = append(os.Environ(), "PULSE_DURABLE_TEST_CHILD="+mode, "PULSE_DURABLE_TEST_STORE="+path)
		return cmd
	}
	prepare := command("recover-prepare")
	var prepareOutput bytes.Buffer
	prepare.Stdout, prepare.Stderr = &prepareOutput, &prepareOutput
	if err := prepare.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if prepare.ProcessState == nil {
			_ = prepare.Process.Kill()
			_ = prepare.Wait()
		}
	}()
	// 读取连接在子进程启动后打开，走与父 Python sqlite3 同样的独立 WAL 读取路径。
	reader, err := newSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	waitDurablePrefix(t, reader.db, c)
	if _, err := reader.Acquire(context.Background(), c.RunID); !errors.Is(err, durable.ErrBusy) {
		t.Fatalf("cross-process ownership: %v", err)
	}
	if err := prepare.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := prepare.Wait(); err == nil {
		t.Fatalf("prepare was not forcibly terminated: %s", prepareOutput.String())
	}
	// 无需清理锁文件或等租约过期，进程退出后立即重新取得同一 RunID。
	session, err := reader.Acquire(context.Background(), c.RunID)
	if err != nil {
		t.Fatalf("ownership survived process exit: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if output, err := command("recover-resume").CombinedOutput(); err != nil {
		t.Fatalf("resume child: %v\n%s", err, output)
	}
	record, err := readDurableRecord(reader.db, c.RunID)
	if err != nil || len(record.Scopes[record.RootScope].Nodes) != c.Nodes {
		t.Fatalf("resume confirmed nodes: %+v %v", record, err)
	}
	calls, err := readDurableCalls(path)
	if err != nil || fmt.Sprint(calls) != "[0 1 2 3 4 5]" {
		t.Fatalf("process-exit audit=%v err=%v", calls, err)
	}
}

func TestDurableRecoveryChildProcess(t *testing.T) {
	mode := os.Getenv("PULSE_DURABLE_TEST_CHILD")
	if mode == "" {
		t.Skip("only the isolated process-exit test starts this helper")
	}
	path := os.Getenv("PULSE_DURABLE_TEST_STORE")
	if path == "" || !strings.Contains(filepath.ToSlash(path), "/validation-bench/") {
		t.Fatal("child storage is outside the isolated validation directory")
	}
	c := Config{Shape: "chain", Nodes: 6, Rounds: 4, Parallelism: 2, Mode: mode, CheckpointAt: 3, RunID: "process-stable", StorePath: path}
	result, err := durableRecovery(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "recover-resume" {
		t.Fatal("prepare returned before forced termination")
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Checksum       uint64 `json:"checksum"`
		ConfirmedNodes int    `json:"confirmed_nodes"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	g, err := newGraph(c.Shape, c.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	_, checksum := g.reference(c.Rounds)
	if got.Checksum != checksum || got.ConfirmedNodes != c.Nodes {
		t.Fatalf("recovery result=%s want checksum=%d nodes=%d", data, checksum, c.Nodes)
	}
}
