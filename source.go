package pulse

import "fmt"

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

// String 是报错用的来源名：外部来源 `seed`、节点来源 `node %q`、没有来源 `none`。
//
// 节点来源**一律**带类型前缀并加引号，不是只给 `seed` 这个哨兵名字开小灶：节点 id 是
// **任意**用户字符串，只特判一个名字挡不住另外几条路——节点 id 叫 `none` 的会与「没有
// 来源」撞，字面量 id `node "seed"` 会与节点 `seed` 的输出撞（两条都实测撞上了）。
// 报错里得能分清「谁在跟谁抢」，来源名就必须**两两可分**。
//
// 节点与外部来源撞名此前会被**错误接受**、根本不报错，所以这里没有「旧文案」要保：
// 变的是**节点来源**这一支（从裸 id 变成 `node "id"`）。
func (s source) String() string {
	switch s.kind {
	case sourceSeed:
		return "seed"
	case sourceNode:
		return fmt.Sprintf("node %q", s.nodeID)
	default:
		return "none"
	}
}
