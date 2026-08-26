package flow

import (
	"context"
	"fmt"
)

// Node 是图中的一个计算单元：声明读哪些 Key、写哪些 Key，以及到达后做什么。
type Node struct {
	id       string
	requires []keyRef
	provides []keyRef
	run      func(*RunCtx) error
	aspects  []Aspect
}

// Requires 声明本节点依赖的输入槽。
func Requires[T any](keys ...Key[T]) []keyRef {
	out := make([]keyRef, len(keys))
	for i, k := range keys {
		out[i] = k.asRef()
	}
	return out
}

// Provides 声明本节点会写出的输出槽。
func Provides[T any](keys ...Key[T]) []keyRef {
	out := make([]keyRef, len(keys))
	for i, k := range keys {
		out[i] = k.asRef()
	}
	return out
}

// Deps 把多组 Requires / Provides 拼成一条声明。用于一个节点同时
// 依赖不同类型的 Key。
func Deps(groups ...[]keyRef) []keyRef {
	var n int
	for _, g := range groups {
		n += len(g)
	}
	out := make([]keyRef, 0, n)
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// NewNode 构造节点。run 只应 Get 声明过的 Requires、Set/Skip 声明过的 Provides。
func NewNode(id string, requires, provides []keyRef, run func(*RunCtx) error, aspects ...Aspect) *Node {
	return &Node{id: id, requires: requires, provides: provides, run: run, aspects: aspects}
}

// ID 返回节点标识。
func (n *Node) ID() string { return n.id }

// RunCtx 是节点（及切面）在一次运行里能看到的世界：声明过的槽位 +
// 本层可取消的 context。拿不到整个黑板。
type RunCtx struct {
	g       *Graph
	node    *Node
	ctx     context.Context
	cancel  context.CancelFunc
	allowed map[string]keyRef // name → 声明过的 key
	wrote   map[string]struct{}
}

func newRunCtx(g *Graph, n *Node, parent context.Context) *RunCtx {
	ctx, cancel := context.WithCancel(parent)
	allowed := make(map[string]keyRef, len(n.requires)+len(n.provides))
	for _, k := range n.requires {
		allowed[k.name] = k
	}
	for _, k := range n.provides {
		allowed[k.name] = k
	}
	return &RunCtx{
		g:       g,
		node:    n,
		ctx:     ctx,
		cancel:  cancel,
		allowed: allowed,
		wrote:   make(map[string]struct{}),
	}
}

// Context 返回本层可取消的 context。切面超时应取消它，以打断 Get/WaitAll。
func (rc *RunCtx) Context() context.Context { return rc.ctx }

// NodeID 返回当前节点 id。
func (rc *RunCtx) NodeID() string {
	if rc.node == nil {
		return ""
	}
	return rc.node.id
}

// Fork 派生一层可独立取消的 RunCtx（对齐 v1 AspectContext）。
// 子层取消不影响父层；父层取消会通过 context 传播到子层。
func (rc *RunCtx) Fork() *RunCtx {
	cp := *rc
	cp.ctx, cp.cancel = context.WithCancel(rc.ctx)
	return &cp
}

// Cancel 取消本层 context。
func (rc *RunCtx) Cancel() {
	if rc.cancel != nil {
		rc.cancel()
	}
}

func (rc *RunCtx) must(k keyRef, write bool) error {
	got, ok := rc.allowed[k.name]
	if !ok || got.typ != k.typ {
		return fmt.Errorf("%w: %s on node %s", ErrUndeclared, k, rc.NodeID())
	}
	if write {
		// 只允许写 Provides。
		for _, p := range rc.node.provides {
			if p.name == k.name {
				return nil
			}
		}
		return fmt.Errorf("%w: %s is not provided by node %s", ErrUndeclared, k, rc.NodeID())
	}
	return nil
}

// Get 等待 Key 到达：就绪返回值，跳过返回 ErrSkipped。
func Get[T any](rc *RunCtx, k Key[T]) (T, error) {
	var zero T
	ref := k.asRef()
	if err := rc.must(ref, false); err != nil {
		return zero, err
	}
	v, err := rc.g.slotOf(ref).wait(rc.ctx)
	if err != nil {
		if err == ErrSkipped {
			return zero, skipErr(ref.name)
		}
		return zero, err
	}
	out, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("flow: key %q type assertion failed", ref.name)
	}
	return out, nil
}

