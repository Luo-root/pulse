# memory/selfedit

P2-C 的 self-edit 记忆工具组（实现票 #82）：把 `memory/store` 写路径适配成三个模型可见工具，**显式 opt-in 注册**（不进任何默认装配，是否放给模型由宿主决定）。
包文档（godoc）见 `doc.go`；设计事实源 [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §12/§17；前置 #56（W2 Preview 已实现）。

## 接入

```go
dispose, err := selfedit.Register(scope, reg, selfedit.Options{
    Store:     memStore,
    Namespace: scope.Namespace(),           // store.MemoryScope 展开——工具唯一作用域
    OriginFn:  func() store.SourceRef {     // 当前 session 回链（实时 Seq）
        return store.SourceRef{Type: store.SourceSession, SessionID: sessID, Seq: currentSeq()}
    },
    // Taint: store.TaintUntrustedExt,      // 默认（ASI06 对位）；可信写手显式升 trusted
    // NewID:  myIDGen,                     // 默认 crypto/rand 16B hex
    // Source: "memory.selfedit",           // Registration.Source 元数据
})
```

审批归宿主 HITL（`before_tool_call`）：三工具 RiskReadWrite + PreviewFn（#56 W2 面），本包不做自动批准白名单。

## 三个工具

| 工具 | 模型参数（schema 全集） | 效果 |
|---|---|---|
| `memory_put` | `kind` / `content` / `structured?` | 新建 Active 记忆（Confidence=1.0、ID 自动分配、回链 OriginFn） |
| `memory_supersede` | `id` / `content` / `kind?` | 替换（旧条目留痕 superseded；kind 缺省沿用；structured 不继承；next 保持原 item namespace） |
| `memory_revoke` | `id` / `reason` | 作废（幂等；对 superseded 目标拒绝；reason 进 store 审计） |

## 不变式

- **scope 防污染**（§17.1 Letta 失效模式对位）：namespace/来源/信任级/置信度/状态/revision 全部 env 钉死，模型给不了也改不了——scope 是存储层边界，不是提示词约定。
- **写权限口径**（复审定案）：supersede/revoke 先 `Get(env.ns, id)`（不可见即不存在），且要求 item.Namespace 与 env.Namespace **完全相等**——store 前缀可见性是读口径（向下可见），写入钉死 env.ns：父 scope 工具不得下钻改写子 scope item（`ErrOutsideScope`）。宿主要管子 scope 记忆 = 按目标 scope 各配一个 env（组合，而非放权）。
- **taint 保守默认**：写入默认 `TaintUntrustedExt`（§17.7 ASI06 对位——self-edit 是模型把工具输出/外部内容复述进记忆的通道，不得默认与宿主权威写入同级；`before_tool_call` 审批是晋升闸，taint 是诚实的数据属性；可信写手宿主显式覆盖）。
- **回链强制**：写入 SourceRefs 只来自 `OriginFn()`；缺省 Register 直接失败，不静默降级为无来源。
- **只写不读**：没有检索工具——读取归 `memory/assemble`（§8）/宿主管线，避免「记忆编辑替代回答」。模型写入的生效路径 = 下轮 Assemble / RefreshStable。
- **禁物理 DELETE**：只有 supersede（留痕替代）与 revoke（幂等作废）；状态机错误原样透传 store 哨兵（`ErrSupersedeRevoked` / `ErrRevokeSuperseded` / `ErrItemNotFound`）。
- **Preview 只读**：三工具 opaque/write 卡片（envelope #56 已冻结，不加第五种 kind）；Preview 不落盘。

## 测试

```bash
go test -race -count=1 ./memory/selfedit/...
```
