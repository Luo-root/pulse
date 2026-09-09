// Package builtins 提供通用基础工具实现，经 toolset.Registry 挂入 pulse.tools。
//
// 中性命名：read / ls / glob / grep / exec / edit / write / apply_patch / web_fetch / web_search / question / job_output / job_kill。
// Skill ≠ Tool；本包不装载 Skills，也不另起执行总线。
//
// 路径：读写根分家。exec：Windows 用 PowerShell，Unix 用 sh。
//
// # 三层边界（#157）
//
// 边界内自由（路径/超时/env 白名单——exec 与后台 job 子进程默认只继承
// 平台必需键，Options.ExecEnv 追加、ExecEnvInheritAll 显式全量）→ 越界
// 审批（写前 diff 卡片走 before_tool_call，seam 在本包、默认接线归 host
// 装配层）→ 命令逃逸面（exec 的 shell 命令不受文件约束管，OS 级隔离归
// 宿主部署层）。不做降低实用性的死墙，对齐主流「边界内自由 + 越界审批
// + OS 兜底」模型。
package builtins
