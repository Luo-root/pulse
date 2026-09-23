package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

// fakeConn 是内存 frameConn：记录 server 发出的帧。onSend 在 Send 调用者
// goroutine 内同步回调——记录序因此严格等于客户端发送序（异步回调曾导致
// Linux -race 下记录序与发送序错位）；回帧仍由 handle 自行异步投递。
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

func (c *fakeConn) Send(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sent = append(c.sent, append([]byte(nil), body...))
	c.mu.Unlock()
	if c.onSend != nil {
		c.onSend(body)
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
			// 回帧保持异步：不阻塞 Send，客户端经 Recv 等待，时序语义与旧实现一致。
			go fs.handle(f)
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

// TestLSPRestartsAfterServerDeath：#216——语言服务器自行退出（配置、OOM、
// 锁文件）是常态，缓存里不能留死连接：下一次调用必须摘掉它、兜底收尾进程树、
// 重新 spawn 并成功。既有用例只覆盖「启动/握手失败下次重试」。
//
// 测试里的「死亡」= 关掉 fake conn 的接收端（Recv 返回 io.EOF，等价于进程
// 退出）。为了不靠 sleep 猜时序，先让一个 fake 不回帧的请求挂在那里，关连接
// 后它会以「connection closed」返回——那一刻 readLoop 已经把这个 server 标
// 死，之后的断言才是确定性的。
func TestLSPRestartsAfterServerDeath(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "hello.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := fileURI(file)

	orig := spawnServer
	var mu sync.Mutex
	var fakes []*fakeServer
	var kills atomic.Int32
	spawnServer = func(ctx context.Context, command, dir string) (*serverProcess, error) {
		fs := newFakeServer()
		fs.handle = func(f rpcFrame) {
			switch f.Method {
			case "initialize":
				fs.reply(f, map[string]any{"capabilities": map[string]any{}})
			case "textDocument/didOpen":
				fs.notify("textDocument/publishDiagnostics", publishDiagParams{URI: uri})
			}
			// textDocument/references 故意不回帧：用它把调用挂住当死亡信号。
		}
		mu.Lock()
		fakes = append(fakes, fs)
		mu.Unlock()
		return &serverProcess{conn: fs.conn, kill: func() { kills.Add(1) }}, nil
	}
	t.Cleanup(func() { spawnServer = orig })

	reg, cleanup := lspSetup(t, lspOptions(root, nil))
	defer cleanup()

	if out := lspCall(t, reg, map[string]any{"op": "diagnostics", "path": "hello.go"}); !strings.Contains(out, "diagnostic(s)") {
		t.Fatalf("first call must work before the server dies: %q", out)
	}

	// 挂住一个请求：它进入 pending 后，关连接才会把等待者唤醒。
	dead := make(chan error, 1)
	go func() {
		b, err := json.Marshal(map[string]any{"op": "references", "path": "hello.go"})
		if err != nil {
			dead <- err
			return
		}
		_, err = reg.AsToolSet().Execute(context.Background(), llm.ToolCall{
			ID: "t2", Name: "lsp", Arguments: b,
		})
		dead <- err
	}()

	mu.Lock()
	first := fakes[0]
	mu.Unlock()
	if !waitForMethod(first, "textDocument/references") {
		t.Fatal("the pending request was never sent")
	}
	close(first.conn.out) // server 自行退出

	select {
	case err := <-dead:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("a pending request must fail with a connection-closed error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pending request never returned after the server died")
	}

	// 自愈：下一次调用必须重建并成功，且死 server 已被收尾（树杀兜底）。
	out := lspCall(t, reg, map[string]any{"op": "diagnostics", "path": "hello.go"})
	if !strings.Contains(out, "diagnostic(s)") {
		t.Fatalf("the tool must recover after the server died: %q", out)
	}
	mu.Lock()
	n := len(fakes)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("a dead server must be replaced by exactly one new spawn, spawns=%d", n)
	}
	if k := kills.Load(); k < 1 {
		t.Fatalf("the dead server's process tree must be reaped, kills=%d", k)
	}
}

// waitForMethod 等 fake 记录到某个方法帧（记录发生在 Send 内，即 pending 已
// 登记之后）。
func waitForMethod(fs *fakeServer, name string) bool {
	t0 := time.Now()
	for {
		for _, m := range fs.methodSequence() {
			if m == name {
				return true
			}
		}
		if time.Since(t0) > 5*time.Second {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLSPDiagnosticsReportsDeadServer：#216 review 该修——`diagnostics` 是
// **不登记 pending** 的那条出口（轮询 s.diags），failPending 唤不醒它。server
// 在等待窗口内猝死时，若不查存活就会返回「no diagnostics reported within …
// (server may still be indexing)」这个**成功的软结果**，宿主按「0 个诊断 =
// 干净」决策就误判了（fail-open）。
func TestLSPDiagnosticsReportsDeadServer(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "hello.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		switch f.Method {
		case "initialize":
			fs.reply(f, map[string]any{"capabilities": map[string]any{}})
		case "textDocument/didOpen":
			// 索引期的 server 猝死：连接断掉，且**始终**不推 diagnostics。
			close(fs.conn.out)
		}
	}
	injectFake(t, fs, nil)

	reg, cleanup := lspSetup(t, lspOptions(root, func(o *Options) { o.DiagWindow = 2 * time.Second }))
	defer cleanup()

	msg := lspCallErr(t, reg, map[string]any{"op": "diagnostics", "path": "hello.go"})
	if !strings.Contains(msg, "connection closed") {
		t.Fatalf("a server that dies during the window must be reported as an error, got %q", msg)
	}
	if strings.Contains(msg, "may still be indexing") {
		t.Fatalf("must not fall back to the soft 'still indexing' text: %q", msg)
	}
}

// TestLSPDiagnosticsKeepsResultPushedRightBeforeDeath：#230——「server 推完这一版
// 诊断、随即猝死」时不能把已经拿到的那份事实丢掉：存活检查先读后判，命中即返回。
// 与上一条配对：始终不推诊断 + 猝死仍然要报 connection closed（保守方向不变）。
func TestLSPDiagnosticsKeepsResultPushedRightBeforeDeath(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "hello.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := fileURI(file)
	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		switch f.Method {
		case "initialize":
			fs.reply(f, map[string]any{"capabilities": map[string]any{}})
		case "textDocument/didOpen":
			// 先推一版诊断，随即猝死——两帧同一方向、同一 channel，
			// 客户端按序处理，所以「已 received」必然先于「标死」。
			fs.notify("textDocument/publishDiagnostics", publishDiagParams{
				URI: uri,
				Diagnostics: []rawDiag{{
					Range:    lsRange{Start: lsPosition{Line: 0, Character: 3}},
					Severity: 1,
					Message:  "pushed then died",
					Source:   "fakegopls",
				}},
			})
			close(fs.conn.out)
		}
	}
	injectFake(t, fs, nil)

	reg, cleanup := lspSetup(t, lspOptions(root, func(o *Options) { o.DiagWindow = 2 * time.Second }))
	defer cleanup()

	out := lspCall(t, reg, map[string]any{"op": "diagnostics", "path": "hello.go"})
	if !strings.Contains(out, "1 diagnostic(s)") || !strings.Contains(out, "pushed then died") {
		t.Fatalf("must return the diagnostics received before the server died, got %q", out)
	}
	if strings.Contains(out, "connection closed") {
		t.Fatalf("must not report the server as dead while a received result is available: %q", out)
	}
}

// failSendConn 是 Send 恒失败的 frameConn：钉住「管道已破」这条标死路径。
type failSendConn struct {
	base *fakeConn
	err  error
}

func (c failSendConn) Send(context.Context, []byte) error { return c.err }
func (c failSendConn) Recv() ([]byte, error)              { return c.base.Recv() }
func (c failSendConn) Close() error                       { return c.base.Close() }

// TestServerMarksDeadWhenSendFails：#216 review 建议 1——`call` 与 `notify`
// 的 Send 失败是两条新增的标死路径（README 承诺「连接一断即标死」的 Send
// 侧），此前零覆盖。这里直接构造 server，不走 spawn 缝。
func TestServerMarksDeadWhenSendFails(t *testing.T) {
	boom := errors.New("write: broken pipe")

	// call：请求发不出去 → 报错 + 标死。
	baseCall := newFakeConn()
	defer close(baseCall.out)
	s1 := newServer(".go", "fake", &serverProcess{
		conn: failSendConn{base: baseCall, err: boom},
		kill: func() {},
	})
	if _, err := s1.call(context.Background(), "initialize", struct{}{}); err == nil {
		t.Fatal("call must fail when Send fails")
	}
	if !s1.unusable() {
		t.Fatal("a failed call Send must mark the server unusable")
	}

	// notify：通知发不出去 → 报错 + 标死。
	baseNotify := newFakeConn()
	defer close(baseNotify.out)
	s2 := newServer(".go", "fake", &serverProcess{
		conn: failSendConn{base: baseNotify, err: boom},
		kill: func() {},
	})
	if err := s2.notify(context.Background(), "initialized", struct{}{}); err == nil {
		t.Fatal("notify must fail when Send fails")
	}
	if !s2.unusable() {
		t.Fatal("a failed notify Send must mark the server unusable")
	}
}

// stallAfterConn 是「server 收下握手后就不再读 stdin」的 frameConn：含 mark 的帧
// 永不写完，只等 ctx。写侧真实现（管道写满）无法注入，用它钉调用面的上限。
type stallAfterConn struct {
	*fakeConn
	mark []byte
}

func (c stallAfterConn) Send(ctx context.Context, body []byte) error {
	if bytes.Contains(body, c.mark) {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.fakeConn.Send(ctx, body)
}

// TestStdioConnSendBoundedByContext：#250——写端是管道，server 不读 stdin 时写会
// 永远挂着。Send 必须由 ctx 兜底（notify 与收尾路径没有请求帧可等，ctx 是唯一上限）。
func TestStdioConnSendBoundedByContext(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	conn := newStdioConn(pw, pr) // 读端没人读：写满缓冲区后阻塞

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := conn.Send(ctx, make([]byte, 1<<20)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Send must return when ctx is done, took %s", d)
	}
}

// TestEnsureOpenBoundedByTimeout：#250——同步帧（didOpen）也在 Timeout 覆盖内：
// server 收下握手后不再读 stdin 时，工具按 Timeout 报错，而不是挂在那里。
func TestEnsureOpenBoundedByTimeout(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newFakeServer()
	fs.handle = func(f rpcFrame) {
		if f.Method == "initialize" {
			fs.reply(f, map[string]any{"capabilities": map[string]any{}})
		}
	}
	orig := spawnServer
	spawnServer = func(ctx context.Context, command, dir string) (*serverProcess, error) {
		return &serverProcess{
			conn: stallAfterConn{fakeConn: fs.conn, mark: []byte("textDocument/didOpen")},
			kill: func() { fs.killed.Add(1) },
		}, nil
	}
	t.Cleanup(func() { spawnServer = orig })

	reg, cleanup := lspSetup(t, lspOptions(root, func(o *Options) { o.Timeout = 300 * time.Millisecond }))
	defer cleanup()

	start := time.Now()
	msg := lspCallErr(t, reg, map[string]any{"op": "diagnostics", "path": "hello.go"})
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the didOpen write must be bounded by Timeout, took %s", d)
	}
	if !strings.Contains(msg, "didOpen") || !strings.Contains(msg, "deadline exceeded") {
		t.Fatalf("want a didOpen deadline error, got %s", msg)
	}
}

// TestSpawnReapsProcess：#250——spawn 后必须 Wait（回收 stdin 管道父端、进程句柄
// 与 os/exec 的 ctx 监视协程）。判据：反复 spawn + 收尾之后 goroutine 数回落。
func TestSpawnReapsProcess(t *testing.T) {
	command := "true"
	if runtime.GOOS == "windows" {
		command = "cmd /c exit"
	}
	base := runtime.NumGoroutine()
	for i := 0; i < 8; i++ {
		sp, err := spawnServer(context.Background(), command, t.TempDir())
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		sp.kill()
		_ = sp.conn.Close()
	}
	// 进程退出 → Wait 归还，给调度留时间；不靠固定 sleep 判死。
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		if n := runtime.NumGoroutine(); n <= base+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines grew from %d to %d across 8 spawn/kill cycles (Wait must reap)",
				base, runtime.NumGoroutine())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestServerForConcurrentRebuildYieldsToOne：#216 review 建议 1——stale 缓存
// 被摘掉后，两路并发 serverFor 都会走到 spawn；先插进缓存的那路获胜，另一路
// 必须收尾自己的进程并退让（否则会出现双缓存 / 每次调用都要重建）。
//
// 用 spawnServer 里的闸门把两路**同时**钉在 spawn 里，避免靠调度撞时序。
func TestServerForConcurrentRebuildYieldsToOne(t *testing.T) {
	root := t.TempDir()
	opt, err := Options{Root: root, Servers: map[string]string{".go": "fake-lsp"}}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}

	orig := spawnServer
	var spawns atomic.Int32
	var kills atomic.Int32
	entered := make(chan struct{}, 2)
	gate := make(chan struct{})
	spawnServer = func(ctx context.Context, command, dir string) (*serverProcess, error) {
		spawns.Add(1)
		fs := newFakeServer()
		fs.handle = func(f rpcFrame) {
			if f.Method == "initialize" {
				fs.reply(f, map[string]any{})
			}
		}
		entered <- struct{}{}
		<-gate // 两路都进到这里再放行
		return &serverProcess{conn: fs.conn, kill: func() { kills.Add(1) }}, nil
	}
	t.Cleanup(func() { spawnServer = orig })

	// 缓存里放一个**已标死**的 server（走真实的 markDead，不用伪造状态）。
	staleBase := newFakeConn()
	stale := newServer(".go", "fake-lsp", &serverProcess{
		conn: staleBase,
		kill: func() { kills.Add(1) },
	})
	stale.markDead()
	m := &manager{opt: opt, servers: map[string]*server{".go": stale}}

	type result struct {
		srv *server
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			srv, err := m.serverFor(context.Background(), ".go")
			results <- result{srv: srv, err: err}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("both callers must reach spawnServer")
		}
	}
	close(gate)

	var got []*server
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("serverFor: %v", r.err)
			}
			got = append(got, r.srv)
		case <-time.After(5 * time.Second):
			t.Fatal("serverFor did not return")
		}
	}

	if got[0] != got[1] {
		t.Fatalf("两路并发重建必须退让给同一个 server：%p vs %p", got[0], got[1])
	}
	m.mu.Lock()
	cached := m.servers[".go"]
	m.mu.Unlock()
	if cached != got[0] {
		t.Fatalf("缓存里必须是获胜的那一个：cached=%p want %p", cached, got[0])
	}
	if n := spawns.Load(); n != 2 {
		t.Fatalf("两路各 spawn 一次且只一次：spawns=%d", n)
	}
	if k := kills.Load(); k < 1 {
		t.Fatalf("落败的那路必须收尾自己的进程树：kills=%d", k)
	}
	if cached.unusable() {
		t.Fatal("获胜的 server 必须是可用的")
	}
}

// TestLSPDiagnosticsFlow：诊断流转 + 协议序（initialize → initialized → didOpen）。
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
