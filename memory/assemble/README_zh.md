# memory/assemble

P2-C3 的上下文装配层：把 stable memory、检索型记忆与 session surface 组装成带预算边界的模型请求序列。
包文档（godoc）见 `doc.go`；设计事实源 [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §8；实现票 #80。

## 接入

```go
a := assemble.NewDefaultAssembler(memStore, nil, assemble.Budget{
    StableMemoryTokens: 300,   // frozen profile/facts 的小固定预算
    RetrievedTokens:    200,   // 检索型记忆动态预算
    MaxSurfaceTail:     40,    // surface 尾部节点数检查
})
ac, err := a.Assemble(ctx, assemble.AssembleInput{
    Namespace: scope.Namespace(),
    Surface:   surface,       // session.Surface() 原样
    Query:     userText,      // 检索召回信号
    Injected:  injected,      // 本轮立即应用的记忆
    // RefreshStable: true,    // 显式重建稳定前缀缓存
})
// ac.Messages        = 稳定前缀 → surface → 检索 → injected
// ac.StablePrefixLen = 稳定前缀边界（provider 侧缓存用）
// ac.Diagnostics     = 每类省略/降级/缓存事件（预算可解释）
```

## 组装顺序（§8.1）

```
[稳定前缀] frozen profile + stable decisions（RoleUser 引用模板；缓存，§8.3）
[surface 尾部] checkpoint + recent surface（原样，不裁切）
[检索记忆] ranked + cited（Query 召回，预算内 top-k）
[injected] 本轮立即应用（无预算约束，紧贴当前消息）
```

- **预算按类**（§8.1 不是只给一个 max messages）：超限**省略并记 Diagnostics**，不静默丢；surface 超限**只诊断不裁切**（裁切归 compaction §9.1 / prune §9.2）。
- **stable snapshot（§8.3）**：同 namespace 二次组装命中缓存（不重查 store）；`RefreshStable` 显式重建；per-namespace 隔离；重建失败退回旧快照并记诊断。
- **引用模板**：每条注入记忆形如 `[memory:<kind> <id> (source: session s9#12)] <content>`——SourceRefs 可读化，模型不当无条件事实。

## 排序口径（P2-C）

确定性排序：untrusted-external 降权 → UpdatedAt 降序 → ID 升序。**不读 Confidence**（w_conf 权重 0，排序不得依赖没人写的值）；semantic/embedding 归 P2-D。`TokenCounter` 由宿主注入（nil = 字符/4 估算）；不 import compaction（meter 复用走装配层接线票）。

## 测试

```bash
go test -race -count=1 ./memory/assemble/...
```
