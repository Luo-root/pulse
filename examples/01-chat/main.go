// 01-chat：装配链手写展开 + 统一消息词汇表。
//
// 运行：go run ./examples/01-chat
// 00 课手写了最小闭环，但把「Registry 怎么来的」藏在了 scripted 里；本课
// 把完整装配链**逐行展开**（demoapp.Open 对示范课封装过——这里是你自己
// 写一遍的机会）：llm.Plugin → adapter 注册 → 命名实例声明 → 打开。
// demoapp 只再提供两样非主线便利：.env 自动加载（import 即生效）与
// REPL 输入解析（/image /file 等命令不是本课主题）。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Luo-root/pulse/examples/internal/demoapp" // 仅用 REPL 壳与 .env 加载；装配链在本文件手写
	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/llm/anthropic"
	"github.com/Luo-root/pulse/llm/openai"
	"github.com/Luo-root/pulse/observability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "01-chat: %v\n", err)
		os.Exit(1)
	}
}

// demoConfig 是本课示例的模型配置（env 读取，非装配语义）。
type demoConfig struct {
	Provider string // openai | openai-responses | anthropic
	Model    string
	APIKey   string
	BaseURL  string
	Scripted bool
}

// loadDemoConfig 读环境变量；无 API Key 时回退 Scripted（课程不依赖凭据）。
func loadDemoConfig() demoConfig {
	getenv := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	cfg := demoConfig{
		Provider: getenv("PULSE_DEMO_PROVIDER", "openai"),
		Model:    os.Getenv("PULSE_DEMO_MODEL"),
		APIKey:   os.Getenv("PULSE_DEMO_API_KEY"),
		BaseURL:  os.Getenv("PULSE_DEMO_BASE_URL"),
	}
	if cfg.APIKey == "" {
		switch cfg.Provider {
		case "anthropic":
			cfg.APIKey = getenv("ANTHROPIC_API_KEY", getenv("PULSE_ANTHROPIC_API_KEY", ""))
			if cfg.BaseURL == "" {
				cfg.BaseURL = os.Getenv("PULSE_ANTHROPIC_BASE_URL")
			}
			if cfg.Model == "" {
				cfg.Model = getenv("PULSE_ANTHROPIC_MODEL", "claude-sonnet-4-5")
			}
		default:
			cfg.APIKey = getenv("OPENAI_API_KEY", getenv("PULSE_OPENAI_API_KEY", ""))
			if cfg.BaseURL == "" {
				cfg.BaseURL = os.Getenv("PULSE_OPENAI_BASE_URL")
			}
			if cfg.Model == "" {
				cfg.Model = getenv("PULSE_OPENAI_MODEL", "gpt-4o-mini")
			}
		}
	}
	cfg.Scripted = cfg.APIKey == ""
	return cfg
}

func run() error {
	cfg := loadDemoConfig()

	// ① 宿主作用域：一切插件与可逆效应挂在这里（00 课已见）。
	host := kernel.New()

	// ② 观测最先 Use（事件不回放；后装只有快照横幅）。
	sink := &observability.MemorySink{}
	if _, err := kernel.Use(host, observability.Bootstrap("chat-host", sink)); err != nil {
		host.Dispose()
		return err
	}

	// ③ llm.Plugin：把模型注册中心装载为服务（kernel.Get 取用）。
	//    Plugin 内部会创建私有子作用域挂事件——Registry 的观测走那里。
	if _, err := kernel.Use(host, llm.Plugin()); err != nil {
		host.Dispose()
		return err
	}
	reg, ok := kernel.Get(host, llm.ServiceKey)
	if !ok {
		host.Dispose()
		return fmt.Errorf("01-chat: llm registry not provided")
	}

	// ④ adapter 注册：把 provider 工厂登记为**可逆 Effect**——Dispose 时
	//    摘除，已打开的模型失效（00 课「卸载即还原」在模型层的落地）。
	if err := openai.Register(host, reg); err != nil {
		host.Dispose()
		return err
	}
	if err := anthropic.Register(host, reg); err != nil {
		host.Dispose()
		return err
	}

	// ⑤ 声明命名实例（Declare 只登记配置；Open 才构造并缓存）。业务代码
	//    只认名字 "main"——换 provider/模型改这一处配置，调用侧零改动。
	var model llm.ChatModel
	if cfg.Scripted {
		// Scripted 也经 Registry 注册并打开：穿过 observed 包装，让
		// before_generate / after_response 事件链对脚本路径同样成立。
		if _, err := reg.RegisterProvider(host, "scripted", func(llm.Config) (llm.ChatModel, error) {
			return llm.NewScripted(llm.Resp("（脚本响应）装配链已手写展开；把 OPENAI_API_KEY 配进仓库根 .env 换真实模型。")), nil
		}); err != nil {
			host.Dispose()
			return err
		}
		if err := reg.Declare("main", llm.Config{Provider: "scripted", Model: "scripted"}); err != nil {
			host.Dispose()
			return err
		}
	} else {
		if err := reg.Declare("main", llm.Config{
			Provider: cfg.Provider,
			Model:    cfg.Model,
			APIKey:   cfg.APIKey,
			BaseURL:  cfg.BaseURL,
		}); err != nil {
			host.Dispose()
			return err
		}
	}
	var err error
	model, err = reg.Open("main")
	if err != nil {
		host.Dispose()
		return err
	}

	// ⑥ Anthropic 线格式 MaxTokens 必填（nil → ErrBadRequest），而 loop 组
	//    请求不填该字段。装配层示范：挂 before_generate 仅在空值时注入
	//    默认（4096）——这是宿主的示范默认，不是给 loop 加请求 Option 面。
	//    挂在 Registry.EventScope（Plugin 的私有子作用域）；reqScope 装配
	//    是 02 课的主题。
	if err := demoapp.InstallAnthropicMaxTokensDefault(reg.EventScope()); err != nil {
		host.Dispose()
		return err
	}

	fmt.Printf("01-chat provider=%s model=%s scripted=%v host=%s\n",
		cfg.Provider, cfg.Model, cfg.Scripted, "chat-host")

	// ⑦ REPL 壳（demoapp：输入解析/多模态附件命令），回调里只做一件事：
	//    统一词汇表进 → Generate → 统一词汇表出。01 不存 history——
	//    每轮独立 Generate 是刻意收窄（多轮归 02）。
	return demoapp.Loop(os.Stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
		started := time.Now()
		resp, err := model.Generate(context.Background(), llm.NewRequest(msg))
		if err != nil {
			// 错误是 llm.Error 分类（provider 不支持该模态 → ErrBadRequest，
			// 例如 Anthropic + 音频）——demo 不在上层吞掉它，看到真实失败。
			return nil, err
		}
		fmt.Println(resp.Message.Text())
		fmt.Fprintf(os.Stderr, "chat turn duration_ms=%d finish=%s tokens=%d/%d\n",
			time.Since(started).Milliseconds(), resp.FinishReason,
			resp.Usage.InputTokens, resp.Usage.OutputTokens)
		return []*llm.Message{resp.Message}, nil
	}, func() int { return 0 })
}
