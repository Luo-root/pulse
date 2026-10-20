package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"
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
	if err == nil {
		return false
	}
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
	mu      sync.Mutex
	err     error
	root    *Execution
	streams map[CheckpointStreamID]*checkpointStreamCache // 索引只用逻辑身份，Generation 由权威记录核验。
	values  map[*writeSet]map[string]bool                 // 完整节点帧中已随消息确认的普通键。
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
	f.root.Cancel() // 可靠失败终止整树（含已接纳 I/O），不能留下只等业务链之外的钩子。
	return failure
}

// 接纳前检查业务域；接纳后整段只受同层外部控制影响。内部 fail-fast 仍会立即
// 唤醒业务等待，但不能把已开始的可靠保存变成不确定结果。钩子同步结束，错误照常锁存。
func (e *Execution) callCheckpoint(ctx, control context.Context, op string, fn func(context.Context) error) (err error) {
	if err := e.checkpointErr(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return &executionCancellation{cause: err}
	}
	if err := control.Err(); err != nil {
		return &executionCancellation{cause: err}
	}
	returned := false
	defer func() {
		if rec := recover(); !returned {
			// 不能凭 recover() != nil 判断：panicnil=1 时 panic(nil) 也未正常返回。
			err = e.stopCheckpoint(op, fmt.Errorf("hook panicked (%T)", rec))
		}
	}()
	err = fn(control)
	returned = true
	if err != nil {
		if failure := e.checkpointErr(); failure != nil {
			return failure
		}
		if stopped, ok := err.(*CheckpointNotStarted); ok && stopped != nil {
			// 同一父取消传播到两条链没有顺序保证；只认本次适用域的取消事实。
			cause := ctx.Err()
			if cause == nil {
				cause = control.Err()
			}
			if cause != nil {
				return &executionCancellation{cause: cause}
			}
		}
		return e.stopCheckpoint(op, err)
	}
	return e.checkpointErr()
}

