package pulse

import (
	"context"
	"fmt"
	"reflect"
	"sync"
)

// Node 是图中的一个计算单元：声明读哪些 Key、写哪些 Key，以及到达后做什么。
type Node struct {
	id string
	// desc 是构造时给的说明（`""` = 没写）。只由 NewNode 写入，没有 setter——
	// 「入图后元数据稳定」是结构上成立的，不靠校验。
	desc     string
	requires []keyRef
	provides []keyRef
	run      func(*RunCtx) error
	aspects  []Aspect
	// streamKind 非空表示这个节点是流式糖装出来的（"produce" / "consume" /
	// "tee"）。Start 的流式校验要按它认流节点：名额是**同时活着**的约束，
	// 而糖在装配那一刻看不全整张图。手写节点为空串。
	streamKind string
	// compete 非空表示这个节点是某次 `FanOut` 的 worker（值 = 那次调用的 id）。
	// 一条流出口只喂一个下游；**同一组** worker 共读一条出口是显式声明的
	// 「抢」（FanOut 本来就是这么用的），不算静默瓜分——见 Graph.problemsStreamLocked。
	compete string
	// sub 表示这个节点是挂子图的一步（「图即节点」，`Sub` 置它）。
	// 它整段都在等子图跑完，所以**不吃运行名额**——占着就等于把父侧额度锁在「等」上，
	// 名额跨图共享之后那是死锁（见 WithMaxRunning / Execution.runNode）。
	sub bool
	// child 是这一步挂的子图**定义**（`Sub` 装的节点才有它）。静态拓扑靠它递归展开
	// 整棵树，组合环也靠它判（见 Sub 与 findInSubtree）。
	child *Graph
	// binds 是这一步的接线。递归校验要用它重新核一遍端口（子图的端口可能在挂载之后、
	// 它自己封口之前改过），静态拓扑要用它展示父子接线。
	binds []SubBind
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
// 静止的图（所有 `Requires` 都没有来源）与含依赖环的图都会在 `Execution.Start`
// 就被拒掉——两者都不可能跑完，且都是启动那一刻能静态判定的。
//
// `description` 是人可读的说明（`""` = 没写，合法）：不参与节点唯一性、Key 匹配、
// 拓扑排序与运行语义，只出现在 `Topology()` / `Snapshot()` 这类**给人看**的出口上。
// 入图后不再变——只由构造函数写入，没有 setter，所以「元数据稳定」是结构上成立的。
//
// **它与 `id` 都是 string，写反了照样编译**（`NewNode("入库", "load", …)`）——
// 那是调用点的错，单元测试兜不住。放在 `id` 后面是因为读调用点时描述就在眼前；
// 写反的后果只是元数据不对，而 `Topology()` / Mermaid / 快照正是让元数据一眼可见
// 的那一层。
//
// **三份声明都复制一份收下**（`requires` / `provides` / `aspects`）：节点一旦 `Add` 进图，
// 它的依赖形状就登记进来源表与依赖边了，此后调用方手里那份切片还能改底层数组的话，
// 就等于绕过装配锁与封口改定义——`Topology()` 与校验看到的图、真跑的那张成了两张。
// 复制是防御性的，诚实的调用方看不出区别（`Requires` / `Provides` / `Deps` 本来就返回新切片）。
func NewNode(id, description string, requires, provides []keyRef, run func(*RunCtx) error, aspects ...Aspect) *Node {
	return &Node{
		id:       id,
		desc:     description,
		requires: append([]keyRef(nil), requires...),
		provides: append([]keyRef(nil), provides...),
		run:      run,
		aspects:  append([]Aspect(nil), aspects...),
	}
}

// ID 返回节点标识。
func (n *Node) ID() string { return n.id }

// Description 是构造时给的说明（`""` = 没写）。
func (n *Node) Description() string { return n.desc }

// RunCtx 是节点（及切面）在一次运行里能看到的世界：这次运行的槽位 +
// 本层可取消的 context。拿不到整个黑板。
type RunCtx struct {
	exec    *Execution
	node    *Node
	ctx     context.Context
	cancel  context.CancelFunc
	allowed map[string]keyRef // name → 声明过的 key
	// writes 是**当前写暂存**：nil 时 `Set` / `Skip` 直写执行槽位。
	//
	// `Retry` 为每次尝试开一层暂存，边界是它包住的 `next`（含更内层切面）；外层切面的
	// 写不属于该 attempt，也不随该 attempt 回滚。启用检查点时，runCheckpointChain
	// 先为完整节点链开外层暂存，Retry 的成功写交到这一层，存储确认 Commit 后才发布。
	// 因此检查点模式下，即使不装 Retry，体和切面的写也先暂存；`Fork` 浅拷贝共享当前帧。
	writes *writeSet
	// advance 是**这一次运行里这个节点**的推进账（见 advanceLedger）。它长在这里而不是
	// 节点上：节点属于**定义**、要跨运行复用，而 `newRunCtx` 是每个 (执行, 节点) 一份
	// ——「这一步推进过没有」是那一次运行的事实。`Fork` 浅拷贝，所以 `Timeout` 里面
	// 那层看到的是同一份账。
	advance *advanceLedger
	// checkpointAttempt 是当前持久尝试，Fork 复制其视图；Retry 只在自己的层次换它。
	checkpointAttempt CheckpointAttempt
	aspectIndex       int
	checkpointEntries *checkpointEntryLedger
}

// advanceLedger 是**一次运行里一个节点**的推进账：体推进过没有、上一次推进挂在哪条
// 尝试边界上、第一次推进失败在哪（见 `RunCtx.beginAdvance`）。
//
// 账记在执行侧而不是定义侧，正是本账的全部意义：同一张定义顺序跑两遍、并发跑两份，
// 各是各的账（实测过：账记在体闭包里，会把第二遍与并发的第二份都误报成重入）。
type advanceLedger struct {
	mu      sync.Mutex
	entered bool
	// frame 是上一次推进时挂着的尝试边界（nil = 当时没有边界）。第二次推进时它**没变**
	// 才叫「无边界重入」；`Retry` 每次尝试都换一条新的暂存，所以它的重入是合法的。
	frame *writeSet
	// firstErr 是**第一次**推进的失败原因：无边界重入的报文要点名病根，不许让后来的错
	// 盖掉它（`Retry` 返回的正是**最后一次**尝试的错误）。
	firstErr error
}

func newRunCtx(e *Execution, n *Node, parent context.Context) *RunCtx {
	ctx, cancel := context.WithCancel(parent)
	allowed := make(map[string]keyRef, len(n.requires)+len(n.provides))
	for _, k := range n.requires {
		allowed[k.name] = k
	}
	for _, k := range n.provides {
		allowed[k.name] = k
	}
	return &RunCtx{
		exec:    e,
		node:    n,
		ctx:     ctx,
		cancel:  cancel,
		allowed: allowed,
		advance: &advanceLedger{},
	}
}

// beginAdvance 是**推进闸**：同一个节点执行上下文里，`next()` 只允许成功推进一次。
//
// 判据是「这一次推进有没有新的尝试边界」：
//
//   - 没推进过 → 放行；
//   - 推进过、但边界换了（`Retry` 每次尝试都开一条新的写暂存）→ 放行；
//   - 推进过、边界没换 → 拒。这就是「用户自己写的重试切面」那个形状：它的第二次推进
//     没有隔离，第一次已经写出去的东西会被幂等首写静默吞掉，节点手里就成了两次推进的混装。
//
// 限制的是**这一次执行里这个节点**的推进次数，不是按节点 id 全局禁止再次执行：另一次
// `Execution`（另一次 `Run`）是全新的执行上下文，账从零开始。
func (rc *RunCtx) beginAdvance() error {
	le := rc.advance
	if le == nil {
		return nil
	}
	le.mu.Lock()
	defer le.mu.Unlock()
	if !le.entered {
		le.entered = true
		le.frame = rc.writes
		return nil
	}
	if le.frame != rc.writes {
		le.frame = rc.writes // 新的尝试边界：合法的重入，账换到新边界上
		return nil
	}
	cause := ""
	if le.firstErr != nil {
		cause = fmt.Sprintf(". The first advance failed with: %v", le.firstErr)
	}
	return fmt.Errorf("pulse: node %s: this node was advanced a second time without a new attempt "+
		"boundary: a node is advanced once per execution, and a second advance would run its body "+
		"again (for a graph step that is the whole child subtree) while every output the first "+
		"advance had already written would be silently ignored — slots are idempotent first write, "+
		"so the node could end up with outputs from both advances. Use pulse.Retry, which opens a "+
		"fresh per-attempt write set on every try; a second Run is a fresh execution and is "+
		"unaffected%s (%w)", rc.NodeID(), cause, ErrNextCalledTwice)
}

// endAdvance 记下这一次推进的结果：**第一次**推进的失败原因供推进闸报病根用。
//
// 推进闸没有报错时才会走到这里（被拒的那次不记账），所以 `firstErr` 永远是「真的跑过
// 并且失败了」的那一次。
func (rc *RunCtx) endAdvance(err error) {
	le := rc.advance
	if le == nil || err == nil {
		return
	}
	le.mu.Lock()
	if le.firstErr == nil {
		le.firstErr = err
	}
	le.mu.Unlock()
}

// writeTarget 是这一次**写**该落的槽：有尝试边界就落那条暂存（并因此建条目），
// 没有就直写执行槽位。
func (rc *RunCtx) writeTarget(ref keyRef) *slot {
	if rc.writes == nil {
		return rc.exec.slotOf(ref)
	}
	return rc.writes.slotOf(ref)
}

// readTarget 是这一次**读**该等的槽：本 attempt 已经写过的优先（读回自己刚写的，自洽），
// 没有就沿写入目标链往外找——外层尝试的暂存，最后是执行槽位。
//
// 与 writeTarget 的差别是**不建条目**（见 writeSet.peek）：读一条本 attempt 没写过的键，
// 该读的是它真正的来源，而不是在暂存里凭空多出一个永远 `pending` 的空位。
//
// 由此有一条要写进文档的边界：**跨 attempt 读不回上一次被丢弃的写**。那条键在这一次尝试
// 里从来没有过值，所以读它就是等一个没人会写的槽（与今天「一条键没人产出」同一个形状，
// 一直等到本层 ctx 取消）。作废的写重新变得可读，正是本阶段要消灭的东西。
func (rc *RunCtx) readTarget(ref keyRef) *slot {
	for ws := rc.writes; ws != nil; ws = ws.outer {
		if s := ws.peek(ref); s != nil {
			return s
		}
	}
	return rc.exec.slotOf(ref)
}

// landed 报一条键到这一刻**已经落地**：写入目标里写过它，或它在执行槽位里已经到达
// （就绪或跳过都算到达）。
//
// 收尾判据用它（见 `Execution.settleUnwritten`）：一条 `Provide` **没有落地**才该被
// 隐式跳过。判「这个节点写过谁」是不够的——一次失败的尝试写过它、而整份暂存被丢掉了，
// 那条输出就仍然没有落地，该以跳过收尾，而不是把下游永远吊在那条槽上。
func (rc *RunCtx) landed(ref keyRef) bool {
	for ws := rc.writes; ws != nil; ws = ws.outer {
		if s := ws.peek(ref); s != nil {
			st, _ := s.snapshot()
			return st != slotPending
		}
	}
	st, _ := rc.exec.slotOf(ref).snapshot()
	return st != slotPending
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

// Fork 只派生取消上下文，不复制写入目标、推进账或声明权限：
// allowed / writes / advance 与父层共享。切面用它打断 Wait，不是独立写入事务——
// 想开一条独立的事务边界是 `Retry` 的事（它换的是 `writes` 这条链）。
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
	v, err := getRef(rc, ref)
	if err != nil {
		return zero, err
	}
	// ready 的 nil 接口没有动态类型，断言会失败，但它仍是接口 T 的合法零值。
	if v == nil && ref.typ.Kind() == reflect.Interface {
		return zero, nil
	}
	out, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("pulse: key %q type assertion failed", ref.name)
	}
	return out, nil
}

