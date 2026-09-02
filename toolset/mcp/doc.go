// Package mcp 把 MCP server 适配为 toolset 工具来源（Accepted：toolset-v1 T3）。
//
// 本包只钉 Client 抽象与 Source 装载：ListTools → Register，CallTool →
// loop.ToolFunc，断开 → DisposeSource。transport / 协议细节由 Client
// 实现承担——mock 与官方 modelcontextprotocol/go-sdk 适配（gosdk.go）
// 均在本包；再换其它 Client 实现（如社区 mcp-go）只换 Client，不动
// Registry / loop。
//
// Skills 不是本包职责：Skill 是规程包，不是 Source 插件。
package mcp
