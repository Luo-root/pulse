package pulse

import (
	"context"
	"fmt"
	"reflect"
	"sort"
)

// CheckpointKey 是检查点桥接的键声明，Type 是真实 Go 类型，不是显示用的短名。
type CheckpointKey struct {
	Name string
	Type reflect.Type
	// Element 仅受管流非空；Codec 编码消息元素，不编码 Stream 端点。
	Element reflect.Type
}

// CheckpointNode 是静态挂载位置上的节点声明副本。Stream 非空表示流式节点。
// 切片与定义隔离，不携带运行状态或业务值。
type CheckpointNode struct {
	Graph, Path, ID    string
	Requires, Provides []CheckpointKey
	Stream             string
}

// CheckpointOutput 是一条已到达输出。ready 的 Value 可为合法的 nil 接口或 typed nil；
// skipped 没有值。pending 不能作为检查点成果。
type CheckpointOutput struct {
	Key   CheckpointKey
	State SlotState
	Value any
	// Stream 与 Value 互斥，是独立流记录的引用；不会把本地端点交给普通 Codec。
	Stream *CheckpointStreamDescriptor
}

// NodeCheckpoint 是一个节点的完整成果，恰好覆盖其全部 Provides（含隐式 skipped）。
// Reason 只能是 NodeCompleted 或 NodeSkipped；逐键 Skip 后返回 nil 仍是 completed。
type NodeCheckpoint struct {
	NodeID  string
	Reason  NodeFinishReason
	Outputs []CheckpointOutput
}

// CheckpointAttemptRequest 打开或认回一个持久尝试。
// Parent 是外层作用域，Boundary 为空表示完整节点链，Limit=1；最终失败不靠 Resume 补充预算。
// Retry 的 Boundary 来自有效 Aspect 链位置，Limit 是其最大尝试次数。
type CheckpointAttemptRequest struct {
	Site                     Site
	Parent, NodeID, Boundary string
	Limit                    int
}

// CheckpointAttempt 是 Store 确认的持久游标。活跃尝试必须认回同一 Scope/Index；
// 已确认失败后才可开 Index+1。Exhausted 表示已用完 Limit，不再进入业务链。
type CheckpointAttempt struct {
	Scope     string
	Index     int
	Exhausted bool
	// Resumed 是可选观察事实：true = 认回已经活跃的 Scope/Index，false = 本次新分配。
	// nil 表示旧桥未提供，Core 记为 unknown，不根据 Index 或 Execution ID 猜测。
	// 只在 !Exhausted 时有意义；桥返回后不得修改所指值，不改变持久格式或预算。
	Resumed *bool
	// Values 仅完整节点根尝试可返回已确认消息贡献的普通输出子集。
	// Core 在业务前恢复到完整链暂存，仍待节点 Commit 后发布；不含流端点。
	Values []CheckpointOutput
}

// CheckpointCommit 是完整节点链的提交单元。Scope 是本层图作用域，Attempt 是该节点
// 完整链的根尝试；集成层必须验证它及全部祖先有效，原子保存终态和全部输出。
type CheckpointCommit struct {
	Site           Site
	Scope, Attempt string
	Node           NodeCheckpoint
	// Streams 只含本节点实际拥有的写端终态；必须与 Node 原子确认。
	// Sub 的别名输出不是写端，不能提前终结子节点的流。
	Streams []CheckpointStreamFinish
}

// CheckpointChild 打开静态子图。Attempt 是当前最内层持久尝试，也是子图作用域。
// Inputs 包含 Fix/FixSkip 及此刻已到达的 In，后者仅保留旧桥接层的观察兼容性。
// Bindings 完整描述 In 的稳定来源，允许入口仍 pending；其中列出的键不参与值指纹。
// 集成层结合根输入指纹、静态拓扑/Version、固定值和绑定身份核验，不能等待全部 In。
type CheckpointChild struct {
	Site                   Site
	Attempt, NodeID, Graph string
	Inputs                 []CheckpointOutput
	Bindings               []CheckpointBinding
}

// CheckpointScopeFailure 表示桥已完整校验并可靠确认某图作用域的最终业务失败。
// 不携带原错误或成果，不表示存储故障，也不能用来重开失败来源或刷新预算。
//
// 只有 OpenChild 直接返回非 nil *CheckpointScopeFailure、Scope 精确匹配请求 Attempt
// 且没有返回成果时，Core 才在安装恢复计划前把它交回父完整链，由父切面/Retry 决定后续。
// 包装、空/错 Scope、同时返回成果，或其他钩子返回本类型，均仍是可靠检查点失败。
// 根作用域由集成层在恢复解码/事件/Start 前识别并返回；此时尚未启动执行。
// 原业务错误文本不持久化，因此恢复失败不承诺还原原错误身份。
type CheckpointScopeFailure struct {
	Scope string
}

