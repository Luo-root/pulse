// Package observability 是 pulse v2 的正式观测包：SpringBoot 式装配
// 日志的最小实现。
//
// # 分层纪律（双基座模型）
//
// 本包只 import kernel，绝不 import llm/loop/flow。它只认识两样东西：
// kernel 发出的装配期事实（typed 事件），以及下游的 Sink 出口。
// 运行期业务事件（token 计数、HITL 结果、节点耗时）由各事实归属包的
// 观测适配（llm.Observe / loop.Observe / flow.NewRecordObserver）折进
// Record 信封写同一 Sink——依赖箭头统一朝基座，全仓无逆向依赖
// （原伴生 bridge 包已废除）。
//
// # 接入姿态
//
// v1 仅一种：旁路事件 + 快照横幅 + Sink。
//
//	host := kernel.New()
//	// 必须最先 Use：完整装载轨迹的前提（kernel 事件不回放，
//	// 后装只能靠快照横幅兜底当前视图）。
//	if _, err := kernel.Use(host, observability.Bootstrap("host-1", sink)); err != nil { ... }
//	kernel.Use(host, llm.Plugin()) // 此后的每次状态迁移都会进 Sink
//
// # TraceID 生成
//
// D3 约定 TraceID 由宿主单一生成源注入：宿主每请求调用 NewTraceID
// 一次即构成单一生成源，也可以完全自带方案（宿主自有格式，如
// hostID 前缀 + 自增序号）。返回值无契约语义，消费方不要解析其结构。
//
// # 与运行期观测的关系
//
// token/HITL/flow 节点耗时等业务指标不在本包扩具名字段。各包观测
// 适配将它们折进 Record 信封（填 TraceID/Duration/Status/Attrs）写
// 同一 Sink，业务维度经 Attrs 进入，key 契约由事实归属包定义
// （llm.AttrModel、loop.AttrTool、flow.AttrNode）。「同一出口」=
// 同一 Sink 实现 ≠ Record 变万能袋。
//
// # 隐私边界
//
// Record 无 map[string]any 逃生舱：Attrs 的写入面只有泛型 Set（标量
// 约束 ~string|~int64|~float64|~bool），prompt、附件字节、密钥、思维
// 链无法通过字段进入。注意边界：Err 字符串来源于调用方传入的
// error——Bootstrap 仅记录 kernel 自产的错误（Apply 失败原因等）；
// 适配层不得把 provider 原始错误体直接塞入 Err，应传已分类的摘要。
//
// 设计全貌（决策记录 D1–D10 与被否方案）见
// docs/design/observability-v1-design.md；可实现契约见 Issue #16、#125、#127。
package observability