// TryGet 非阻塞读取。ok=true 表示已就绪；skipped=true 表示已跳过。
func TryGet[T any](rc *RunCtx, k Key[T]) (v T, ok bool, skipped bool, err error) {
	ref := k.asRef()
	if err = rc.must(ref, false); err != nil {
		return
	}
	st, raw := rc.g.slotOf(ref).snapshot()
	switch st {
	case slotReady:
		out, cast := raw.(T)
		if !cast {
			err = fmt.Errorf("flow: key %q type assertion failed", ref.name)
			return
		}
		return out, true, false, nil
	case slotSkipped:
		return v, false, true, nil
	default:
		return v, false, false, nil
	}
}

// Set 幂等首写为就绪。二次调用忽略。与已跳过冲突则报错。
func Set[T any](rc *RunCtx, k Key[T], v T) error {
	ref := k.asRef()
	if err := rc.must(ref, true); err != nil {
		return err
	}
	if err := rc.g.slotOf(ref).resolveValue(v); err != nil {
		return err
	}
	rc.wrote[ref.name] = struct{}{}
	return nil
}

// SetOrUpdate 覆盖为最新值。已跳过则冲突。
func SetOrUpdate[T any](rc *RunCtx, k Key[T], v T) error {
	ref := k.asRef()
	if err := rc.must(ref, true); err != nil {
		return err
	}
	if err := rc.g.slotOf(ref).updateValue(v); err != nil {
		return err
	}
	rc.wrote[ref.name] = struct{}{}
	return nil
}

// Skip 将一条 Provide 标记为跳过。已就绪则冲突。
func Skip[T any](rc *RunCtx, k Key[T]) error {
	ref := k.asRef()
	if err := rc.must(ref, true); err != nil {
		return err
	}
	if err := rc.g.slotOf(ref).resolveSkip(); err != nil {
		return err
	}
	rc.wrote[ref.name] = struct{}{}
	return nil
}

// WaitAll 等待全部 keys 就绪。任一跳过 → ErrSkipped（带被跳过的名字）。
func WaitAll(rc *RunCtx, keys ...keyRef) error {
	var skipped []string
	for _, k := range keys {
		if err := rc.must(k, false); err != nil {
			return err
		}
		_, err := rc.g.slotOf(k).wait(rc.ctx)
		if err == ErrSkipped {
			skipped = append(skipped, k.name)
			continue
		}
		if err != nil {
			return err
		}
	}
	if len(skipped) > 0 {
		return skipErr(skipped...)
	}
	return nil
}

// WaitAnyResult 是 WaitAny 的到达结果。
type WaitAnyResult struct {
	Name    string
	Skipped bool
}

// WaitAny 等待任一 key 以「值」到达。先到跳过则继续等其余；
// 全部跳过 → ErrSkipped。
func WaitAny(rc *RunCtx, keys ...keyRef) (WaitAnyResult, error) {
	if len(keys) == 0 {
		return WaitAnyResult{}, fmt.Errorf("flow: WaitAny requires at least one key")
	}
	type arr struct {
		k   keyRef
		err error
	}
	ch := make(chan arr, len(keys))
	for _, k := range keys {
		if err := rc.must(k, false); err != nil {
			return WaitAnyResult{}, err
		}
		k := k
		go func() {
			_, err := rc.g.slotOf(k).wait(rc.ctx)
			ch <- arr{k: k, err: err}
		}()
	}
	var skipped []string
	pending := len(keys)
	for pending > 0 {
		select {
		case <-rc.ctx.Done():
			return WaitAnyResult{}, rc.ctx.Err()
		case a := <-ch:
			pending--
			if a.err == nil {
				return WaitAnyResult{Name: a.k.name}, nil
			}
			if a.err == ErrSkipped {
				skipped = append(skipped, a.k.name)
				continue
			}
			return WaitAnyResult{}, a.err
		}
	}
	return WaitAnyResult{Skipped: true}, skipErr(skipped...)
}
