package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

// fakeConn 是内存 frameConn：记录 server 发出的帧，onSend 异步回调供 fake server 回帧。
type fakeConn struct {
	mu     sync.Mutex
	sent   [][]byte
	closed bool
	out    chan []byte
	onSend func(body []byte)
}

func newFakeConn() *fakeConn {
	return &fakeConn{out: make(chan []byte, 64)}
}

func (c *fakeConn) Send(body []byte) error {
	c.mu.Lock()
	c.sent = append(c.sent, append([]byte(nil), body...))
	c.mu.Unlock()
	if c.onSend != nil {
		go c.onSend(body)
	}
	return nil
}

func (c *fakeConn) Recv() ([]byte, error) {
	b, ok := <-c.out
	if !ok {
		return nil, io.EOF
	}
	return b, nil
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *fakeConn) frames() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.sent...)
}

// rpcFrame 是测试侧解析请求/通知的最小结构。
type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// fakeServer 模拟语言服务器：按 method 回帧，并记录协议序。
// handle 由用例注入；nil 时只记录不回帧。
type fakeServer struct {
	conn    *fakeConn
	handle  func(f rpcFrame)
	mu      sync.Mutex
	methods []string
	killed  atomic.Int32
}

func newFakeServer() *fakeServer {
	fs := &fakeServer{conn: newFakeConn()}
	fs.conn.onSend = func(body []byte) {
		var f rpcFrame
		if json.Unmarshal(body, &f) != nil {
			return
		}
		fs.mu.Lock()
		fs.methods = append(fs.methods, f.Method)
		fs.mu.Unlock()
		if fs.handle != nil {
			fs.handle(f)
		}
	}
	return fs
}

func (fs *fakeServer) reply(f rpcFrame, result interface{}) {
	b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: f.ID, Result: mustJSON(result)})
	fs.conn.out <- b
}

func (fs *fakeServer) replyErr(f rpcFrame, msg string) {
	b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: f.ID, Error: &rpcError{Code: -32603, Message: msg}})
	fs.conn.out <- b
}

func (fs *fakeServer) notify(method string, params interface{}) {
	b, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	fs.conn.out <- b
}

func (fs *fakeServer) methodSequence() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.methods...)
}

