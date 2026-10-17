package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Luo-root/pulse"
	durable "github.com/Luo-root/pulse-durable"
)

func init() { adapters["durable"] = buildDurable }

type durableRunner struct {
	graph    *pulseRunner
	runtime  *durable.Runtime
	store    *sqliteStore
	config   Config
	prefix   string
	sequence atomic.Uint64
	audit    *os.File
	auditMu  sync.Mutex
	once     sync.Once
	closeErr error
}

func buildDurable(_ context.Context, g *graphSpec, c Config, p *probe) (runner, error) {
	switch c.Mode {
	case "checkpoint":
	case "recover-prepare", "recover-resume":
		if g.shape != "chain" || c.RunID == "" {
			return nil, errors.New("durable recovery requires a chain and stable run ID")
		}
		if c.Mode == "recover-prepare" && (c.CheckpointAt < 0 || c.CheckpointAt >= len(g.names)) {
			return nil, errors.New("durable recovery checkpoint node is outside the chain")
		}
	default:
		return nil, fmt.Errorf("durable benchmark: unsupported mode %q", c.Mode)
	}
	store, err := newSQLiteStore(c.StorePath)
	if err != nil {
		return nil, err
	}
	r := &durableRunner{store: store, config: c}
	fail := func(err error) (runner, error) { return nil, errors.Join(err, r.close()) }
	// 业务值显式使用 JSON uint64，避免解码到 float64 后截断真实出口。
	codecs := &durable.Codecs{}
	if err := durable.RegisterJSON[uint64](codecs, "benchmark/uint64", "1"); err != nil {
		return fail(err)
	}
	r.runtime, err = durable.New(store, codecs)
	if err != nil {
		return fail(err)
	}
	var before func(context.Context, int) error
	if c.Mode == "checkpoint" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return fail(err)
		}
		r.prefix = c.RunID + "-" + hex.EncodeToString(nonce[:])
	} else {
		r.audit, err = os.OpenFile(c.StorePath+".calls.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return fail(err)
		}
		before = r.beforeRecoveryNode
	}
	r.graph, err = buildPulseGraph(g, c, p, before)
	if err != nil {
		return fail(err)
	}
	return r, nil
}

func (r *durableRunner) beforeRecoveryNode(ctx context.Context, node int) error {
	if r.config.Mode == "recover-prepare" && node == r.config.CheckpointAt {
		// 这里已到真实节点业务入口，前驱已 confirm-before-publish；不增添空任务。
		// 定时器仅保持无 deadline 的独立宿主可等待外部强杀，避免 Go 判为全局死锁。
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 仅恢复验收记录「通过门闩、将进入业务」的节点号。它不代表提交完成，
	// 不逐条 fsync；强杀前须同时读到独立审计前缀与 SQLite 确认前缀。
	data, err := json.Marshal(struct {
		Node int `json:"node"`
	}{Node: node})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	n, err := r.audit.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func (r *durableRunner) run(ctx context.Context) (uint64, error) {
	runID := r.config.RunID
	if r.config.Mode == "checkpoint" {
		runID = fmt.Sprintf("%s-%d", r.prefix, r.sequence.Add(1))
	}
	opts := durable.RunOptions{RunID: runID, Version: fmt.Sprintf("production-427/compute-v1/rounds=%d", r.config.Rounds)}
	var execution *pulse.Execution
	var err error
	if r.config.Mode == "recover-resume" {
		execution, err = r.runtime.Resume(ctx, r.graph.definition, pulse.Inputs{}, opts)
	} else {
		execution, err = r.runtime.Run(ctx, r.graph.definition, pulse.Inputs{}, opts)
	}
	if err != nil {
		return 0, err
	}
	return r.graph.checksum(execution)
}

func (r *durableRunner) close() error {
	r.once.Do(func() {
		if r.audit != nil {
			r.closeErr = r.audit.Close()
		}
		r.closeErr = errors.Join(r.closeErr, r.store.Close())
	})
	return r.closeErr
}

// durableRecovery 供父宿主的 pulse/prepare 与 pulse/resume 命令注册使用；
// 调用方分别设置 Mode=recover-prepare/recover-resume，输出 Engine 仍为 pulse。
// prepare 正常情况不返回，等待宿主强杀；resume 返回真实出口及已确认节点数。
func durableRecovery(ctx context.Context, c Config) (result any, err error) {
	if c.Mode != "recover-prepare" && c.Mode != "recover-resume" {
		return nil, errors.New("durable recovery helper requires a recovery mode")
	}
	g, err := newGraph(c.Shape, c.Nodes)
	if err != nil {
		return nil, err
	}
	built, err := buildDurable(ctx, g, c, nil)
	if err != nil {
		return nil, err
	}
	r := built.(*durableRunner)
	defer func() { err = errors.Join(err, r.close()) }()
	checksum, err := r.run(ctx)
	if err != nil {
		return nil, err
	}
	session, err := r.store.Acquire(ctx, c.RunID)
	if err != nil {
		return nil, err
	}
	record, loadErr := session.Load(ctx)
	if err := errors.Join(loadErr, session.Close()); err != nil {
		return nil, err
	}
	return struct {
		Checksum       uint64 `json:"checksum"`
		ConfirmedNodes int    `json:"confirmed_nodes"`
	}{Checksum: checksum, ConfirmedNodes: len(record.Scopes[record.RootScope].Nodes)}, nil
}
