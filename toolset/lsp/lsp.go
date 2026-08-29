// Package lsp 把外部语言服务器（gopls、typescript-language-server、pyright…）
// 挂成可注册的只读工具：诊断、定义跳转、引用、悬停。
//
// 与 builtins 平级的可选包：依赖外部进程、按语言显式配置，不进 builtins
// 开箱清单。单工具多 op；server 进程 lazy 启动、常驻到 dispose，
// scope Dispose 兜底（独立 Effect）。
package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

// 默认上限（产品参数，不是行业标准）。
const (
	DefaultTimeout    = 30 * time.Second
	DefaultDiagWindow = 3 * time.Second
	DefaultSourcePrefix = "lsp"
)

// Options 控制 lsp 装配与 server 生命周期。
type Options struct {
	// Root 是 workspace 根（initialize rootUri 与路径解析基准）。必填。
	Root string
	// Servers 映射文件扩展名 → server 启动命令，如 {".go": "gopls"}。必填非空。
	// 命令按空格分词，不支持引号路径。
	Servers map[string]string
	// Timeout 是单个 LSP 请求（含 initialize）的超时。默认 30s。
	Timeout time.Duration
	// DiagWindow 是 diagnostics 等待 server push 的窗口。默认 3s。
	DiagWindow time.Duration
	// SourcePrefix 写入 Registration.Source，默认 "lsp"。
	SourcePrefix string
}

func (o Options) withDefaults() (Options, error) {
	if strings.TrimSpace(o.Root) == "" {
		return Options{}, fmt.Errorf("lsp: Root is required")
	}
	abs, err := filepath.Abs(o.Root)
	if err != nil {
		return Options{}, fmt.Errorf("lsp: resolve Root: %w", err)
	}
	o.Root = abs
	if len(o.Servers) == 0 {
		return Options{}, fmt.Errorf("lsp: Servers is required (map file extension to server command, e.g. {\".go\": \"gopls\"})")
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.DiagWindow <= 0 {
		o.DiagWindow = DefaultDiagWindow
	}
	if o.SourcePrefix == "" {
		o.SourcePrefix = DefaultSourcePrefix
	}
	return o, nil
}

// manager 持有按扩展名懒启动的 server。
type manager struct {
	opt     Options
	mu      sync.Mutex
	servers map[string]*server
}

func (m *manager) killAll() {
	m.mu.Lock()
	ss := make([]*server, 0, len(m.servers))
	for _, s := range m.servers {
		ss = append(ss, s)
	}
	m.mu.Unlock()
	for _, s := range ss {
		s.shutdownAndKill()
	}
}

// serverFor 按扩展名取（或懒启动）server。启动/握手失败不缓存，下次重试。
func (m *manager) serverFor(ctx context.Context, ext string) (*server, error) {
	command, ok := m.opt.Servers[ext]
	if !ok {
		exts := make([]string, 0, len(m.opt.Servers))
		for e := range m.opt.Servers {
			exts = append(exts, e)
		}
		sort.Strings(exts)
		return nil, fmt.Errorf("lsp: no server configured for %q files (configured: %s)", ext, strings.Join(exts, ", "))
	}
	m.mu.Lock()
	s := m.servers[ext]
	m.mu.Unlock()
	if s != nil {
		return s, nil
	}

	sp, err := spawnServer(ctx, command, m.opt.Root)
	if err != nil {
		return nil, fmt.Errorf("lsp: start %q: %w", command, err)
	}
	s = newServer(ext, command, sp)

	m.mu.Lock()
	if existing := m.servers[ext]; existing != nil {
		m.mu.Unlock()
		s.shutdownAndKill() // 并发双启动，退让给先到者
		return existing, nil
	}
	m.servers[ext] = s
	m.mu.Unlock()

	ctx2, cancel := context.WithTimeout(ctx, m.opt.Timeout)
	defer cancel()
	if err := s.initialize(ctx2, m.opt.Root); err != nil {
		m.mu.Lock()
		delete(m.servers, ext)
		m.mu.Unlock()
		s.shutdownAndKill()
		return nil, fmt.Errorf("lsp: initialize %s: %w%s", command, err, s.stderrHint())
	}
	return s, nil
}

// stderrHint 取语言服务器 stderr 的保尾快照（排障用），最多 512 字节。
func (s *server) stderrHint() string {
	if s.sp.stderr == nil {
		return ""
	}
	tail := strings.TrimSpace(s.sp.stderr())
	if tail == "" {
		return ""
	}
	if len(tail) > 512 {
		tail = tail[len(tail)-512:]
	}
	return "; server stderr: " + tail
}

type env struct {
	m *manager
}

// Register 将 lsp 工具登记到 reg；返回统一 dispose（含杀 server 进程树）。
func Register(scope *kernel.Context, reg *toolset.Registry, opt Options) (func(), error) {
	if scope == nil {
		return nil, fmt.Errorf("lsp: nil scope")
	}
	if reg == nil {
		return nil, fmt.Errorf("lsp: nil registry")
	}
	opt, err := opt.withDefaults()
	if err != nil {
		return nil, err
	}
	m := &manager{opt: opt, servers: make(map[string]*server)}
	e := &env{m: m}

	r := e.regLSP()
	r.Source = opt.SourcePrefix + ".lsp"
	d, err := reg.Register(scope, r)
	if err != nil {
		return nil, fmt.Errorf("lsp: register: %w", err)
	}
	// scope Dispose 兜底：宿主忘记显式 dispose 也要杀 server（幂等）。
	if _, err := scope.Effect(func() (func(), error) {
		return m.killAll, nil
	}); err != nil {
		d()
		return nil, fmt.Errorf("lsp: register cleanup: %w", err)
	}
	return func() {
		m.killAll()
		d()
	}, nil
}

var lspOps = []string{"diagnostics", "definition", "references", "hover"}

func (e *env) regLSP() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name: "lsp",
			Description: "Query a language server for the given file (requires the extension to be configured in Options.Servers). " +
				"ops: diagnostics (compiler errors/warnings for the file), definition (location where the symbol at line/column is defined), " +
				"references (locations referencing it; set include_declaration for the declaration itself), hover (type/signature docs). " +
				"line/column are 0-based; column counts UTF-16 code units as per LSP.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "op":{"type":"string","enum":["diagnostics","definition","references","hover"]},
    "path":{"type":"string","description":"File path (relative to workspace Root or absolute under Root)"},
    "line":{"type":"integer","description":"0-based line (definition/references/hover)","minimum":0},
    "column":{"type":"integer","description":"0-based column in UTF-16 code units (definition/references/hover)","minimum":0},
    "include_declaration":{"type":"boolean","description":"references: include the declaration itself"}
  },
  "required":["op","path"]
}`),
		},
		Fn:        e.lspTool,
		Risk:      toolset.RiskReadonly,
		PreviewFn: e.previewLSP,
	}
}

func (e *env) previewLSP(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
	var p struct {
		Op   string `json:"op"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolset.Preview{}, fmt.Errorf("lsp: invalid args: %w", err)
	}
	return toolset.Preview{
		Kind:    toolset.KindOpaque,
		Action:  toolset.ActionRead,
		Subject: fmt.Sprintf("lsp %s %s", p.Op, p.Path),
		Opaque: &toolset.OpaqueChange{
			Summary: fmt.Sprintf("query language server: %s %s", p.Op, p.Path),
		},
	}, nil
}