func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func waitForFrames(fs *fakeServer, want int) {
	t0 := time.Now()
	for {
		fs.mu.Lock()
		n := len(fs.methods)
		fs.mu.Unlock()
		if n >= want {
			return
		}
		if time.Since(t0) > 5*time.Second {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- 注册面 helper（同 builtins_test 形态）----

func lspSetup(t *testing.T, opt Options) (*toolset.Registry, func()) {
	t.Helper()
	host := kernel.New()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok {
		t.Fatal("no registry")
	}
	dispose, err := Register(host, reg, opt)
	if err != nil {
		host.Dispose()
		t.Fatal(err)
	}
	return reg, func() {
		dispose()
		host.Dispose()
	}
}

func lspCall(t *testing.T, reg *toolset.Registry, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := reg.AsToolSet().Execute(context.Background(), llm.ToolCall{
		ID: "t1", Name: "lsp", Arguments: b,
	})
	if err != nil {
		t.Fatalf("lsp: %v\n%s", err, out)
	}
	return out
}

func lspCallErr(t *testing.T, reg *toolset.Registry, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := reg.AsToolSet().Execute(context.Background(), llm.ToolCall{
		ID: "t1", Name: "lsp", Arguments: b,
	})
	if err == nil {
		t.Fatalf("want error, got %q", out)
	}
	return err.Error()
}

// injectFake 把 spawnServer 换成注入 fake 的实现；返回 kill 计数指针。
func injectFake(t *testing.T, fs *fakeServer, spawnErr error) *atomic.Int32 {
	t.Helper()
	orig := spawnServer
	spawnServer = func(ctx context.Context, command, dir string) (*serverProcess, error) {
		if spawnErr != nil {
			return nil, spawnErr
		}
		return &serverProcess{
			conn: fs.conn,
			kill: func() { fs.killed.Add(1) },
		}, nil
	}
	t.Cleanup(func() { spawnServer = orig })
	return &fs.killed
}

func lspOptions(root string, extra func(*Options)) Options {
	o := Options{
		Root:       root,
		Servers:    map[string]string{".go": "fake-lsp --stdio"},
		Timeout:    3 * time.Second,
		DiagWindow: 2 * time.Second,
	}
	if extra != nil {
		extra(&o)
	}
	return o
}

// ---- 用例 ----

func TestLSPDiagnosticsFlow(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "hello.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := fileURI(file)

	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		switch f.Method {
		case "initialize":
			fs.reply(f, map[string]any{"capabilities": map[string]any{}})
		case "textDocument/didOpen":
			var p didOpenParams
			if json.Unmarshal(f.Params, &p) != nil {
				return
			}
			if p.TextDocument.URI != uri {
				return
			}
			if p.TextDocument.Text != "package main\n\nfunc main() {}\n" {
				return
			}
			fs.notify("textDocument/publishDiagnostics", publishDiagParams{
				URI: p.TextDocument.URI,
				Diagnostics: []rawDiag{{
					Range:    lsRange{Start: lsPosition{Line: 2, Character: 5}},
					Severity: 1,
					Message:  "declared and not used: x",
					Source:   "fakegopls",
				}},
			})
		}
	}
	kills := injectFake(t, fs, nil)

	reg, cleanup := lspSetup(t, lspOptions(root, nil))
	defer cleanup()

	out := lspCall(t, reg, map[string]any{"op": "diagnostics", "path": "hello.go"})
	if !strings.Contains(out, "1 diagnostic(s)") ||
		!strings.Contains(out, "error") ||
		!strings.Contains(out, "declared and not used: x") ||
		!strings.Contains(out, "fakegopls") {
		t.Fatalf("out=%q", out)
	}
	if !strings.Contains(out, "3:6") {
		t.Fatalf("want 1-based position 3:6 in %q", out)
	}

	// 协议序：initialize → initialized → didOpen。
	seq := fs.methodSequence()
	init, initialized, didOpen := -1, -1, -1
	for i, m := range seq {
		switch m {
		case "initialize":
			init = i
		case "initialized":
			initialized = i
		case "textDocument/didOpen":
			didOpen = i
		}
	}
	if init < 0 || initialized < init || didOpen < initialized {
		t.Fatalf("protocol order broken: %v", seq)
	}
	if n := kills.Load(); n != 0 {
		t.Fatalf("dispose not yet called, kills=%d", n)
	}
	cleanup()
	if n := kills.Load(); n != 1 {
		t.Fatalf("dispose must kill server once, kills=%d", n)
	}
}

func TestLSPDefinitionReferencesHover(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "hello.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defURI := fileURI(filepath.Join(root, "other.go"))

	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		switch f.Method {
		case "initialize":
			fs.reply(f, map[string]any{})
		case "textDocument/definition":
			fs.reply(f, []location{{
				URI:   defURI,
				Range: lsRange{Start: lsPosition{Line: 9, Character: 5}},
			}})
		case "textDocument/references":
			var p referenceParams
			_ = json.Unmarshal(f.Params, &p)
			fs.reply(f, []location{
				{URI: fileURI(file), Range: lsRange{Start: lsPosition{Line: 0}}},
				{URI: defURI, Range: lsRange{Start: lsPosition{Line: 3}}},
			})
		case "textDocument/hover":
			fs.reply(f, map[string]any{
				"contents": map[string]any{"kind": "markdown", "value": "func main()"},
			})
		}
	}
	injectFake(t, fs, nil)

	reg, cleanup := lspSetup(t, lspOptions(root, nil))
	defer cleanup()

	out := lspCall(t, reg, map[string]any{"op": "definition", "path": "hello.go", "line": 2, "column": 6})
	if !strings.Contains(out, fmt.Sprintf("%s:10:6", uriPath(defURI))) {
		t.Fatalf("definition=%q", out)
	}

	out = lspCall(t, reg, map[string]any{"op": "references", "path": "hello.go", "line": 2, "column": 6, "include_declaration": true})
	if !strings.Contains(out, "2 reference(s)") || !strings.Contains(out, "hello.go:1:1") {
		t.Fatalf("references=%q", out)
	}

	out = lspCall(t, reg, map[string]any{"op": "hover", "path": "hello.go", "line": 2, "column": 6})
	if !strings.Contains(out, "func main()") {
		t.Fatalf("hover=%q", out)
	}
}

func TestLSPNoServerAndUnknownOp(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "x.py")
	if err := os.WriteFile(file, []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		if f.Method == "initialize" {
			fs.reply(f, map[string]any{})
		}
	}
	injectFake(t, fs, nil)

	reg, cleanup := lspSetup(t, lspOptions(root, nil))
	defer cleanup()

	msg := lspCallErr(t, reg, map[string]any{"op": "diagnostics", "path": "x.py"})
	if !strings.Contains(msg, "no server configured") || !strings.Contains(msg, ".go") {
		t.Fatalf("msg=%s", msg)
	}
	msg = lspCallErr(t, reg, map[string]any{"op": "rename", "path": "x.py"})
	if !strings.Contains(msg, "unknown op") {
		t.Fatalf("msg=%s", msg)
	}
}

func TestLSPStartAndInitFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFakeServer()
	injectFake(t, fs, fmt.Errorf("exec: not found"))

	reg, cleanup := lspSetup(t, lspOptions(root, nil))
	defer cleanup()

	msg := lspCallErr(t, reg, map[string]any{"op": "diagnostics", "path": "a.go"})
	if !strings.Contains(msg, "start") {
		t.Fatalf("start failure: %s", msg)
	}

	// initialize 失败：回 JSON-RPC error；不缓存 server，下次重试。
	fs2 := newFakeServer()
	fs2.handle = func(f rpcFrame) {
		if f.Method == "initialize" {
			fs2.replyErr(f, "boom")
		}
	}
	injectFake(t, fs2, nil)
	msg = lspCallErr(t, reg, map[string]any{"op": "diagnostics", "path": "a.go"})
	if !strings.Contains(msg, "initialize") {
		t.Fatalf("init failure: %s", msg)
	}
}

func TestLSPDiagWindowTimeout(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		if f.Method == "initialize" {
			fs.reply(f, map[string]any{})
		}
		// didOpen 不回 publish → 超时路径。
	}
	injectFake(t, fs, nil)

	reg, cleanup := lspSetup(t, lspOptions(root, func(o *Options) { o.DiagWindow = 60 * time.Millisecond }))
	defer cleanup()

	out := lspCall(t, reg, map[string]any{"op": "diagnostics", "path": "a.go"})
	if !strings.Contains(out, "no diagnostics reported within") {
		t.Fatalf("out=%q", out)
	}
}

func TestLSPDidChangeAfterEdit(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "hello.go")
	first := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(file, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var changeTexts []string

	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		switch f.Method {
		case "initialize":
			fs.reply(f, map[string]any{})
		case "textDocument/didOpen":
			var p didOpenParams
			if json.Unmarshal(f.Params, &p) != nil {
				return
			}
			mu.Lock()
			changeTexts = append(changeTexts, "open:"+p.TextDocument.Text)
			mu.Unlock()
			fs.notify("textDocument/publishDiagnostics", publishDiagParams{
				URI:         p.TextDocument.URI,
				Diagnostics: []rawDiag{{Range: lsRange{Start: lsPosition{Line: 2}}, Severity: 1, Message: "old error"}},
			})
		case "textDocument/didChange":
			var p didChangeParams
			if json.Unmarshal(f.Params, &p) != nil {
				return
			}
			if len(p.ContentChanges) == 0 {
				return
			}
			mu.Lock()
			changeTexts = append(changeTexts, fmt.Sprintf("change:v%d:%s", p.TextDocument.Version, p.ContentChanges[0].Text))
			mu.Unlock()
			fs.notify("textDocument/publishDiagnostics", publishDiagParams{
				URI:         p.TextDocument.URI,
				Diagnostics: []rawDiag{{Range: lsRange{Start: lsPosition{Line: 2}}, Severity: 1, Message: "new error"}},
			})
		}
	}
	injectFake(t, fs, nil)

	reg, cleanup := lspSetup(t, lspOptions(root, nil))
	defer cleanup()

	out := lspCall(t, reg, map[string]any{"op": "diagnostics", "path": "hello.go"})
	if !strings.Contains(out, "old error") {
		t.Fatalf("first=%q", out)
	}

	// 模拟 edit/apply_patch 改盘后再调 diagnostics：必须看到 didChange 全量同步 + 新诊断。
	second := "package main\n\nfunc main() { undefined() }\n"
	if err := os.WriteFile(file, []byte(second), 0o644); err != nil {
		t.Fatal(err)
	}
	out = lspCall(t, reg, map[string]any{"op": "diagnostics", "path": "hello.go"})
	if !strings.Contains(out, "new error") || strings.Contains(out, "old error") {
		t.Fatalf("after edit=%q", out)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(changeTexts, "|")
	want := "open:" + first + "|change:v2:" + second
	if joined != want {
		t.Fatalf("sync sequence = %q, want %q", joined, want)
	}
}

