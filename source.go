package pulse

import "fmt"

// 本文件是引擎内部的**来源表**：某个 Key 由**哪个节点**提供。
//
// 表里只有一种来源——恰好一个节点。「没有来源」的表达永远是「表里没有这个 Key」，
// 而不是表里有一个零值。之所以不用字符串记来源（旧实现把外部来源记成一个字符串哨兵）：
// 节点 ID 是**用户可控**的，一个真叫 `seed` 的节点会与哨兵撞名，判据同时失灵——
//
//  1. 它 `Provides` 某个 Key 之后还能再种同一个 Key（「一个 Key 一个来源」被绕过，
//     **静默**：宿主的值抢先占住槽位，下游读到宿主值，整图照样成功）；
//  2. 环检查按字符串查节点索引，会把「来自外部的那条」错认成这个节点提供的，
//     凭空造出一条边与一个不存在的环。
//
// 结构化的来源让这两种判据都只按 `kind` 走，名字再像也分得开。
//
// 阶段 2 起表里**只剩节点来源**：外部值不再往定义里写（`Seed` / `SkipSeed` 写的是
// **这一次运行的** `Inputs`，「这条键从外面来」由 `DeclareInput` 声明），所以公开拓扑里
// 那条「外部来源」也随之消失——它已经没有产出路径了（见 topology.go 的 `SourceKind`）。

// sourceKind 是来源的种类。零值 `sourceNone` **不得存入来源表**——表达「没有来源」
// 的永远是「表里没有这个 Key」，而不是表里有一个零值。
type sourceKind uint8

const (
	sourceNone sourceKind = iota // 只作为零值
	sourceNode                   // 某个节点 Provides
	// sourceInput 是**声明过的输入端口**（`DeclareInput`）。它**不落来源表**
	// ——端口属于定义，不是「谁会写这条槽」的那张表；`publicSource` 与
	// `keyShapes` 用它把「入口」写进静态拓扑。放进来只是为了让「内部种类 →
	// 公开写法」这条映射只存在一份（sourceName）。
	sourceInput
)

// source 是一条来源记录。`nodeID` 只在 `sourceNode` 时有意义。
type source struct {
	kind   sourceKind
	nodeID string
}

// nodeSource 是某个节点提供的那一条。
func nodeSource(id string) source { return source{kind: sourceNode, nodeID: id} }

// present 报这条来源是否**真的存在**（即「有个节点会写这条槽」）。零值按「没有来源」
// 处理：它只该出现在「表里没有这个 Key」的情形，万一落表了，所有判据都必须当它不存在
// ——不能因为表里有个格子就当成有来源（那会让等待它的节点永远停在 pending）。
func (s source) present() bool { return s.kind == sourceNode }

// same 判两条来源是不是**同一个来源**：同一个节点来源相同。零值不与任何东西相同。
func (s source) same(t source) bool {
	return s.present() && s.kind == t.kind && s.nodeID == t.nodeID
}

// sourceName 是来源名的**唯一**实现（内部 `source` 与公开 `pulse.Source` 共用同一条规则）：
// 节点来源 `node %q`、声明过的入口 `input`、没有来源 `none`。
//
// 节点来源**一律**带类型前缀并加引号，不是只给某个哨兵名字开小灶：节点 id 是
// **任意**用户字符串，只特判一个名字挡不住另外几条路——节点 id 叫 `none` 的会与「没有
// 来源」撞，字面量 id `node "seed"` 会与节点 `seed` 的输出撞（两条都实测撞上了）。
// 报错里得能分清「谁在跟谁抢」，来源名就必须**两两可分**；引擎内部的报错与公开
// `String()` 共用这一份，所以「引擎里分得开」与「日志里分得开」是同一条规则。
//
// 节点与外部来源撞名此前会被**错误接受**、根本不报错，所以这里没有「旧文案」要保：
// 变的是**节点来源**这一支（从裸 id 变成 `node "id"`）。
func sourceName(kind sourceKind, nodeID string) string {
	switch kind {
	case sourceInput:
		return "input"
	case sourceNode:
		return fmt.Sprintf("node %q", nodeID)
	default:
		return "none"
	}
}

// String 是报错用的来源名（与公开 `pulse.Source.String()` 同一份实现，见 `sourceName`）。
func (s source) String() string { return sourceName(s.kind, s.nodeID) }