// checkCheckpointLocked 在封口前递归检查全部声明，不在锁内运行钩子。
func (g *Graph) checkCheckpointLocked(c *checkpointConfig, chain mountChain) error {
	if c.scope == "" || c.hooks.OpenAttempt == nil || c.hooks.FailAttempt == nil || c.hooks.OpenChild == nil || c.hooks.Commit == nil {
		return fmt.Errorf("pulse: checkpoint mode requires a scope and all four hooks")
	}
	if c.hooks.StreamHooks != nil && !c.hooks.StreamHooks.complete() {
		return fmt.Errorf("pulse: checkpoint StreamHooks requires all six hooks")
	}
	for _, ref := range append(append([]keyRef(nil), g.inPortOrder...), g.outPortOrder...) {
		if err := checkCheckpointDeclaration(c, ref); err != nil {
			return err
		}
	}
	for _, n := range g.nodes {
		for _, ref := range append(append([]keyRef(nil), n.requires...), n.provides...) {
			if err := checkCheckpointDeclaration(c, ref); err != nil {
				return fmt.Errorf("pulse: checkpoint node %q: %w", n.id, err)
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
		if st == slotPending { // 有效 In 绑定尚未到达，不是一个待保存的成果。
			continue
		}
		if err := checkCheckpointOutput(checkpointOutput(ref, st, value)); err != nil {
			return fmt.Errorf("pulse: checkpoint input: %w", err)
		}
	}
	return nil
}

// applyRestoreLocked 在全量验证以后灌入；输入不能占输出，且每条输出只有一个节点来源。
// 调用方持装配锁、Graph.mu 和 Execution.mu，还未发任何事件或提交 goroutine。
func (e *Execution) applyRestoreLocked() error {
	// 本作用域已整批预检；跨作用域缓存也须先整批核验，失败不能安装任何槽位
	// 或改变已有端点。树锁保护缓存和 Open 结果安装，不在这里调用可靠钩子。
	f := e.checkpoint.failure
	f.mu.Lock()
	defer f.mu.Unlock()
	descriptors := make(map[CheckpointStreamID]CheckpointStreamDescriptor)
	var order []CheckpointStreamID
	for _, r := range e.restore {
		for _, o := range r.Outputs {
			if o.Stream != nil {
				descriptor := *o.Stream
				logical := checkpointStreamLogicalID(descriptor.ID)
				if previous, exists := descriptors[logical]; exists {
					descriptor, _ = mergeCheckpointStreamReference(previous, descriptor)
				} else {
					order = append(order, logical)
				}
				descriptors[logical] = descriptor
			}
		}
	}
	for _, logical := range order {
		if cached := f.streams[logical]; cached != nil {
			descriptor := descriptors[logical]
			if cached.opened {
				if err := checkCheckpointStreamReference(*cached.endpoint.pulseCheckpointDescriptor(), descriptor, cached.resumed); err != nil {
					return err
				}
			} else if cached.hasReference {
				var err error
				descriptors[logical], err = mergeCheckpointStreamConstraints(cached.reference, descriptor)
				if err != nil {
					return err
				}
			} else if !sameCheckpointStreamSource(*cached.endpoint.pulseCheckpointDescriptor(), descriptor) {
				return fmt.Errorf("checkpoint stream %q reference names a different pending source", descriptor.ID.Key)
			}
		}
	}
	if len(order) != 0 && f.streams == nil {
		f.streams = make(map[CheckpointStreamID]*checkpointStreamCache)
	}
	for _, logical := range order {
		if cached := f.streams[logical]; cached == nil {
			f.streams[logical] = &checkpointStreamCache{reference: descriptors[logical], hasReference: true}
		} else if !cached.opened {
			cached.reference, cached.hasReference = descriptors[logical], true
		}
	}
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
			} else if o.Stream != nil {
				cached := f.streams[checkpointStreamLogicalID(o.Stream.ID)]
				if cached.endpoint == nil {
					factory := reflect.Zero(o.Key.Type).Interface().(checkpointStreamFactory)
					cached.endpoint = factory.pulseRestoreCheckpoint(e, descriptors[checkpointStreamLogicalID(o.Stream.ID)], cached).(checkpointEndpoint)
				}
				_ = s.resolveValue(cached.endpoint)
			} else {
				// 全量预检已确认可赋值；安装仍须落成键的声明类型。
				value, _ := valueForType(reflect.ValueOf(o.Value), o.Key.Type)
				_ = s.resolveValue(value.Interface())
			}
		}
	}
	return nil
}

func (rc *RunCtx) openCheckpointAttempt(parent, boundary string, limit int) (CheckpointAttempt, error) {
	var result CheckpointAttempt
	if err := rc.ctx.Err(); err != nil {
		return result, err
	}
	err := rc.exec.callCheckpoint(rc.ctx, rc.controlCtx, "open attempt", func(ctx context.Context) error {
		var err error
		result, err = rc.exec.checkpoint.hooks.OpenAttempt(ctx, CheckpointAttemptRequest{
			Site: rc.exec.site, Parent: parent, NodeID: rc.node.id, Boundary: boundary, Limit: limit,
		})
		if err != nil {
			return err
		}
		if result.Scope == "" || result.Scope == parent || result.Index < 1 || result.Index > limit || (result.Exhausted && result.Index != limit) {
			return fmt.Errorf("invalid persistent attempt cursor")
		}
		if len(result.Values) != 0 && (boundary != "" || result.Exhausted) {
			return fmt.Errorf("only an active complete-chain attempt may restore message values")
		}
		if err := checkCheckpointAttemptValues(rc.node, result.Values); err != nil {
			return err
		}
		if result.Resumed != nil {
			resumed := *result.Resumed
			result.Resumed = &resumed
		}
		result.Values = append([]CheckpointOutput(nil), result.Values...)
		for i := range result.Values {
			output := &result.Values[i]
			if output.State == SlotStateReady {
				value, _ := valueForType(reflect.ValueOf(output.Value), output.Key.Type)
				output.Value = value.Interface()
			}
		}
		return nil
	})
	return result, err
}