// getRef 是 Get 的非泛型内核：只有 keyRef 可用的地方（`Only` 之类的糖、
// 子图绑定）走这里，类型断言留给调用方。`Get` 在它之上做一次断言，
// 判据与读语义完全同源。
func getRef(rc *RunCtx, ref keyRef) (any, error) {
	if err := rc.must(ref, false); err != nil {
		return nil, err
	}
	v, err := rc.readTarget(ref).wait(rc.ctx)
	if err != nil {
		if err == ErrSkipped {
			return nil, skipErr(ref.name)
		}
		return nil, err
	}
	return v, nil
}

// TryGet 非阻塞读取。ok=true 表示已就绪；skipped=true 表示已跳过。
func TryGet[T any](rc *RunCtx, k Key[T]) (v T, ok bool, skipped bool, err error) {
	ref := k.asRef()
	if err = rc.must(ref, false); err != nil {
		return
	}
	st, raw := rc.readTarget(ref).snapshot()
	switch st {
	case slotReady:
		if raw == nil && ref.typ.Kind() == reflect.Interface {
			return v, true, false, nil
		}
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
//
// **有写暂存时先暂存**：Retry 内的 Set 写入当前尝试，成功或正常跳过才交给外层写入目标。
// 检查点模式还为完整节点链提供外层暂存，存储确认 Commit 后才发布；未开任何暂存时直写执行槽位。
func Set[T any](rc *RunCtx, k Key[T], v T) error { return setRef(rc, k.asRef(), v) }

// setRef 是 Set 的非泛型内核：只有 keyRef 可用的地方（糖、子图桥接）走这里。
// 槽位的类型不靠它兜——同一个键名在本图里只能有一种类型（`keyRegistry`
// 在 Add / Seed 就拒了同名跨型），所以写进来的值一定是对的那一种。
func setRef(rc *RunCtx, ref keyRef, v any) error {
	if err := rc.must(ref, true); err != nil {
		return err
	}
	return rc.writeTarget(ref).resolveValue(v)
}

// Skip 将一条 Provide 标记为跳过。已就绪则冲突。
func Skip[T any](rc *RunCtx, k Key[T]) error { return skipRef(rc, k.asRef()) }

// skipRef 是 Skip 的非泛型内核：糖（Only）要按本节点的声明表逐条作废时，只有
// keyRef 可用，调不了泛型的 Skip。「跳过也是到达」，所以它跟 `Set` 一样按尝试边界分流。
func skipRef(rc *RunCtx, ref keyRef) error {
	if err := rc.must(ref, true); err != nil {
		return err
	}
	return rc.writeTarget(ref).resolveSkip()
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
		_, err := rc.readTarget(k).wait(rc.ctx)
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
