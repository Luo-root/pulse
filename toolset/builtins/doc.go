// Package builtins 提供通用基础工具实现，经 toolset.Registry 挂入 pulse.tools。
//
// 中性命名：read / ls / glob / grep / exec / edit / write / apply_patch / web_fetch / web_search / question / job_output / job_kill。
// Skill ≠ Tool；本包不装载 Skills，也不另起执行总线。
//
// 路径：读写根分家。exec：Windows 用 PowerShell，Unix 用 sh。
package builtins
