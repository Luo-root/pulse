# memory/candidate

P2-D3 的候选记忆管线（Issue #90）：candidate extractor（宿主注入 seam）→ 去重 → Pending 入库 → 审批晋升/否决。**本包默认关**——无后台循环、无自动触发（调用时机归宿主；background reflection 归 D4）。

设计立场（§17 决议 7 / ASI06）：自动记忆只写 candidate，经过 provenance/taint/重复/审批策略才成为 active item；未过审批不得晋升 active。包文档（godoc）见 `doc.go`。

## 接入

```go
p, err := candidate.New(candidate.Options{
    Store:     memStore,
    Extractor: myLLMExtractor,            // 必填：LLM 提取协议归宿主 prompt（seam）
    Namespace: scope.Namespace(),         // 必填：候选作用域（scope 防污染同 selfedit）
    OriginFn:  func() store.SourceRef { … }, // 必填：当前 session 回链
    // Taint: store.TaintUntrustedExt,    // 默认（ASI06：自动提炼自工具/外部内容）
})

stored, report, err := p.Extract(ctx, surface) // 会话末/每 N 轮由宿主调
// report = {Extracted, Stored, Duplicates, Invalid}——可解释计数
pending, err := p.Pending(ctx)                 // 宿主审批面列表
active, err := p.Approve(ctx, pending[0].ID)   // 晋升
err = p.Reject(ctx, pending[0].ID, "noisy")    // 否决（reason 落审计）
```

## 关键语义

- **审批 = 既有状态机，store 契约零改动**：approve = `Supersede`（旧候选 Superseded 留痕、批准版新 ID Active、`Confidence=1.0` 即宿主背书、SourceRefs 继承 + manual 审批标记——审批动作在 provenance 显式可辨）；reject = `Revoke`。非 Pending 一律 `ErrNotPending`（fail closed）。
- **审批作用域 = namespace 完全相等**（selfedit 写权限同口径）：`Pending` 不列出越界候选；`Approve`/`Reject` 对越界 item 一律 `ErrOutsideScope`（父 scope 不得下钻操作子 scope 候选）。
- **不可见性免费拿**：Pending 候选对 `store.Search`（默认只 Active）不可见——未批准不进 assemble/selfedit 上下文；批准后自然出现。
- **模型参数最小化**：extractor 返回的 item 只取 Kind/Content/Structured；namespace/status/taint/source/ID 由 Pipeline 钉死。
- **门禁（ASI06）**：候选默认 `TaintUntrustedExt`（可覆盖）；SourceRefs 强制 OriginFn 会话回链；批准晋升**不改 taint**（审批是晋升闸，taint 是数据属性）。
- **去重 v1 保守口径**：归一（小写 + 空白收紧）后已有 item 的 Content 包含候选 → 丢弃（子串冗余即重复，**超集不拦**——超集归 Supersede 修订语义）；判定在**内存双归一**完成（存量/候选两侧同口径）——store 的 ASCII 折叠不收紧空白，粗筛查询会漏拦存量侧脏数据；向量相似度去重不做（阈值语义未定，后续票）。
- **可解释**：`Report{Extracted, Stored, Duplicates, Invalid}` 计数——禁止静默丢；去重查询失败中断批次（store 故障宁可失败让宿主重试）。

## 指标面（D4）

`Pipeline.Metrics()` 返回累计动作计数快照（atomic，-race 安全）：`{Extracted, Stored, Duplicates, Invalid, Approved, Rejected, RejectedUntrusted}`——提炼率/批准率/撤销率/污染拒绝率的数据源（率值计算归宿主或展示层）。计数只在动作**完整成功**时累计（错误中断的批次不计——重试成功后完整计一轮）。`RejectedUntrusted` 仅 `TaintUntrustedExternal` 档计入（user-supplied 被拒不算外部污染闸实证，票 #92 补强口径）。

批次内重复同样去重：判定集 = 存量归一快照 + **本轮已入库候选**（随入库追加）——快照不含本轮写入，缺后者批次内重复会漏拦。

D4 六项指标的另两处：`reflection.Metrics`（token 成本运行计数）+ `index.Counted`（召回命中）。

## 测试

```bash
go test -race -count=1 ./memory/candidate/...
```
