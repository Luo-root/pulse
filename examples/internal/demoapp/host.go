package demoapp

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/llm/anthropic"
	"github.com/Luo-root/pulse/llm/openai"
	"github.com/Luo-root/pulse/observability"
)

// hostSeq 保证同纳秒内多次 Open 的 hostID 仍唯一。
var hostSeq atomic.Uint64

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
}

// Host 是一次 demo 运行的 kernel 宿主。
type Host struct {
	Ctx      *kernel.Context
	Registry *llm.Registry
	Sink     *observability.MemorySink
	Peak     *FlowPeak
	Model    llm.ChatModel
	Flags    Flags

	hostID string        // 宿主生命周期稳定标识（装配/横幅用）
	seq    atomic.Uint64 // 每请求 trace_id 序号源
}

// HostID 返回宿主稳定标识。
func (h *Host) HostID() string { return h.hostID }

// NewTraceID 为每次用户请求生成独立 trace 标识（D3：与 hostID 分层）。
func (h *Host) NewTraceID() string {
	return fmt.Sprintf("%s-req-%d", h.hostID, h.seq.Add(1))
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

// Open 装配 kernel、Registry、观测（Bootstrap 最先 Use）和 ChatModel。
// scripted 非空时覆盖默认脚本响应。
func Open(flags Flags, scripted ...*llm.Response) (*Host, error) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	host := kernel.New()

	// D3 两层标识：hostID 稳定（宿主装配期），trace_id 每请求独立生成。
	hostID := fmt.Sprintf("host-%d-%d", time.Now().UnixNano(), hostSeq.Add(1))

	// observability.Bootstrap 必须最先 Use：kernel 事件不回放，
	// 后装只能靠快照横幅兜底当前视图，历史轨迹不保证。
	sink := &observability.MemorySink{}
	if _, err := kernel.Use(host, observability.Bootstrap(hostID, sink)); err != nil {
		host.Dispose()
		return nil, err
	}

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
	var model llm.ChatModel
	if flags.Scripted {
		if len(scripted) == 0 {
			scripted = []*llm.Response{llm.Resp("Pulse v2 用插件内核、模型词汇表和无状态 ReAct 回合组成。")}
		}
		// 经 Registry 注册并打开：脚本模型同样穿过 observed 包装，
		// 使 before_generate / after_response（及其上的桥事件）对脚本
		// 路径也成立，而不是绕开整个观测链。
		if _, err := reg.RegisterProvider(host, "scripted", func(llm.Config) (llm.ChatModel, error) {
			return llm.NewScripted(scripted...), nil
		}); err != nil {
			host.Dispose()
			return nil, err
		}
		if err := reg.Declare("main", llm.Config{Provider: "scripted", Model: "scripted"}); err != nil {
			host.Dispose()
			return nil, err
		}
		var err error
		model, err = reg.Open("main")
		if err != nil {
			host.Dispose()
			return nil, err
		}
	} else {
		if err := reg.Declare("main", llm.Config{
			Provider: flags.Provider,
			Model:    flags.Model,
			APIKey:   flags.APIKey,
			BaseURL:  flags.BaseURL,
		}); err != nil {
			host.Dispose()
			return nil, err
		}
		var err error
		model, err = reg.Open("main")
		if err != nil {
			host.Dispose()
			return nil, err
		}
	}
	// Anthropic MaxTokens 必填：loop 组请求不填该字段。
	// 无 reqScope 时 observed 回退 Registry.EventScope()（llm.Plugin
	// Apply 私有子 ctx）。EmitLocal 不向父冒泡——挂 host 根无效。
	// 每请求 NewBridge 还会在 reqScope 再挂一次。
	if err := InstallAnthropicMaxTokensDefault(reg.EventScope()); err != nil {
		host.Dispose()
		return nil, err
	}

	return &Host{
		Ctx: host, Registry: reg, Sink: sink, Peak: &FlowPeak{},
		Model: model, Flags: flags, hostID: hostID,
	}, nil
}

// defaultAnthropicMaxTokens 是装配层示范默认值，不是 loop/Agent 的 Option 面。
const defaultAnthropicMaxTokens = 4096

// InstallAnthropicMaxTokensDefault 在 scope 上挂 before_generate：仅当
// MaxTokens 为空时填默认。不区分 provider（OpenAI 有 MaxTokens 也无害）；
// 目的是让 anthropic 路径不被 ErrBadRequest 打穿。监听随 scope 销毁摘除。
func InstallAnthropicMaxTokensDefault(scope *kernel.Context) error {
	_, err := kernel.OnWaterfall(scope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			if req != nil && req.MaxTokens == nil {
				v := defaultAnthropicMaxTokens
				req.MaxTokens = &v
			}
			return next(req)
		})
	return err
}

// NewBridge 为一次请求创建观测桥（TraceID = NewTraceID），并把监听
// 安装到 scope（建议传每轮请求自己的子作用域；随其销毁自动摘除）。
func (h *Host) NewBridge(scope *kernel.Context) (*Bridge, error) {
	if err := InstallAnthropicMaxTokensDefault(scope); err != nil {
		return nil, err
	}
	b := &Bridge{Sink: h.Sink, HostID: h.hostID, TraceID: h.NewTraceID()}
	if err := b.install(scope); err != nil {
		return nil, err
	}
	return b, nil
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


