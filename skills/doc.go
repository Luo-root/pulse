// Package skills 实现 Agent Skills 装载器（Accepted：docs/design/skills-v1-design.md I1）。
//
// Skill 是规程包（目录 + SKILL.md），经 List/Load/ReadFile 供装配层做
// 渐进披露。本包不执行脚本、不实现 loop.ToolSet、不往 pulse.tools
// 注册工具。格式对齐 https://agentskills.io/specification 。
package skills
