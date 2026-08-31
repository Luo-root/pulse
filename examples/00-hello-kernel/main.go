// 00-hello-kernel：Pulse 第一课——内核生命周期与第一次模型调用。
//
// 运行：go run ./examples/00-hello-kernel
// 本课刻意不经过 examples/internal/demoapp 的装配封装、不走 Registry
//（那是 01 课的内容）：只用 kernel + llm + observability 三个包，把
// 「宿主作用域 → 插件装载 → 模型调用 → 观测 → 卸载还原」的最小闭环
// 完整展开一遍。无需任何 API Key。
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/observability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "00-hello-kernel:", err)
		os.Exit(1)
	}
}

func run() error {
	// ① 创建宿主作用域：一切插件、服务、可逆效应都挂在它下面。
	host := kernel.New()

	// ② 观测插件必须最先 Use——kernel 事件不回放，后装的观测只能靠
	//    快照横幅兜底当前视图，装配历史轨迹不保证。
	sink := &observability.MemorySink{}
	if _, err := kernel.Use(host, observability.Bootstrap("hello-host", sink)); err != nil {
		host.Dispose()
		return err
	}

	// ③ 拿一个 ChatModel。本课用 llm.NewScripted（脚本响应）：不依赖
	//    任何 API Key、输出确定，先专注在内核与观测链路上。真实
	//    provider（openai/anthropic）经 Registry 装配是 01 课的主题。
	model := llm.NewScripted(
		llm.Resp("我是 Pulse 示例助手（脚本响应）。把 OPENAI_API_KEY 配进 .env 并上 01 课，就能换上真实模型。"),
	)

	// ④ 第一次模型调用：provider 中立词汇表进、词汇表出。无论底层是
	//    OpenAI、Anthropic 还是本课的脚本模型，请求与响应的结构一致。
	res, err := model.Generate(context.Background(), &llm.GenerateRequest{
		Messages: []*llm.Message{llm.UserText("用一句话介绍你自己")},
	})
	if err != nil {
		host.Dispose()
		return err
	}
	fmt.Println("模型回复：", res.Message.Text())
	fmt.Println()

	// ⑤ 读观测记录：Bootstrap 订阅了 kernel 的 fiber_state /
	//    loader_action 事件——插件装载、服务挂载的每一步都在这里。
	fmt.Printf("观测记录（%d 条）：\n", sink.Len())
	for _, r := range sink.Snapshot() {
		line := fmt.Sprintf("  [%s] %s", r.Source, r.Event)
		if r.FiberName != "" {
			line += fmt.Sprintf(" fiber=%s %s→%s", r.FiberName, r.From, r.To)
		}
		if r.PluginName != "" {
			line += fmt.Sprintf(" plugin=%s %s", r.PluginName, r.LoaderKind)
		}
		fmt.Println(line)
	}

	// ⑥ 卸载：Dispose 按 LIFO 还原全部效应（观测监听摘除、服务关闭）。
	//    卸载后 Sink 不再增长——「卸载即还原」可以在单测里断言
	//    （examples/internal/demoapp 的 demoapp_test.go 有现成示范）。
	host.Dispose()
	fmt.Println()
	fmt.Println("宿主已卸载，全部效应按 LIFO 还原。")
	return nil
}
