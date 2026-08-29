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
