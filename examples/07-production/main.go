// 07-production：生产形态聚合——多来源工具 + 反思 + 指标面。
//
// 运行：go run ./examples/07-production
// 两段演示：
//  1. 三路工具装配（sources.go）：本地 toolset 注册 + MCP Source（官方
//     go-sdk InMemory）+ Skills 装载器 → 同一 Registry → 一个聚合工具回合；
//  2. 反思与指标面（reflection.go）：会话末 reflection.Reflect（预算门 +
//     candidate 提炼）+ 三处指标快照（D4 六项指标全貌）。
//
// 本课聚合 00–06 的全部组件形态：宿主在生产里把「工具来源、会话记忆、
// 反思调度、观测桥」装到同一个 kernel 宿主上——各组件默认关、按需装配。
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "07-production: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// 段 1：多来源工具（本地 / MCP / Skills）。
	if err := RunSourcesDemo(); err != nil {
		return fmt.Errorf("sources demo: %w", err)
	}

	// 段 2：反思与指标面。
	if err := reflectionDemo(); err != nil {
		return fmt.Errorf("reflection demo: %w", err)
	}

	fmt.Println()
	fmt.Println("课程要点：生产装配 = 各组件显式 opt-in 组合（MCP/Skills 是 Source 不是插件魔法，")
	fmt.Println("反思默认关、触发归宿主；指标面 = 三处快照）。Pulse 的「默认关」让每一层都可独立缺席。")
	return nil
}
