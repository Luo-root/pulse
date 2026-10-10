package pulse

// 本文件是引擎内部的**来源表**：某个 Key 由谁提供。
//
// 表里只有两种来源——外部 `Seed` / `SkipSeed`，或恰好一个节点。之所以不用字符串
// 哨兵（旧实现把外部来源记成 `"seed"`）：节点 ID 是**用户可控**的，一个真叫 `seed`
// 的节点会与哨兵撞名，两处判据同时失灵——
//
//  1. 它 `Provides` 某个 Key 之后还能再 `Seed` 同一个 Key（「一个 Key 一个来源」被
//     绕过，**静默**：宿主抢先占住槽位，下游读到宿主值，整图照样成功）；
//  2. 环检查按字符串查节点索引，会把「来自外部 Seed 的 Key」错认成这个节点提供的，
//     凭空造出一条边与一个不存在的环。
//
// 结构化的来源让这两种判据都只按 `kind` 走，名字再像也分得开。

// sourceKind 是来源的种类。零值 `sourceNone` **不得存入来源表**——表达「没有来源」
// 的永远是「表里没有这个 Key」，而不是表里有一个零值。
type sourceKind uint8

const (
	sourceNone sourceKind = iota // 只作为零值
	sourceSeed                   // 外部 Seed / SkipSeed
	sourceNode                   // 某个节点 Provides
)

// source 是一条来源记录。`nodeID` 只在 `sourceNode` 时有意义。
type source struct {
	kind   sourceKind
	nodeID string
}

// seedSource 是外部来源那一条（Seed / SkipSeed 共用）。
var seedSource = source{kind: sourceSeed}

// nodeSource 是某个节点提供的那一条。
func nodeSource(id string) source { return source{kind: sourceNode, nodeID: id} }

// present 报这条来源是否**真的存在**。零值按「没有来源」处理：它只该出现在
// 「表里没有这个 Key」的情形，万一落表了，所有判据都必须当它不存在——不能因为
// 表里有个格子就当成有来源（那会让等待它的节点永远停在 pending）。
func (s source) present() bool {
	return s.kind == sourceSeed || s.kind == sourceNode
}

// same 判两条来源是不是**同一个来源**：Seed 与 Seed 相同；同一个节点来源相同；
// Seed 与任何节点来源都不同（哪怕那个节点就叫 `seed`）。零值不与任何东西相同。
func (s source) same(t source) bool {
	if !s.present() || !t.present() || s.kind != t.kind {
		return false
	}
	return s.kind == sourceSeed || s.nodeID == t.nodeID
}

// String 是报错用的来源名：外部来源输出 `seed`，节点来源输出节点 id。
//
// 节点 id 恰好是 `seed` 时输出 `node "seed"`——否则报错里两个 `seed` 无从分辨，
// 而这条路径正是这次修的那个 bug。普通场景的文案逐字不变（那种撞名此前会被**错误
// 接受**，根本不报错，所以没有「旧文案」要保）。
func (s source) String() string {
	switch s.kind {
	case sourceSeed:
		return "seed"
	case sourceNode:
		if s.nodeID == "seed" {
			return `node "seed"`
		}
		return s.nodeID
	default:
		return "none"
	}
}
