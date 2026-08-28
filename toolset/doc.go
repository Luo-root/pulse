// Package toolset 是 pulse v2 的可逆工具注册面（Accepted：docs/design/toolset-v1-design.md）。
//
// 分层（路径 A）：
//
//	来源插件（本地 / 未来 MCP …）
//	    │ Registry.Register / DisposeSource
//	    ▼
//	Registry（本包，kernel 服务 pulse.tools）
//	    │ AsToolSet()
//	    ▼
//	loop.ToolSet（loop.Agent 唯一消费面）
//
// 本包负责注册、来源归属、可逆 Effect 与宿主侧元数据（Source / Risk）。
// 模型回合、HITL 审批与执行后轨迹仍归 loop 的 before_tool_call /
// after_tool_call（Local 事件）。不引入第二套 before_execute 总线。
//
// 依赖方向写死：toolset import loop；loop 不 import toolset。
package toolset
