package pulse

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

// NewNode 构造节点。run 的读写面是**本节点声明过的 Key**：`Get` / `TryGet` /
// `WaitAll` 读声明过的任意 Key（含自己的 `Provides`，用于回看自己那条输出的
// 去向），`Set` / `Skip` 只写自己的 `Provides`；没声明过的名字一律
// `ErrUndeclared`。
//
// 注意 `Get` 是**阻塞等待**：等一个永远不会被写入的槽会一直等到本层 ctx
// 被取消（`Timeout` 切面能打断它）。想知道「现在到了没有」用 `TryGet`。
// 静止的图（所有 Requires 都没有来源）会在 `Graph.Start` 就被拒掉。
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

// Fork 只派生取消上下文，不复制写入记录或声明权限：
// allowed / wrote 与父层共享。切面用它打断 Wait，不是独立写入事务。
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

// Get 等待 Key 到达：就绪返回值，跳过返回 *SkipError（带 Key 名，
// errors.Is(err, ErrSkipped) 成立）。
//
// 能读的只有「本节点声明过的」Key（`Requires` 与自己的 `Provides`），其余
// 返回 ErrUndeclared；但**声明过不等于会到达**——没人写它时就一直等下去，
// 直到本层 ctx 取消。要非阻塞地问就用 TryGet。
//
// 读到跳过是**常见路径**，不是异常：节点到几个收几个（见 WaitAll），
// 所以 Run 里读到一条没值的输入得到 *SkipError 是正常结果——按本节点的语义
// 处理它（少一路输入照做，或让整条链路以跳过收尾），它既不阻塞、也不是失败。
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
		return zero, fmt.Errorf("pulse: key %q type assertion failed", ref.name)
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
			err = fmt.Errorf("pulse: key %q type assertion failed", ref.name)
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

// awaitAll 阻塞到 keys 全部到达（就绪或跳过），返回其中以跳过到达的名字。
//
// 「跳过」是到达的一种，不算这一层的错误——只有取消与未声明才返回 err。
// 引擎的门与 WaitAll 共用它，读法不同：门判「有没有值」，WaitAll 报「谁没值」。
func awaitAll(rc *RunCtx, keys []keyRef) ([]string, error) {
	var skipped []string
	for _, k := range keys {
		if err := rc.must(k, false); err != nil {
			return nil, err
		}
		_, err := rc.g.slotOf(k).wait(rc.ctx)
		if err == ErrSkipped {
			skipped = append(skipped, k.name)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return skipped, nil
}

// WaitAll 阻塞到全部 keys 到达（就绪或跳过）。有跳过的返回 *SkipError
// （`Keys` 列出被跳过的名字，errors.Is(err, ErrSkipped) 成立），全部就绪才返回 nil。
//
// 它**不是**节点默认的门：`Requires` 里只要有一条输入真的到了值，节点就会
// 带着到了的那些进入 Run（到几个收几个），输入跳过拦不住它。想让「缺一条就
// 别跑我」的节点把这个返回值直接 return 出去——引擎把 *SkipError 读成
// 「本节点以跳过收尾」：**尚未发布的输出会被跳过，已经发布的槽位不回滚**
// （一次性槽位契约），且不是失败（`Retry` 不重试、`Run`/`Err` 不报错）。
// 这是显式的 fan-in 策略声明，默认策略则相反。
func WaitAll(rc *RunCtx, keys ...keyRef) error {
	skipped, err := awaitAll(rc, keys)
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		return skipErr(skipped...)
	}
	return nil
}
