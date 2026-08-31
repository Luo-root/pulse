// Package candidate 是 P2-D3 的候选记忆管线：candidate extractor（宿
// 主注入 seam）→ 去重 → Pending 入库 → 审批晋升/否决。
//
// # 定位（§17 决议 7 / ASI06）
//
// 自动记忆只写 candidate（StatusPending），经过 provenance/taint/重复/
// 审批策略才成为 active item；未过审批不得晋升 active。Pending 候选对
// assemble/selfedit 检索**天然不可见**（store.Search 默认只 Active）——
// 渐进披露由 store 状态机免费保证。本包默认关：无后台循环、无自动触发
// （调用时机归宿主；background reflection 归 D4）。
//
// # 不变式
//
//   - 审批 = 既有状态机，store 契约零改动：approve = Supersede 为
//     Active 版（旧候选 Superseded 留痕、批准版新 ID、Confidence=1.0
//     即宿主背书）；reject = Revoke（reason 落审计）；非 Pending 一律
//     ErrNotPending（fail closed）；
//   - 模型参数最小化：extractor 返回的 item 只取 Kind/Content/Structured
//     ——namespace/status/taint/source/ID 由 Pipeline 钉死（selfedit
//     哲学）；
//   - 门禁（ASI06）：候选默认 TaintUntrustedExt（可覆盖）；SourceRefs
//     强制来自 OriginFn 会话回链；批准晋升不改 taint（审批是晋升闸，
//     taint 是数据属性）；
//   - 去重 v1 保守口径：归一后已有 item 的 Content 包含候选 → 丢弃
//     （子串冗余即重复，超集不拦——超集信息归 Supersede 修订语义）；
//   - 可解释：Extract 返回 Report 计数（Extracted/Stored/Duplicates/
//     Invalid）——禁止静默丢，D4 指标票的雏形。
//
// # 接入姿态
//
//	p, _ := candidate.New(candidate.Options{
//	    Store: memStore, Extractor: myLLMExtractor,
//	    Namespace: scope.Namespace(),
//	    OriginFn:  func() store.SourceRef { … }, // 当前 session 回链
//	})
//	stored, report, _ := p.Extract(ctx, surface) // 会话末/每 N 轮由宿主调
//	pending, _ := p.Pending(ctx)                 // 宿主审批面列表
//	active, _ := p.Approve(ctx, pending[0].ID)   // 或 p.Reject(ctx, id, reason)
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md §6.5/
// §12 P2-D/§17.6；实现票 #90（D3）。
package candidate