func TestLSPScopeDisposeKills(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		if f.Method == "initialize" {
			fs.reply(f, map[string]any{})
		}
	}
	kills := injectFake(t, fs, nil)

	// 手工装配：只走 host.Dispose()，不显式 dispose。
	host := kernel.New()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok {
		t.Fatal("no registry")
	}
	dispose, err := Register(host, reg, lspOptions(root, nil))
	if err != nil {
		host.Dispose()
		t.Fatal(err)
	}
	_ = dispose

	lspCall(t, reg, map[string]any{"op": "diagnostics", "path": "a.go"})
	host.Dispose()
	if n := kills.Load(); n != 1 {
		t.Fatalf("scope dispose must kill server, kills=%d", n)
	}
}

func TestLSPConcurrentEnsureOpenSendsOnce(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "hello.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		if f.Method == "initialize" {
			fs.reply(f, map[string]any{})
		}
	}
	injectFake(t, fs, nil)

	// 直接打 server 层：8 个 goroutine 并发同步同一文件。
	// uriLock 串行化 + hash 意图提交 ⇒ 恰好一次 didOpen、零 didChange。
	abs, err := resolveUnderRoot(root, "hello.go")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spawnServer(context.Background(), "fake", root)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(".go", "fake", sp)
	if err := srv.initialize(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ensureOpen(context.Background(), abs, ".go"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("ensureOpen: %v", err)
	}

	waitForFrames(fs, 3) // initialize / initialized / didOpen
	counts := map[string]int{}
	for _, f := range fs.methodSequence() {
		counts[f]++
	}
	if counts["textDocument/didOpen"] != 1 {
		t.Fatalf("didOpen=%d, want 1", counts["textDocument/didOpen"])
	}
	if counts["textDocument/didChange"] != 0 {
		t.Fatalf("didChange=%d, want 0 (content unchanged)", counts["textDocument/didChange"])
	}
}

func TestLSPInitializeFailureCarriesStderr(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		if f.Method == "initialize" {
			fs.replyErr(f, "boom")
		}
	}
	// 注入带 stderr 快照的 spawn：initialize 失败时 error 应带 stderr 尾巴。
	orig := spawnServer
	spawnServer = func(ctx context.Context, command, dir string) (*serverProcess, error) {
		return &serverProcess{
			conn:   fs.conn,
			kill:   func() { fs.killed.Add(1) },
			stderr: func() string { return "gopls: bad flag: -x\n" },
		}, nil
	}
	t.Cleanup(func() { spawnServer = orig })

	reg, cleanup := lspSetup(t, lspOptions(root, nil))
	defer cleanup()

	msg := lspCallErr(t, reg, map[string]any{"op": "diagnostics", "path": "a.go"})
	if !strings.Contains(msg, "initialize") || !strings.Contains(msg, "server stderr: gopls: bad flag: -x") {
		t.Fatalf("stderr hint missing: %s", msg)
	}
}

func TestStdioConnFrameTooLarge(t *testing.T) {
	c := newStdioConn(nil, strings.NewReader("Content-Length: 999999999\r\n\r\n"))
	_, err := c.Recv()
	if err == nil || !strings.Contains(err.Error(), "frame too large") {
		t.Fatalf("want frame limit refuse, got %v", err)
	}
}

func TestLSPRegisterValidation(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok {
		t.Fatal("no registry")
	}
	if _, err := Register(host, reg, Options{Root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "Servers is required") {
		t.Fatalf("empty servers: %v", err)
	}
	if _, err := Register(host, reg, Options{Servers: map[string]string{".go": "gopls"}}); err == nil || !strings.Contains(err.Error(), "Root is required") {
		t.Fatalf("missing root: %v", err)
	}
}
