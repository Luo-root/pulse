// Package reflection 是 P2-D4 的可配置 background reflection（§10.3）：
// 输入截断（预算门）→ candidate 提炼 → 计数 → 审计结果。
//
// # 定位（§10.3 / §17.5）
//
// 反思是异步巩固的最小编排面：宿主在会话末/每 N 轮/空闲钩子调用
// Reflect，把已结束的 surface 喂进 candidate.Pipeline 提炼候选。**输出
// 只到候选**（StatusPending 入库）——不自动 Approve/Reject，审批人盖章
// （HITL 立场）。本包默认关：无后台循环、无计时器、无自动触发（不
// New 不运行、零成本）；§10.3 的并发上限由调用方控制（同步单次执行，
// 包内 -race 安全）。
//
// # 不变式
//
//   - 输入预算可配置：Options.MaxInputChars（rune 计，口径对齐
//     compaction.CharMeter）超限从头部丢弃**整条消息**（尾部保留——提
//     取看近期内容；至少保最后一条；不截半条消息，tool pairing 完整、
//     多字节字符不截半）；
//   - 模型路由 = Pipeline 的 Extractor seam（宿主注入——本包不重复
//     注入、不提供默认实现）；
//   - 不 import session（surface 由宿主取出喂入——compaction 依赖
//     session 是因为要 fold/写回，本包只读输入，零依赖更薄）、不
//     import observability（审计 = ReflectionResult 返回值，宿主桥）；
//   - 错误透传不静默；错误轮不计数（计数只反映完整成功轮）；
//   - token 成本 v1 = Runs + 字符数；真实 LLM usage 归宿主 client
//     （compaction.request.usage 同口径——装配层桥）。
//
// # 指标面（D4 六项指标的本包部分）
//
// Reflect 提供 token 成本的运行计数（Runs/TotalInputChars/
// TruncatedChars）；提炼率/批准率/撤销率/污染拒绝率见 candidate.Metrics，
// 召回命中见 index.Counted——三处快照即 D4 指标面全貌（不建独立 metrics
// 聚合包，票 #92 定案）。
//
// # 接入姿态
//
//	r, _ := reflection.New(reflection.Options{
//	    Pipeline:      cand,          // candidate.Pipeline（必填）
//	    MaxInputChars: 8000,          // 0 = 不限
//	})
//	surface, _ := sess.Surface(ctx)    // 宿主从 session 取 surface
//	res, err := r.Reflect(ctx, surface) // 会话末/每 N 轮由宿主调
//	// res.Items = 本轮入库 Pending 候选（宿主审批面展示）
//	// res.Report / res.InputChars / res.TruncatedChars = 审计计数
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md §10.3/
// §12 P2-D/§17.5；实现票 #92（D4，P2 收口）。
package reflection