func (rc *RunCtx) failCheckpointAttempt(scope string) error {
	return rc.exec.callCheckpoint(rc.exec.ctx, rc.exec.controlCtx, "fail attempt", func(ctx context.Context) error {
		// 业务清理取消节点 ctx 不应阻断失败确认；仍保留所属执行的父层控制。
		return rc.exec.checkpoint.hooks.FailAttempt(ctx, scope)
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

func invokeCheckpointChain(chain func(*RunCtx) error, rc *RunCtx) error {
	// 与业务体/切面共用正常返回判据，也保留 panic 的失败身份。
	return invokeNode(rc, chain)
}

// 非空 confirmed 是已经可靠确认、发布并完成流安装的成功结论；调用方只复用它，
// 不再运行会重新遍历用户 error 的通用收尾。其余路径仍走完整的失败/中断收尾。
func (e *Execution) runCheckpointChain(n *Node, rc *RunCtx, chain func(*RunCtx) error) (err error, confirmed NodeFinishReason) {
	root, err := rc.openCheckpointAttempt(e.checkpoint.scope, "", 1)
	if err != nil {
		return err, ""
	}
	if root.Exhausted {
		return fmt.Errorf("pulse: checkpoint node %q already failed; start a new durable run for a new retry budget", n.id), ""
	}
	rc.checkpointAttempt = root
	rc.checkpointEntries = &checkpointEntryLedger{}
	ws := &writeSet{}
	for _, output := range root.Values {
		ref := keyRef{name: output.Key.Name, typ: output.Key.Type}
		if output.State == SlotStateSkipped {
			_ = ws.slotOf(ref).resolveSkip()
		} else {
			_ = ws.slotOf(ref).resolveValue(output.Value)
		}
	}
	rc.writes, rc.checkpointWrites = ws, ws
	rc.confirmCheckpointValues(root.Values)
	defer rc.forgetCheckpointValues()
	err = e.classifyFailure(invokeCheckpointChain(chain, rc))
	returned, failed := false, false
	err = inspectNodeError(func() error {
		result := e.finishCheckpointChain(n, rc, ws, root, err, &failed, &confirmed)
		returned = true
		return result
	})
	if !returned {
		// 实际收尾检查异常后，以不再关联原 error 的失败确认完整节点根预算。
		// 已确认的失败不重复确认；可靠钩子的不确定结果仍由树闩立即截断。
		err = inspectNodeError(func() error { return e.finishCheckpointChain(n, rc, ws, root, err, &failed, &confirmed) })
	}
	return err, confirmed
}

func (e *Execution) finishCheckpointChain(n *Node, rc *RunCtx, ws *writeSet, root CheckpointAttempt, err error, failed *bool, confirmed *NodeFinishReason) error {
	err = checkpointChainResult(rc, err)
	if failure := e.checkpointErr(); failure != nil {
		err = failure
	}
	err, reason := e.classifyResult(err)
	normal := reason == NodeCompleted || reason == NodeSkipped
	if normal && rc.ctx.Err() != nil {
		// 本地清理没有形成业务失败；不再提交，也不能确认失败耗掉根预算。
		err = &executionCancellation{cause: rc.ctx.Err()}
	}
	if isCheckpointFailure(err) || isExecutionCancellation(err) || e.ctx.Err() != nil {
		return err // 可靠失败或执行中断不再尝试新的确认。
	}
	finishRC := rc
	if !normal {
		// 节点局部清理不能阻断真实失败确认，取门与 Finish 共用执行取消域。
		copy := *rc
		copy.ctx = e.ctx
		finishRC = &copy
	}
	unlock, lockErr := lockCheckpointStreamFinishes(finishRC)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	if normal && rc.streams != nil {
		// 等操作门期间读取组可能已经失败；持门后复核，避免把旧的成功采样
		// 确认后再让 finish 从 sendErr 改判。业务体已退出，不再有本节点写入。
		rc.streams.mu.Lock()
		outputs := append([]streamLifecycle(nil), rc.streams.outputs...)
		rc.streams.mu.Unlock()
		for _, output := range outputs {
			if cause := output.failure(); cause != nil {
				err, reason = e.classifyResult(cause)
				normal = reason == NodeCompleted || reason == NodeSkipped
				if !normal {
					break
				}
			}
		}
	}
	// 取门后的复核可能取得新原因；取消和可靠失败仍不能落入业务失败确认。
	if isCheckpointFailure(err) || isExecutionCancellation(err) || e.ctx.Err() != nil {
		return err
	}
	if !normal {
		// 在 fail 取消执行域之前区分外部中断与最终真实失败。
		if !isCheckpointFailure(err) && e.ctx.Err() == nil {
			for _, terminal := range checkpointFinishes(rc, err) {
				if ferr := e.callCheckpoint(e.ctx, e.controlCtx, "finish stream", func(ctx context.Context) error {
					return e.checkpoint.hooks.StreamHooks.Finish(ctx, terminal)
				}); ferr != nil {
					return ferr
				}
			}
			if !*failed {
				if ferr := rc.failCheckpointAttempt(root.Scope); ferr != nil {
					return ferr
				}
				*failed = true
			}
			installCheckpointStreamFinishes(rc, err)
		}
		return err
	}
	// 普通节点保持恢复集合依赖闭合；Sub 的 In 按绑定来源核验，不整体等待入口。
	if !n.sub {
		if _, werr := awaitAll(rc, n.requires); werr != nil {
			return werr
		}
	}
	e.settleUnwritten(n, rc)
	// normal 已包含检查过的读取义务、消息失败和输出失败；成功流只需安装完成/停止，
	// 无须在可靠确认之后再检查节点的原始 skip。节点事件仍保留原 error 身份。
	terminals := checkpointFinishes(rc, nil)
	r := NodeCheckpoint{NodeID: n.id, Reason: reason, Outputs: make([]CheckpointOutput, 0, len(n.provides))}
	for _, ref := range n.provides {
		st, value := rc.readTarget(ref).snapshot()
		o := checkpointOutput(ref, st, value)
		if o.Stream != nil {
			for _, terminal := range terminals {
				if terminal.Stream == o.Stream.ID {
					o.Stream.Terminal = terminal.Terminal
				}
			}
		}
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
	if cerr := e.callCheckpoint(rc.ctx, rc.controlCtx, "commit", func(ctx context.Context) error {
		return e.checkpoint.hooks.Commit(ctx, CheckpointCommit{Site: e.site, Scope: e.checkpoint.scope, Attempt: root.Scope, Node: r, Streams: terminals})
	}); cerr != nil {
		return cerr
	}
	if cerr := ws.commit(e); cerr != nil {
		return e.stopCheckpoint("publish", cerr)
	}
	installCheckpointStreamFinishes(rc, nil)
	*confirmed = reason
	return err
}

func (rc *RunCtx) prepareChildCheckpoint(sub *Execution) error {
	if rc.exec.checkpoint == nil {
		return nil
	}
	c := *rc.exec.checkpoint
	c.scope = rc.checkpointAttempt.Scope
	sub.checkpoint = &c
	request := CheckpointChild{Site: sub.site, Attempt: c.scope, NodeID: rc.node.id, Graph: sub.site.Graph}
	for _, bind := range rc.node.binds {
		if bind.fix != nil {
			for _, ref := range bind.fixKeys() {
				entry := bind.fix.in.entries[ref.name]
				state := slotReady
				if entry.skip {
					state = slotSkipped
				}
				output := checkpointOutput(ref, state, entry.value)
				if err := checkCheckpointOutput(output); err != nil {
					return rc.exec.stopCheckpoint("child fixed input", err)
				}
				request.Inputs = append(request.Inputs, output)
			}
		} else if bind.in {
			request.Bindings = append(request.Bindings, CheckpointBinding{Source: rc.exec.site,
				Scope: rc.exec.checkpoint.scope, Attempt: c.scope, Parent: checkpointKey(bind.parent), Child: checkpointKey(bind.child)})
			if state, value := rc.readTarget(bind.parent).snapshot(); state != slotPending {
				request.Inputs = append(request.Inputs, checkpointOutput(bind.child, state, value))
			}
		}
	}
	sort.Slice(request.Inputs, func(i, j int) bool { return request.Inputs[i].Key.Name < request.Inputs[j].Key.Name })
	var records []NodeCheckpoint
	var scopeFailure *CheckpointScopeFailure
	if err := rc.exec.callCheckpoint(rc.ctx, rc.controlCtx, "open child", func(ctx context.Context) error {
		var err error
		records, err = c.hooks.OpenChild(ctx, request)
		// 只有本次请求的精确信号表示已确认业务失败；普通/包装错误与
		// panic 仍由 callCheckpoint 锁存为可靠故障，不能让 Retry 吞掉。
		if failure, ok := err.(*CheckpointScopeFailure); ok && failure != nil &&
			failure.Scope != "" && failure.Scope == request.Attempt && len(records) == 0 {
			scopeFailure = failure
			return nil
		}
		return err
	}); err != nil {
		return err
	}
	if scopeFailure != nil {
		return scopeFailure // 父完整链作业务决策；不安装或运行这张已失败子图。
	}
	if err := sub.RestoreNodes(records); err != nil {
		return rc.exec.stopCheckpoint("restore child", err)
	}
	return nil
}

// retryCheckpoint 使用 Store 的活跃游标，不按本次调用循环的次数重建历史身份。
// 成功的内层尝试保持可认回，直到完整节点检查点确认；写集仍只交给外层写集。
func retryCheckpoint(rc *RunCtx, next func(*RunCtx) error, attempts int, delay time.Duration, o aspectObservation) error {
	if err := rc.enterCheckpointRetry(); err != nil {
		return err
	}
	parent := rc.checkpointAttempt
	boundary := fmt.Sprintf("aspect/%d", rc.aspectIndex)
	lastIndex := 0
	for {
		attempt, err := rc.openCheckpointAttempt(parent.Scope, boundary, attempts)
		if err != nil {
			return err
		}
		if attempt.Exhausted {
			return fmt.Errorf("pulse: checkpoint Retry on node %q exhausted after %d confirmed attempts", rc.NodeID(), attempts)
		}
		previous := rc.writes
		ws := &writeSet{outer: previous}
		rc.writes, rc.checkpointAttempt = ws, attempt
		if o.obs != nil {
			rc.aspectAttempt = attempt.Index
		}
		// 新桥以真实分配标记判定；旧桥只有本次调用已经确认失败并推进时可证明是重试。
		if attempt.Index > 1 && ((attempt.Resumed != nil && !*attempt.Resumed) ||
			(attempt.Resumed == nil && lastIndex > 0 && attempt.Index > lastIndex)) {
			o.attempt(AspectRetrying, attempt, parent.Scope, nil, time.Time{})
		}
		start := time.Now()
		o.attempt(AspectAttemptStarted, attempt, parent.Scope, nil, time.Time{})
		err = invokeCheckpointChain(next, rc)
		rc.writes, rc.checkpointAttempt = previous, parent
		if failure := rc.exec.checkpointErr(); failure != nil {
			err = failure
		}
		if (err == nil || isSkipped(err)) && rc.ctx.Err() != nil {
			err = &executionCancellation{cause: rc.ctx.Err()}
		}
		if err == nil || isSkipped(err) {
			if cerr := ws.commit(rc.exec); cerr != nil {
				err = cerr
			}
			return o.attempt(AspectAttemptFinished, attempt, parent.Scope, err, start)
		}
		if isCheckpointFailure(err) || isExecutionCancellation(err) || isErrorInspectionFailure(err) || rc.ctx.Err() != nil {
			return o.attempt(AspectAttemptFinished, attempt, parent.Scope, err, start)
		}
		// 确认失败先于下一轮；可靠失败本身也作为本次尝试的失败结果报告。
		if ferr := rc.failCheckpointAttempt(attempt.Scope); ferr != nil {
			return o.attempt(AspectAttemptFinished, attempt, parent.Scope, ferr, start)
		}
		err = o.attempt(AspectAttemptFinished, attempt, parent.Scope, err, start)
		if isErrorInspectionFailure(err) || attempt.Index == attempts {
			return err
		}
		lastIndex = attempt.Index
		if werr := o.backoff(rc, attempt, parent.Scope, delay); werr != nil {
			return werr
		}
	}
}

// 类型检查管声明，实际值检查穿透 interface。沿引用记身份，避免自指 map/slice/指针无限递归。
// 不调用 Interface、用户方法或 Codec，也不复制业务对象；与 Set 的值一样，遍历期间不能并发改值。
type checkpointValueVisit struct {
	typ    reflect.Type
	ptr    uintptr
	length int
}

func checkpointChannelValue(v reflect.Value, seen map[checkpointValueVisit]bool) bool {
	if !v.IsValid() {
		return false
	}
	if v.Kind() == reflect.Chan {
		return true
	}
	if v.Kind() == reflect.Interface {
		return !v.IsNil() && checkpointChannelValue(v.Elem(), seen)
	}
	var visit checkpointValueVisit
	switch v.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		if v.IsNil() {
			return false
		}
		visit.typ = v.Type()
		if v.Kind() == reflect.Map {
			visit.ptr = uintptr(v.UnsafePointer())
		} else {
			visit.ptr = v.Pointer()
		}
		if v.Kind() == reflect.Slice {
			visit.length = v.Len() // 同一底层数组的不同长度视图不能互相遮住后半段。
		}
		if seen[visit] {
			return false
		}
		if seen == nil {
			seen = make(map[checkpointValueVisit]bool)
		}
		seen[visit] = true
	}
	switch v.Kind() {
	case reflect.Pointer:
		return checkpointChannelValue(v.Elem(), seen)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if checkpointChannelValue(v.Index(i), seen) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if checkpointChannelValue(iter.Key(), seen) || checkpointChannelValue(iter.Value(), seen) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if checkpointChannelValue(v.Field(i), seen) {
				return true
			}
		}
	}
	return false
}

// 已确认键归完整节点帧，不归消息尝试；连续消息和恢复都不能重复保存普通键。
// 共用执行树锁只保护索引，不在锁内调用钩子或读取业务值。
func (rc *RunCtx) checkpointValueConfirmed(name string) bool {
	failure := rc.exec.checkpoint.failure
	failure.mu.Lock()
	defer failure.mu.Unlock()
	return failure.values[rc.checkpointWrites][name]
}

func (rc *RunCtx) confirmCheckpointValues(values []CheckpointOutput) {
	if len(values) == 0 {
		return
	}
	failure := rc.exec.checkpoint.failure
	failure.mu.Lock()
	defer failure.mu.Unlock()
	if failure.values == nil {
		failure.values = make(map[*writeSet]map[string]bool)
	}
	confirmed := failure.values[rc.checkpointWrites]
	if confirmed == nil {
		confirmed = make(map[string]bool, len(values))
		failure.values[rc.checkpointWrites] = confirmed
	}
	for _, value := range values {
		confirmed[value.Key.Name] = true
	}
}

func (rc *RunCtx) forgetCheckpointValues() {
	failure := rc.exec.checkpoint.failure
	failure.mu.Lock()
	defer failure.mu.Unlock()
	delete(failure.values, rc.checkpointWrites)
}

// 全量验证后才安装恢复子集；不能让存储钩子借它发布别人的键、流或未到达值。
func checkCheckpointAttemptValues(n *Node, values []CheckpointOutput) error {
	keys := make(map[string]keyRef, len(n.provides))
	for _, ref := range n.provides {
		keys[ref.name] = ref
	}
	for _, output := range values {
		ref, ok := keys[output.Key.Name]
		if !ok || output.Key != checkpointKey(ref) || isLiveStreamType(ref.typ) || output.Stream != nil {
			return fmt.Errorf("checkpoint node %q: undeclared, duplicate or non-ordinary message value %q", n.id, output.Key.Name)
		}
		delete(keys, output.Key.Name)
		if err := checkCheckpointOutput(output); err != nil {
			return err
		}
	}
	return nil
}
