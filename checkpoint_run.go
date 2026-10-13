package pulse

import (
	"errors"
	"fmt"
	"sync"
)

// CheckpointError 是可靠钩子或恢复协议的失败，不是正常业务跳过。
// errors.Is/As 保留原原因，即使原原因含 ErrSkipped，引擎仍按失败处理。
type CheckpointError struct {
	Operation string
	Err       error
}

func (e *CheckpointError) Error() string {
	return fmt.Sprintf("pulse: checkpoint %s: %v", e.Operation, e.Err)
}
func (e *CheckpointError) Unwrap() error { return e.Err }

func isCheckpointFailure(err error) bool {
	var failure *CheckpointError
	return errors.As(err, &failure)
}

type checkpointConfig struct {
	scope   string
	hooks   CheckpointHooks
	failure *checkpointFailure
}

// 整棵执行树共享可靠失败，不受 Aspect 的错误吞并、Retry 或子图取消域截断。
type checkpointFailure struct {
	mu   sync.Mutex
	err  error
	root *Execution
}

func (e *Execution) checkpointErr() error {
	if e.checkpoint == nil {
		return nil
	}
	f := e.checkpoint.failure
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (e *Execution) stopCheckpoint(op string, err error) error {
	f := e.checkpoint.failure
	f.mu.Lock()
	if f.err == nil {
		f.err = &CheckpointError{Operation: op, Err: err}
	}
	failure := f.err
	f.mu.Unlock()
	f.root.fail(failure)
	return failure
}

// 可靠调用必须同步结束；panic 与不确定错误都锁存，不自动重试、不调用回滚钩子。
func (e *Execution) callCheckpoint(op string, fn func() error) (err error) {
	if err := e.checkpointErr(); err != nil {
		return err
	}
	defer func() {
		if rec := recover(); rec != nil {
			err = e.stopCheckpoint(op, fmt.Errorf("hook panicked (%T)", rec))
		}
	}()
	if err = fn(); err != nil {
		return e.stopCheckpoint(op, err)
	}
	return e.checkpointErr()
}

// checkCheckpointLocked 在封口前递归检查全部声明，不在锁内运行钩子。
func (g *Graph) checkCheckpointLocked(c *checkpointConfig, chain mountChain) error {
	if c.scope == "" || c.hooks.OpenAttempt == nil || c.hooks.FailAttempt == nil || c.hooks.OpenChild == nil || c.hooks.Commit == nil {
		return fmt.Errorf("pulse: checkpoint mode requires a scope and all four hooks")
	}
	for _, ref := range g.inPortOrder {
		if checkpointChannelType(ref.typ, nil) {
			return fmt.Errorf("pulse: checkpoint mode does not support channel input %q", ref.name)
		}
	}
	for _, ref := range g.outPortOrder {
		if checkpointChannelType(ref.typ, nil) {
			return fmt.Errorf("pulse: checkpoint mode does not support channel output %q", ref.name)
		}
	}
	for _, n := range g.nodes {
		if n.streamKind != "" {
			return fmt.Errorf("pulse: checkpoint mode does not support stream node %q", n.id)
		}
		for _, ref := range append(append([]keyRef(nil), n.requires...), n.provides...) {
			if checkpointChannelType(ref.typ, nil) {
				return fmt.Errorf("pulse: checkpoint node %q does not support channel key %q", n.id, ref.name)
			}
		}
		if n.child != nil && !chain.has(n.child) {
			n.child.mu.Lock()
			err := n.child.checkCheckpointLocked(c, chain.push(n.child))
			n.child.mu.Unlock()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCheckpointInputsLocked 检查本次已经收到的动态值；接口键里也不能藏 channel。
// 调用方持 Graph.mu；槽位只读，不调用 Codec 或宿主代码。
func (e *Execution) checkCheckpointInputsLocked() error {
	for _, ref := range e.graph.inPortOrder {
		if _, supplied := e.seeded[ref.name]; !supplied {
			continue
		}
		e.slotsMu.Lock()
		s := e.slots[ref.name]
		e.slotsMu.Unlock()
		st, value := s.snapshot()
		if err := checkCheckpointOutput(CheckpointOutput{Key: CheckpointKey{Name: ref.name, Type: ref.typ}, State: publicSlotState(st), Value: value}); err != nil {
			return fmt.Errorf("pulse: checkpoint input: %w", err)
		}
	}
	return nil
}

// applyRestoreLocked 在全量验证以后灌入；输入不能占输出，且每条输出只有一个节点来源。
// 调用方持装配锁、Graph.mu 和 Execution.mu，还未发任何事件或提交 goroutine。
func (e *Execution) applyRestoreLocked() {
	e.restored = make(map[*Node]NodeCheckpoint, len(e.restore))
	nodes := make(map[string]*Node, len(e.graph.nodes))
	for _, n := range e.graph.nodes {
		nodes[n.id] = n
	}
	for _, r := range e.restore {
		e.restored[nodes[r.NodeID]] = r
		for _, o := range r.Outputs {
			s := e.slotOf(keyRef{name: o.Key.Name, typ: o.Key.Type})
			if o.State == SlotStateSkipped {
				_ = s.resolveSkip()
			} else {
				_ = s.resolveValue(o.Value)
			}
		}
	}
}

func (rc *RunCtx) openCheckpointAttempt(parent, boundary string, limit int) (CheckpointAttempt, error) {
	var result CheckpointAttempt
	if err := rc.ctx.Err(); err != nil {
		return result, err
	}
	err := rc.exec.callCheckpoint("open attempt", func() error {
		var err error
		result, err = rc.exec.checkpoint.hooks.OpenAttempt(rc.ctx, CheckpointAttemptRequest{
			Site: rc.exec.site, Parent: parent, NodeID: rc.node.id, Boundary: boundary, Limit: limit,
		})
		if err != nil {
			return err
		}
		if result.Scope == "" || result.Scope == parent || result.Index < 1 || result.Index > limit || (result.Exhausted && result.Index != limit) {
			return fmt.Errorf("invalid persistent attempt cursor")
		}
		return nil
	})
	return result, err
}

func (rc *RunCtx) failCheckpointAttempt(scope string) error {
	return rc.exec.callCheckpoint("fail attempt", func() error {
		return rc.exec.checkpoint.hooks.FailAttempt(rc.ctx, scope)
	})
}

// 去重的是 Retry 切面的入口，不是入口内部的多次 OpenAttempt。
type checkpointEntryLedger struct {
	mu   sync.Mutex
	seen map[checkpointEntry]bool
}

type checkpointEntry struct {
	parent string
	index  int
}

func (rc *RunCtx) enterCheckpointRetry() error {
	le := rc.checkpointEntries
	le.mu.Lock()
	defer le.mu.Unlock()
	key := checkpointEntry{parent: rc.checkpointAttempt.Scope, index: rc.aspectIndex}
	if le.seen[key] {
		return rc.exec.stopCheckpoint("retry boundary", fmt.Errorf("node %q re-entered the same Retry boundary without a new parent attempt", rc.node.id))
	}
	if le.seen == nil {
		le.seen = make(map[checkpointEntry]bool)
	}
	le.seen[key] = true
	return nil
}

func invokeCheckpointChain(chain func(*RunCtx) error, rc *RunCtx) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("pulse: panic in node chain %s: %v", rc.NodeID(), rec)
		}
	}()
	return chain(rc)
}

func (e *Execution) runCheckpointChain(n *Node, rc *RunCtx, chain func(*RunCtx) error) error {
	root, err := rc.openCheckpointAttempt(e.checkpoint.scope, "", 1)
	if err != nil {
		return err
	}
	if root.Exhausted {
		return fmt.Errorf("pulse: checkpoint node %q already failed; start a new durable run for a new retry budget", n.id)
	}
	rc.checkpointAttempt = root
	rc.checkpointEntries = &checkpointEntryLedger{}
	ws := &writeSet{}
	rc.writes = ws
	err = invokeCheckpointChain(chain, rc)
	if failure := e.checkpointErr(); failure != nil {
		err = failure
	}
	if (err == nil || isSkipped(err)) && rc.ctx.Err() != nil {
		err = rc.ctx.Err()
	}
	if err != nil && !isSkipped(err) {
		// 在 fail 取消执行域之前区分外部中断与最终真实失败。
		if !isCheckpointFailure(err) && e.ctx.Err() == nil {
			if ferr := rc.failCheckpointAttempt(root.Scope); ferr != nil {
				return ferr
			}
		}
		return err
	}
	// 切面即使短路业务体，成功检查点也等声明依赖到达，保证恢复集合依赖闭合。
	if _, werr := awaitAll(rc, n.requires); werr != nil {
		return werr
	}
	e.settleUnwritten(n, rc)
	r := NodeCheckpoint{NodeID: n.id, Reason: finishReason(err), Outputs: make([]CheckpointOutput, 0, len(n.provides))}
	for _, ref := range n.provides {
		st, value := ws.peek(ref).snapshot()
		o := CheckpointOutput{Key: CheckpointKey{Name: ref.name, Type: ref.typ}, State: publicSlotState(st), Value: value}
		if cerr := checkCheckpointOutput(o); cerr != nil {
			return e.stopCheckpoint("finalize output", cerr)
		}
		if cerr := checkCommitTarget(e, ws, ref, st); cerr != nil {
			return e.stopCheckpoint("publish preflight", cerr)
		}
		r.Outputs = append(r.Outputs, o)
	}
	if cerr := rc.ctx.Err(); cerr != nil {
		return cerr
	}
	if cerr := e.callCheckpoint("commit", func() error {
		return e.checkpoint.hooks.Commit(rc.ctx, CheckpointCommit{Site: e.site, Scope: e.checkpoint.scope, Attempt: root.Scope, Node: r})
	}); cerr != nil {
		return cerr
	}
	if cerr := ws.commit(e); cerr != nil {
		return e.stopCheckpoint("publish", cerr)
	}
	return err
}

func (rc *RunCtx) prepareChildCheckpoint(sub *Execution) error {
	if rc.exec.checkpoint == nil {
		return nil
	}
	c := *rc.exec.checkpoint
	c.scope = rc.checkpointAttempt.Scope
	sub.checkpoint = &c
	var records []NodeCheckpoint
	if err := rc.exec.callCheckpoint("open child", func() error {
		var err error
		records, err = c.hooks.OpenChild(rc.ctx, CheckpointChild{Site: sub.site, Attempt: c.scope, NodeID: rc.node.id, Graph: sub.site.Graph, Inputs: sub.CheckpointInputs()})
		return err
	}); err != nil {
		return err
	}
	if err := sub.RestoreNodes(records); err != nil {
		return rc.exec.stopCheckpoint("restore child", err)
	}
	return nil
}
