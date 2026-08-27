// Package observability 是 pulse v2 的正式观测包：SpringBoot 式装配
// 日志的最小实现。
//
// # 分层纪律（方案 A）
//
// 本包只 import kernel，绝不 import llm/loop/flow。它只认识两样东西：
// kernel 发出的装配期事实（typed 事件），以及下游的 Sink 出口。
// 运行期业务事件（token 计数、HITL 结果、节点耗时）由装配层桥
// （如 examples/demoapp/bridge.go）订阅后折进 Record 信封写同一 Sink，
// 不经过本包。
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
// # 与运行期观测的关系
//
// token/HITL/flow 节点耗时等业务指标不进本包。装配层桥将它们折进
// Record 信封（填 TraceID/Duration/Status）写同一 Sink；装不进信封的
// 指标走 SlogSink 附加键。「同一出口」= 同一 Sink 实现 ≠ Record 变
// 万能袋。
//
// # 隐私边界
//
// Record 无 Attributes map 或任意 kv 注入口：prompt、附件字节、密钥、
// 思维链无法通过字段进入。注意边界：Err 字符串来源于调用方传入的
// error——Bootstrap 仅记录 kernel 自产的错误（Apply 失败原因等）；
// 桥不得把 provider 原始错误体直接塞入 Err，应传已分类的摘要。
//
// 设计全貌（决策记录 D1–D6 与被否方案）见
// docs/design/observability-v1-design.md；可实现契约见 Issue #16。
package observability