func (e *CheckpointScopeFailure) Error() string {
	return fmt.Sprintf("pulse: checkpoint scope %q has a confirmed business failure", e.Scope)
}

// CheckpointNotStarted 仅表示桥因调用上下文取消，在任何可靠操作开始前停止。
// 不得用于已经调用 Store、可能写成或结果不确定的错误；不包装其他原因。
// Core 仅在本次调用适用的业务域或控制域已取消时接受直接非 nil 指针，保留为中断；
// 两域传播没有先后保证。包装信号、两域仍健康或任何普通存储错误仍锁存为可靠失败。
type CheckpointNotStarted struct{}

func (*CheckpointNotStarted) Error() string {
	return "pulse: checkpoint operation not started after cancellation"
}

// CheckpointBinding 描述一次静态 In 绑定；Source.Execution 仅用于观察，不参与持久身份。
// Scope 是父图作用域，Attempt 是此次挂载的有效持久尝试，Parent/Child 是真实键声明。
type CheckpointBinding struct {
	Source         Site
	Scope, Attempt string
	Parent, Child  CheckpointKey
}

// CheckpointHooks 是显式检查点模式的同步可靠边界，四个函数都必须提供。
//
// 钩子在引擎锁外调用，可以并发，不得修改业务值或异步保存引用后提前确认。
// 错误或 panic 停止整棵执行树；即使错误含 ErrSkipped 也不是正常跳过，不能被父 Retry
// 或外层切面吞掉。唯一例外是 OpenChild 的精确 CheckpointScopeFailure 信号，见其契约。
// 返回其他错误可能表示存储结果不确定，Core 不自动重试或撤销提交。
//
// 调用接纳前检查适用的业务取消域，已取消时不再新开可靠操作。接纳后的整个同步调用
// 不受内部业务 fail-fast 取消影响；宿主、Execution.Cancel、父层显式取消与适用的
// Deadline/Timeout 仍传入钩子。上下文保留 values，调用方不应依赖其与业务 ctx 指针相同。
//
// FailAttempt 同步确认真实失败，之后才能打开下一轮；不保存业务错误文本。
// 宿主取消或可靠性失败保持尝试活跃，由恢复时读取的权威记录决定下一步。
type CheckpointHooks struct {
	OpenAttempt func(context.Context, CheckpointAttemptRequest) (CheckpointAttempt, error)
	FailAttempt func(context.Context, string) error
	OpenChild   func(context.Context, CheckpointChild) ([]NodeCheckpoint, error)
	Commit      func(context.Context, CheckpointCommit) error
	// StreamHooks 可选；普通完整节点仍只要求上面四个钩子。
	// 使用受管流时必须提供完整增量钩子，原生 channel 仍不支持检查点。
	StreamHooks *CheckpointStreamHooks
}

// WithExecCheckpoint 为本次执行及全部子执行启用检查点模式，与 Aspect 独立。
// 完整链的普通输出先暂存，最终成果经 Commit 确认才逐键发布；需非空作用域及完整钩子。
// 受管流使用可选 StreamHooks 逐消息确认；完整成果只保存流 descriptor。
// Store/Codec/RunID/版本/所有权属于集成层，Core 不提供存储或 exactly-once 外部副作用。
func WithExecCheckpoint(scope string, hooks CheckpointHooks) ExecOption {
	if hooks.StreamHooks != nil {
		streamHooks := *hooks.StreamHooks
		hooks.StreamHooks = &streamHooks
	}
	return func(c *execConfig) { c.checkpoint = &checkpointConfig{scope: scope, hooks: hooks} }
}

// CheckpointPort 是静态端口的真实类型声明，同键透传时 Input/Output 都为 true。
type CheckpointPort struct {
	Graph, Path   string
	Key           CheckpointKey
	Input, Output bool
}

// CheckpointPorts 递归复制全部声明端口，包括未使用和透传端口，供 Codec/schema 校验。
// 每层先按进口声明顺序，再补仅作出口的键；不封口、不读取业务值。
func (g *Graph) CheckpointPorts() []CheckpointPort {
	if g == nil {
		return nil
	}
	return g.checkpointPortsAt("", nil)
}