type lspArgs struct {
	Op                 string `json:"op"`
	Path               string `json:"path"`
	Line               int    `json:"line"`
	Column             int    `json:"column"`
	IncludeDeclaration bool   `json:"include_declaration"`
}

func (e *env) lspTool(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p lspArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("lsp: invalid args: %w", err)
	}
	valid := false
	for _, op := range lspOps {
		if p.Op == op {
			valid = true
			break
		}
	}
	if !valid {
		return "", fmt.Errorf("lsp: unknown op %q (want one of: %s)", p.Op, strings.Join(lspOps, ", "))
	}
	if strings.TrimSpace(p.Path) == "" {
		return "", fmt.Errorf("lsp: path is required")
	}
	abs, err := resolveUnderRoot(e.m.opt.Root, p.Path)
	if err != nil {
		return "", err
	}
	if !withinRoot(e.m.opt.Root, abs) {
		return "", fmt.Errorf("lsp: path escapes Root: %s", abs)
	}
	ext := strings.ToLower(filepath.Ext(abs))
	if ext == "" {
		return "", fmt.Errorf("lsp: file %s has no extension; cannot pick a server", abs)
	}
	srv, err := e.m.serverFor(ctx, ext)
	if err != nil {
		return "", err
	}
	if err := srv.ensureOpen(ctx, abs, ext); err != nil {
		return "", err
	}

	ctx2, cancel := context.WithTimeout(ctx, e.m.opt.Timeout)
	defer cancel()

	switch p.Op {
	case "diagnostics":
		return srv.diagnostics(ctx2, abs, e.m.opt.DiagWindow)
	case "definition":
		return srv.definition(ctx2, abs, p.Line, p.Column)
	case "references":
		return srv.references(ctx2, abs, p.Line, p.Column, p.IncludeDeclaration)
	case "hover":
		return srv.hover(ctx2, abs, p.Line, p.Column)
	}
	return "", fmt.Errorf("lsp: unknown op %q", p.Op)
}
