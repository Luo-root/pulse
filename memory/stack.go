// Package memory 是记忆层的根级装配门面：把子包（session / store /
// assemble）的推荐默认组合成开箱可用的「会话栈」与「条目栈」。
//
// 本包只 import memory 自己的子包，不跨包（碰 loop / toolset 的跨包
// 接线归 host 装配层）。每个门面都是薄组合——不发明新抽象，只知道
// 「构造顺序」与「推荐默认值」。
package memory

import (
	"context"
	"errors"
	"fmt"

	"github.com/Luo-root/pulse/memory/assemble"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/memory/store"
)

// ---- 会话栈 ----

// SessionOptions 是会话栈装配参数。零值 = 内存 store（零依赖起步）。
type SessionOptions struct {
	// Dir 非空时用 JSONL store（blobs 溢出 + 文件锁 + Flush fsync）；
	// 空 = 内存 store。JSONL 是明文：文件即密钥面，路径宿主拥有。
	Dir string
}

// SessionStack 是会话侧的基础装配：按 Dir 选 store，暴露 Create / Open /
// 底层 store（导出/导入、列表等完整接口面经 Store() 取用）。
//
// compaction 是函数式编排（memory/compaction 的 Compact），触发时机归宿
// 主，不在门面内——各组件默认关、按需装配。
type SessionStack struct {
	store session.SessionStore
}

// NewSessionStack 装配会话栈。Dir 非空时初始化 JSONL store（目录不存在
// 则创建），否则用内存 store。
func NewSessionStack(opt SessionOptions) (*SessionStack, error) {
	if opt.Dir == "" {
		return &SessionStack{store: session.NewMemoryStore()}, nil
	}
	st, err := session.NewJSONLStore(opt.Dir)
	if err != nil {
		return nil, fmt.Errorf("memory: jsonl store: %w", err)
	}
	return &SessionStack{store: st}, nil
}

// Create 新建会话（header 零值即可，SessionID 缺省时由 store 生成）。
func (s *SessionStack) Create(ctx context.Context, header session.SessionHeader) (session.Session, error) {
	return s.store.Create(ctx, header)
}

// Open 打开会话（live 或冷恢复，语义同底层 store）。
func (s *SessionStack) Open(ctx context.Context, id string) (session.Session, error) {
	return s.store.Open(ctx, id)
}

// Store 暴露底层 store——导出/导入（Seeder / ErrSeedUnsupported）、列表、
// 删除等完整接口面经此取用；门面只收敛构造默认，不收窄能力。
func (s *SessionStack) Store() session.SessionStore { return s.store }

// ---- 条目栈 ----

// ItemOptions 是长期记忆条目栈装配参数。
type ItemOptions struct {
	// Budget 透传给 assembler 的类预算；零值 = assembler 的零值预算
	// 语义（各类别不设上限时即全部注入，宿主按需收紧）。
	Budget assemble.Budget
	// Meter 是 assembler 的 token 计数 seam；nil = 不计费快照。
	Meter assemble.TokenCounter
}

// ItemStack 是长期记忆条目侧的基础装配：store + assembler 按构造顺序
// （store 先于 assembler）组合成默认栈。index / candidate 是可选件
// （EmbeddingProvider / Extractor seam 无默认实现），按需另行接线。
type ItemStack struct {
	Store     store.MemoryStore
	Assembler *assemble.DefaultAssembler
}

// NewItemStack 装配条目栈（内存 store；SQLite / FTS 用户直接用
// memory/store 的 build-tag 构造并自组 assembler——门面不锁死 build tag）。
func NewItemStack(opt ItemOptions) *ItemStack {
	st := store.NewMemoryStore()
	return &ItemStack{
		Store:     st,
		Assembler: assemble.NewDefaultAssembler(st, opt.Meter, opt.Budget),
	}
}

// Assemble 透传 assembler 的上下文组装（省一层 .Assembler.）。
func (s *ItemStack) Assemble(ctx context.Context, in assemble.AssembleInput) (assemble.AssembledContext, error) {
	return s.Assembler.Assemble(ctx, in)
}

// ErrStackClosed 是栈已关闭后的操作错误（会话栈当前无关闭语义，占位
// 供两层装配的宿主生命周期使用）。
var ErrStackClosed = errors.New("memory: stack closed")
