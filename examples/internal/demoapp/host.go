package demoapp

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Luo-root/pulse/examples/internal/observability"
	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/llm/anthropic"
	"github.com/Luo-root/pulse/llm/openai"
)

// Flags 是三个 demo 共用的环境/CLI 配置。
// 用户输入一律走 REPL（Draft），这里不再承载一次性 Prompt/媒体入口。
type Flags struct {
	Provider  string
	Model     string
	APIKey    string
	BaseURL   string
	DenyTool  string // denylist 模式的拒绝名单；allowlist 模式忽略
	AllowTool string // allowlist 模式的白名单；空则仅 lookup
	HITL      string // 原始 PULSE_DEMO_HITL，便于横幅展示
	Scripted  bool
	TraceID   string
}

// Host 是一次 demo 运行的 kernel 宿主。
type Host struct {
	Ctx      *kernel.Context
	Registry *llm.Registry
	Reporter *observability.Reporter
	Model    llm.ChatModel
	Flags    Flags
}

func init() {
	loadDotEnv()
}

// loadDotEnv 从仓库根的 .env 读入尚未设置的环境变量。
// .env 已被 gitignore，专供本机真机冒烟；解析失败静默忽略。
func loadDotEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, ".env")
		data, err := os.ReadFile(p)
		if err == nil {
			applyDotEnv(data)
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func applyDotEnv(data []byte) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// LoadFlagsFromEnv 从环境变量填充默认配置。没有 API Key 时自动走 ScriptedModel。
func LoadFlagsFromEnv() Flags {
	provider := getenv("PULSE_DEMO_PROVIDER", "openai")
	f := Flags{
		Provider:  provider,
		Model:     os.Getenv("PULSE_DEMO_MODEL"),
		APIKey:    os.Getenv("PULSE_DEMO_API_KEY"),
		BaseURL:   os.Getenv("PULSE_DEMO_BASE_URL"),
		DenyTool:  os.Getenv("PULSE_DEMO_DENY_TOOL"),
		AllowTool: os.Getenv("PULSE_DEMO_ALLOW_TOOL"),
		HITL:      os.Getenv("PULSE_DEMO_HITL"),
		TraceID:   getenv("PULSE_DEMO_TRACE_ID", fmt.Sprintf("demo-%d", time.Now().UnixMilli())),
	}
	if f.APIKey == "" {
		switch f.Provider {
		case "anthropic":
			f.APIKey = firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("PULSE_ANTHROPIC_API_KEY"))
			if f.BaseURL == "" {
				f.BaseURL = os.Getenv("PULSE_ANTHROPIC_BASE_URL")
			}
			if f.Model == "" {
				f.Model = firstNonEmpty(os.Getenv("PULSE_ANTHROPIC_MODEL"), "claude-sonnet-4-5")
			}
		default:
			f.APIKey = firstNonEmpty(os.Getenv("OPENAI_API_KEY"), os.Getenv("PULSE_OPENAI_API_KEY"))
			if f.BaseURL == "" {
				f.BaseURL = os.Getenv("PULSE_OPENAI_BASE_URL")
			}
			if f.Model == "" {
				f.Model = firstNonEmpty(os.Getenv("PULSE_OPENAI_MODEL"), "gpt-4o-mini")
			}
		}
	}
	if f.APIKey == "" {
		f.Scripted = true
		if f.Model == "" {
			f.Model = "scripted"
		}
	}
	return f
}

// Open 装配 kernel、Registry、观测插件和 ChatModel。
// scripted 非空时覆盖默认脚本响应。
func Open(flags Flags, scripted ...*llm.Response) (*Host, error) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	host := kernel.New()
	if _, err := kernel.Use(host, llm.Plugin()); err != nil {
		host.Dispose()
		return nil, err
	}
	reg, ok := kernel.Get(host, llm.ServiceKey)
	if !ok {
		host.Dispose()
		return nil, fmt.Errorf("demo: llm registry not provided")
	}
	if err := openai.Register(host, reg); err != nil {
		host.Dispose()
		return nil, err
	}
	if err := anthropic.Register(host, reg); err != nil {
		host.Dispose()
		return nil, err
	}
	sink := observability.SlogSink{Logger: slog.Default()}
	if _, err := kernel.Use(host, observability.Plugin(flags.TraceID, sink)); err != nil {
		host.Dispose()
		return nil, err
	}
	reporter, ok := kernel.Get(host, observability.ServiceKey)
	if !ok {
		host.Dispose()
		return nil, fmt.Errorf("demo: observability reporter not provided")
	}
	var model llm.ChatModel
	var err error
	if flags.Scripted {
		if len(scripted) == 0 {
			scripted = []*llm.Response{llm.Resp("Pulse v2 用插件内核、模型词汇表和无状态 ReAct 回合组成。")}
		}
		model = llm.NewScripted(scripted...)
	} else {
		if err = reg.Declare("main", llm.Config{
			Provider: flags.Provider,
			Model:    flags.Model,
			APIKey:   flags.APIKey,
			BaseURL:  flags.BaseURL,
		}); err != nil {
			host.Dispose()
			return nil, err
		}
		model, err = reg.Open("main")
		if err != nil {
			host.Dispose()
			return nil, err
		}
	}
	return &Host{Ctx: host, Registry: reg, Reporter: reporter, Model: model, Flags: flags}, nil
}

// Close 回收 kernel 作用域及全部效应。
func (h *Host) Close() {
	if h != nil && h.Ctx != nil {
		h.Ctx.Dispose()
	}
}

// GetRegistry 从宿主读取 llm.Registry 服务（缺省时 ok=false）。
func GetRegistry(h *Host) (*llm.Registry, bool) {
	if h == nil || h.Ctx == nil {
		return nil, false
	}
	return kernel.Get(h.Ctx, llm.ServiceKey)
}

// ObservabilityReporter 从宿主读取观测 Reporter（缺省时返回 nil）。
func ObservabilityReporter(h *Host) *observability.Reporter {
	if h == nil || h.Ctx == nil {
		return nil
	}
	r, ok := kernel.Get(h.Ctx, observability.ServiceKey)
	if !ok {
		return nil
	}
	return r
}