func (g *Graph) checkpointPortsAt(path string, chain mountChain) []CheckpointPort {
	if chain.has(g) {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	chain = chain.push(g)
	var out []CheckpointPort
	for _, k := range g.inPortOrder {
		_, alsoOutput := g.outPorts[k.name]
		out = append(out, CheckpointPort{Graph: g.id, Path: path, Key: checkpointKey(k), Input: true, Output: alsoOutput})
	}
	for _, k := range g.outPortOrder {
		if _, alsoInput := g.inPorts[k.name]; !alsoInput {
			out = append(out, CheckpointPort{Graph: g.id, Path: path, Key: checkpointKey(k), Output: true})
		}
	}
	for _, n := range g.nodes {
		if n.child != nil {
			childPath := n.id
			if path != "" {
				childPath = path + "/" + n.id
			}
			out = append(out, n.child.checkpointPortsAt(childPath, chain)...)
		}
	}
	return out
}

// CheckpointNodes 按声明顺序递归复制静态节点，Path 区分同一子图的多个挂载位置。
// 不封口、不执行业务；需要稳定 schema 的集成层先调用 Validate。
func (g *Graph) CheckpointNodes() []CheckpointNode {
	if g == nil {
		return nil
	}
	return g.checkpointNodesAt("", nil)
}

func (g *Graph) checkpointNodesAt(path string, chain mountChain) []CheckpointNode {
	if chain.has(g) {
		return nil // 未验证的非法组合环不递归；Validate 提供正式诊断。
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	chain = append(append(mountChain(nil), chain...), g)
	var out []CheckpointNode
	for _, n := range g.nodes {
		out = append(out, checkpointNode(g.id, path, n))
		if n.child != nil {
			childPath := n.id
			if path != "" {
				childPath = path + "/" + n.id
			}
			out = append(out, n.child.checkpointNodesAt(childPath, chain)...)
		}
	}
	return out
}

func checkpointNode(graph, path string, n *Node) CheckpointNode {
	return CheckpointNode{Graph: graph, Path: path, ID: n.id,
		Requires: checkpointKeys(n.requires), Provides: checkpointKeys(n.provides), Stream: n.streamKind}
}

func checkpointKeys(refs []keyRef) []CheckpointKey {
	out := make([]CheckpointKey, len(refs))
	for i, k := range refs {
		out[i] = checkpointKey(k)
	}
	return out
}

// CheckpointInputs 复制本执行已接收的外部输入，按键名排序。不会读取内部输出、建槽或发事件。
// 业务引用不深拷贝；集成层同步编码用于输入兼容性核对，不得修改这些值。
func (e *Execution) CheckpointInputs() []CheckpointOutput {
	if e == nil || e.graph == nil {
		return nil
	}
	e.graph.mu.Lock()
	refs := make([]keyRef, 0, len(e.seeded))
	for name := range e.seeded {
		refs = append(refs, e.graph.inPorts[name])
	}
	e.graph.mu.Unlock()
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	out := make([]CheckpointOutput, 0, len(refs))
	for _, ref := range refs {
		e.slotsMu.Lock()
		s := e.slots[ref.name]
		e.slotsMu.Unlock()
		st, v := s.snapshot()
		if st != slotPending { // 子图 In 可已绑定但尚未到达；它由 CheckpointChild.Bindings 表达。
			out = append(out, checkpointOutput(ref, st, v))
		}
	}
	return out
}

// RestoreNodes 核验并替换启动前的整份恢复计划，不立即灌槽。容器复制，业务值不深拷贝。
// 失败保留原计划；Start 在封口同一临界区按实际定义复验，全部通过才恢复。
// 仅用于检查点模式，成功 Start 后拒绝；恢复节点不重跑切面、业务体或 Commit。
func (e *Execution) RestoreNodes(records []NodeCheckpoint) error {
	if e == nil || e.graph == nil {
		return fmt.Errorf("pulse: nil execution in RestoreNodes")
	}
	assemblyMu.Lock()
	defer assemblyMu.Unlock()
	e.graph.mu.Lock()
	defer e.graph.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return ErrGraphStarted
	}
	if e.checkpoint == nil {
		return fmt.Errorf("pulse: RestoreNodes requires checkpoint mode")
	}
	plan := copyCheckpoints(records)
	if err := e.graph.checkRestoreLocked(plan); err != nil {
		return err
	}
	e.restore = plan
	return nil
}

func copyCheckpoints(records []NodeCheckpoint) []NodeCheckpoint {
	out := append([]NodeCheckpoint(nil), records...)
	for i := range out {
		out[i].Outputs = append([]CheckpointOutput(nil), out[i].Outputs...)
		for j := range out[i].Outputs {
			if descriptor := out[i].Outputs[j].Stream; descriptor != nil {
				copy := *descriptor
				out[i].Outputs[j].Stream = &copy
			}
		}
	}
	return out
}

// checkRestoreLocked 只读核验整批，不调用宿主代码、不建运行槽位。调用方持 Graph.mu。
func (g *Graph) checkRestoreLocked(records []NodeCheckpoint) error {
	nodes := make(map[string]*Node, len(g.nodes))
	for _, n := range g.nodes {
		nodes[n.id] = n
	}
	seen := make(map[string]bool, len(records))
	descriptors := make(map[[4]string]CheckpointStreamDescriptor)
	for _, r := range records {
		n := nodes[r.NodeID]
		if n == nil || seen[r.NodeID] {
			return fmt.Errorf("pulse: restore node %q: unknown or duplicate node", r.NodeID)
		}
		seen[r.NodeID] = true
		if r.Reason != NodeCompleted && r.Reason != NodeSkipped {
			return fmt.Errorf("pulse: restore node %q: invalid terminal reason %q", r.NodeID, r.Reason)
		}
		if len(r.Outputs) != len(n.provides) {
			return fmt.Errorf("pulse: restore node %q: outputs must cover all Provides", r.NodeID)
		}
		keys := make(map[string]reflect.Type, len(n.provides))
		for _, k := range n.provides {
			keys[k.name] = k.typ
		}
		for _, o := range r.Outputs {
			typ, ok := keys[o.Key.Name]
			if !ok || typ != o.Key.Type {
				return fmt.Errorf("pulse: restore node %q: undeclared, duplicate or mistyped output %q", r.NodeID, o.Key.Name)
			}
			delete(keys, o.Key.Name)
			if err := checkCheckpointOutput(o); err != nil {
				return fmt.Errorf("pulse: restore node %q: %w", r.NodeID, err)
			}
			if o.Stream != nil {
				if o.Stream.Terminal != StreamCompleted && o.Stream.Terminal != StreamStopped && !(n.sub && (o.Stream.Terminal == "" || o.Stream.Terminal == StreamFailed)) {
					return fmt.Errorf("pulse: restore node %q: stream %q is not a completed source reference", r.NodeID, o.Key.Name)
				}
				id := o.Stream.ID
				logical := [4]string{id.Scope, id.NodeID, id.Key, id.Group}
				descriptor := *o.Stream
				if previous, exists := descriptors[logical]; exists {
					var err error
					descriptor, err = mergeCheckpointStreamReference(previous, descriptor)
					if err != nil {
						return err
					}
				}
				descriptors[logical] = descriptor
			}
		}
	}
	for _, r := range records {
		n := nodes[r.NodeID]
		// Sub 的 Requires 只是 In 绑定，不是整体完成门；Aspect 也可在进入子图前
		// 正常短路。实际业务节点的普通依赖仍在各自作用域按完整成果核验。
		if n.sub {
			continue
		}
		for _, k := range n.requires {
			if p := g.producer[k.name]; p.kind == sourceNode && !seen[p.nodeID] && !isManagedStreamType(k.typ) {
				return fmt.Errorf("pulse: restore node %q: required producer %q has no checkpoint", r.NodeID, p.nodeID)
			}
		}
	}
	return nil
}

func checkCheckpointOutput(o CheckpointOutput) error {
	if o.Key.Type == nil || o.Key.Name == "" {
		return fmt.Errorf("checkpoint output has an empty key or type")
	}
	if o.State == SlotStateSkipped {
		if o.Value != nil || o.Stream != nil {
			return fmt.Errorf("checkpoint skipped output %q carries a value", o.Key.Name)
		}
		return nil
	}
	if o.State != SlotStateReady {
		return fmt.Errorf("checkpoint output %q is not ready or skipped", o.Key.Name)
	}
	if isManagedStreamType(o.Key.Type) {
		if o.Value != nil || o.Stream == nil {
			return fmt.Errorf("checkpoint stream output %q requires a descriptor, not an endpoint value", o.Key.Name)
		}
		return checkCheckpointStreamDescriptor(o.Key, *o.Stream)
	}
	if o.Stream != nil {
		return fmt.Errorf("checkpoint ordinary output %q carries a stream descriptor", o.Key.Name)
	}
	if o.Value == nil {
		if o.Key.Type.Kind() != reflect.Interface {
			return fmt.Errorf("checkpoint output %q: nil is not a value of %s", o.Key.Name, o.Key.Type)
		}
	} else if _, ok := valueForType(reflect.ValueOf(o.Value), o.Key.Type); !ok {
		return fmt.Errorf("checkpoint output %q: value type does not match %s", o.Key.Name, o.Key.Type)
	}
	if checkpointChannelType(o.Key.Type, nil) || checkpointChannelValue(reflect.ValueOf(o.Value), nil) {
		return fmt.Errorf("checkpoint output %q contains a channel", o.Key.Name)
	}
	return nil
}

func checkpointChannelType(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	if seen == nil {
		seen = make(map[reflect.Type]bool)
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Chan:
		return true
	case reflect.Pointer, reflect.Array, reflect.Slice:
		return checkpointChannelType(t.Elem(), seen)
	case reflect.Map:
		return checkpointChannelType(t.Key(), seen) || checkpointChannelType(t.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if checkpointChannelType(t.Field(i).Type, seen) {
				return true
			}
		}
	}
	return false
}
