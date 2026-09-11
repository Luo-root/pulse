// Package memory 是记忆层的根级装配门面：把子包（session / store /
// assemble）组合成开箱可用的「会话栈」与「条目栈」。
//
// 装配分两层（#156）：**最泛化构造 = 全参数注入**（任意 SessionStore /
// MemoryStore 实现，含宿主自定义持久化——Postgres、Redis 等实现接口后
// 直接注入，门面零改动），**便捷封装 = 基于最泛化的推荐默认**（内存 /
// JSONL）。
//
// 本包只 import memory 自己的子包，不跨包（碰 loop / toolset 的跨包
// 接线归 host 装配层）。
package memory

import (
	"context"

	"github.com/Luo-root/pulse/memory/assemble"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/memory/store"
)

// ---- 会话栈 ----

// NewSessionStack 是会话栈的**最泛化构造**：注入任意 session.SessionStore
// 实现（官方内存 / JSONL，或宿主自定义持久化）。
func NewSessionStack(store session.SessionStore) *SessionStack {
	return &SessionStack{store: store}
}

// NewMemorySessionStack 便捷封装：进程内内存 store（零依赖起步，重启即失）。
func NewMemorySessionStack() *SessionStack {
	return NewSessionStack(session.NewMemoryStore())
}

// NewJSONLSessionStack 便捷封装：JSONL 落盘 store（blobs 溢出 + 文件锁 +
// Flush fsync）。JSONL 是明文：文件即密钥面，路径宿主拥有。
func NewJSONLSessionStack(dir string) (*SessionStack, error) {
	st, err := session.NewJSONLStore(dir)
	if err != nil {
		return nil, err
	}
	return NewSessionStack(st), nil
}

// SessionStack 是会话侧的基础装配：暴露 Create / Open / 底层 store
// （导出/导入、列表、恢复策略等完整接口面经 Store() 取用）。
//
// compaction 是函数式编排（memory/compaction 的 Compact），触发时机归宿
// 主，不在门面内——各组件默认关、按需装配。
type SessionStack struct {
	store session.SessionStore
}

// Create 新建会话（header 零值即可，SessionID 缺省时由 store 生成）。
func (s *SessionStack) Create(ctx context.Context, header session.SessionHeader) (session.Session, error) {
	return s.store.Create(ctx, header)
}

// Open 打开会话（live 或冷恢复，语义同底层 store）。
func (s *SessionStack) Open(ctx context.Context, id string) (session.Session, error) {
	return s.store.Open(ctx, id)
}

// Store 暴露底层 store——完整接口面经此取用；门面只收敛构造，不收窄能力。
func (s *SessionStack) Store() session.SessionStore { return s.store }

// ---- 条目栈 ----

// NewItemStack 是条目栈的**最泛化构造**：注入任意 store.MemoryStore 实现
// + assembler 的 meter 与预算。SQLite / FTS 用户注入 store 包的
// build-tag 构造产物即可（门面不锁死 build tag）。
func NewItemStack(store store.MemoryStore, meter assemble.TokenCounter, budget assemble.Budget) *ItemStack {
	return &ItemStack{
		Store:     store,
		Assembler: assemble.NewDefaultAssembler(store, meter, budget),
	}
}

// NewMemoryItemStack 便捷封装：内存 store（meter nil = 不计费快照）。
func NewMemoryItemStack(budget assemble.Budget) *ItemStack {
	return NewItemStack(store.NewMemoryStore(), nil, budget)
}

// ItemStack 是长期记忆条目侧的基础装配：store + assembler。index /
// candidate 是可选件（EmbeddingProvider / Extractor seam 无默认实现），
// 按需另行接线。
type ItemStack struct {
	Store     store.MemoryStore
	Assembler *assemble.DefaultAssembler
}

// Assemble 透传 assembler 的上下文组装（省一层 .Assembler.）。
func (s *ItemStack) Assemble(ctx context.Context, in assemble.AssembleInput) (assemble.AssembledContext, error) {
	return s.Assembler.Assemble(ctx, in)
}
