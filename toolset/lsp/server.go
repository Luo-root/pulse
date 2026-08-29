package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// serverProcess 是一次 spawn 的产物：协议连接 + 树杀闭包。
// spawnServer 是缝：真实现拉子进程 stdio；测试注入内存 frameConn。
type serverProcess struct {
	conn frameConn
	kill func()
}

var spawnServer = func(ctx context.Context, command, dir string) (*serverProcess, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := buildServerCommand(runCtx, command)
	cmd.Dir = dir
	setupProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	errBuf := &cappedBuffer{max: 8 * 1024}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	pid := cmd.Process.Pid
	return &serverProcess{
		conn: newStdioConn(stdin, stdout),
		kill: func() {
			if err := killTree(pid); err != nil {
				cancel()
			}
		},
	}, nil
}

// cappedBuffer 是保尾的环形字节缓冲（语言服务器 stderr，只用于排障）。
type cappedBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if b.max > 0 && len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// ---- JSON-RPC 2.0 线格式 ----

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int64      `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---- LSP 结构（本票消费的最小子集）----

type lsPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lsRange struct {
	Start lsPosition `json:"start"`
	End   lsPosition `json:"end"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type initializeParams struct {
	ProcessID    *int      `json:"processId"`
	RootURI      string    `json:"rootUri"`
	Capabilities struct{}  `json:"capabilities"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type definitionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     lsPosition             `json:"position"`
}

type referenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

type referenceParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     lsPosition             `json:"position"`
	Context      referenceContext       `json:"context"`
}

type hoverParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     lsPosition             `json:"position"`
}

type location struct {
	URI   string  `json:"uri"`
	Range lsRange `json:"range"`
}

type publishDiagParams struct {
	URI         string    `json:"uri"`
	Diagnostics []rawDiag `json:"diagnostics"`
}

type rawDiag struct {
	Range    lsRange `json:"range"`
	Severity int     `json:"severity,omitempty"`
	Message  string  `json:"message"`
	Source   string  `json:"source,omitempty"`
}

// diagState 记录某文件是否收到过 publish（空数组也是有效结果）与最新一版。
type diagState struct {
	received bool
	items    []rawDiag
}

// server 是一个语言服务器连接：请求/通知、诊断缓存、生命周期。
type server struct {
	lang    string // 扩展名（如 ".go"）
	command string
	sp      *serverProcess

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResponse
	diags   map[string]*diagState
	opened  map[string]bool
	closed  bool
}

func newServer(lang, command string, sp *serverProcess) *server {
	s := &server{
		lang:    lang,
		command: command,
		sp:      sp,
		pending: make(map[int64]chan rpcResponse),
		diags:   make(map[string]*diagState),
		opened:  make(map[string]bool),
	}
	go s.readLoop()
	return s
}

func (s *server) readLoop() {
	for {
		body, err := s.sp.conn.Recv()
		if err != nil {
			s.failPending(errors.New("lsp: server " + s.lang + " connection closed"))
			return
		}
		var env struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &env) != nil {
			continue
		}
		if env.ID != nil {
			var resp rpcResponse
			if json.Unmarshal(body, &resp) != nil {
				continue
			}
			s.mu.Lock()
			ch := s.pending[*env.ID]
			delete(s.pending, *env.ID)
			s.mu.Unlock()
			if ch != nil {
				ch <- resp // cap=1，等待者必在 select
			}
			continue
		}
		if env.Method == "textDocument/publishDiagnostics" {
			var p publishDiagParams
			if json.Unmarshal(env.Params, &p) == nil {
				s.mu.Lock()
				st := s.diags[p.URI]
				if st == nil {
					st = &diagState{}
					s.diags[p.URI] = st
				}
				st.received = true
				st.items = p.Diagnostics
				s.mu.Unlock()
			}
		}
	}
}

func (s *server) failPending(err error) {
	s.mu.Lock()
	chs := make([]chan rpcResponse, 0, len(s.pending))
	for id, ch := range s.pending {
		chs = append(chs, ch)
		delete(s.pending, id)
	}
	s.mu.Unlock()
	for _, ch := range chs {
		close(ch)
	}
}

// call 发请求并等对应 id 的响应。
func (s *server) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("lsp: server %s is closed", s.lang)
	}
	s.nextID++
	id := s.nextID
	ch := make(chan rpcResponse, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if err := s.sp.conn.Send(body); err != nil {
		return nil, fmt.Errorf("lsp: %s: send %s: %w", s.lang, method, err)
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("lsp: server " + s.lang + " closed while waiting for " + method)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("lsp: %s: %s: %s", s.lang, method, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (s *server) notify(method string, params interface{}) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return s.sp.conn.Send(body)
}

// initialize 完成握手（initialize → initialized）。
func (s *server) initialize(ctx context.Context, root string) error {
	if _, err := s.call(ctx, "initialize", initializeParams{RootURI: fileURI(root)}); err != nil {
		return err
	}
	return s.notify("initialized", struct{}{})
}

// ensureOpen 首次访问时读文件发 didOpen（不做 didChange——改文件走写工具）。
func (s *server) ensureOpen(ctx context.Context, abs, ext string) error {
	uri := fileURI(abs)
	s.mu.Lock()
	_, opened := s.opened[uri]
	s.mu.Unlock()
	if opened {
		return nil
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("lsp: %w", err)
	}
	if strings.IndexByte(string(content), 0) >= 0 {
		return fmt.Errorf("lsp: binary file (NUL detected): %s", abs)
	}
	err = s.notify("textDocument/didOpen", didOpenParams{
		TextDocument: textDocumentItem{
			URI:        uri,
			LanguageID: langID(ext),
			Version:    1,
			Text:       string(content),
		},
	})
	if err != nil {
		return fmt.Errorf("lsp: didOpen: %w", err)
	}
	s.mu.Lock()
	s.opened[uri] = true
	s.mu.Unlock()
	return nil
}

// diagnostics 在窗口内等第一次 publish（空数组也是有效结果），返回该文件诊断。
func (s *server) diagnostics(abs string, window time.Duration) (string, error) {
	uri := fileURI(abs)
	deadline := time.Now().Add(window)
	for {
		s.mu.Lock()
		st := s.diags[uri]
		if st != nil && st.received {
			items := append([]rawDiag(nil), st.items...)
			s.mu.Unlock()
			return formatDiags(abs, items), nil
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			return fmt.Sprintf("%s: no diagnostics reported within %s (server may still be indexing)", abs, window), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func formatDiags(abs string, items []rawDiag) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d diagnostic(s) for %s\n", len(items), abs)
	for _, d := range items {
		fmt.Fprintf(&b, "%s:%d:%d [%s] %s",
			abs, d.Range.Start.Line+1, d.Range.Start.Character+1, severityName(d.Severity), d.Message)
		if d.Source != "" {
			fmt.Fprintf(&b, " (%s)", d.Source)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func severityName(sev int) string {
	switch sev {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "severity" + strconv.Itoa(sev)
	}
}

// definition / references / hover 请求（同一次响应）。

func (s *server) definition(ctx context.Context, abs string, line, col int) (string, error) {
	res, err := s.call(ctx, "textDocument/definition", definitionParams{
		TextDocument: textDocumentIdentifier{URI: fileURI(abs)},
		Position:     lsPosition{Line: line, Character: col},
	})
	if err != nil {
		return "", err
	}
	locs, err := parseLocations(res)
	if err != nil {
		return "", fmt.Errorf("lsp: definition: %w", err)
	}
	if len(locs) == 0 {
		return "(no definition found)", nil
	}
	var b strings.Builder
	for _, l := range locs {
		fmt.Fprintf(&b, "%s:%d:%d\n", uriPath(l.URI), l.Range.Start.Line+1, l.Range.Start.Character+1)
	}
	return b.String(), nil
}

func (s *server) references(ctx context.Context, abs string, line, col int, includeDecl bool) (string, error) {
	res, err := s.call(ctx, "textDocument/references", referenceParams{
		TextDocument: textDocumentIdentifier{URI: fileURI(abs)},
		Position:     lsPosition{Line: line, Character: col},
		Context:      referenceContext{IncludeDeclaration: includeDecl},
	})
	if err != nil {
		return "", err
	}
	locs, err := parseLocations(res)
	if err != nil {
		return "", fmt.Errorf("lsp: references: %w", err)
	}
	if len(locs) == 0 {
		return "(no references found)", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d reference(s):\n", len(locs))
	for _, l := range locs {
		fmt.Fprintf(&b, "%s:%d:%d\n", uriPath(l.URI), l.Range.Start.Line+1, l.Range.Start.Character+1)
	}
	return b.String(), nil
}

func (s *server) hover(ctx context.Context, abs string, line, col int) (string, error) {
	res, err := s.call(ctx, "textDocument/hover", hoverParams{
		TextDocument: textDocumentIdentifier{URI: fileURI(abs)},
		Position:     lsPosition{Line: line, Character: col},
	})
	if err != nil {
		return "", err
	}
	if len(res) == 0 || string(res) == "null" {
		return "(no hover information)", nil
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(res, &h) != nil || len(h.Contents) == 0 {
		return "(no hover information)", nil
	}
	return formatHover(h.Contents), nil
}

// formatHover 宽松解析 contents：MarkupContent 对象 / 纯字符串 / 数组。
func formatHover(raw json.RawMessage) string {
	var mc struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &mc) == nil && mc.Value != "" {
		return mc.Value
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		var parts []string
		for _, item := range arr {
			parts = append(parts, formatHover(item))
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// parseLocations 接受 null / Location / Location[]。
func parseLocations(res json.RawMessage) ([]location, error) {
	if len(res) == 0 || string(res) == "null" {
		return nil, nil
	}
	var one location
	if json.Unmarshal(res, &one) == nil && one.URI != "" {
		return []location{one}, nil
	}
	var many []location
	if err := json.Unmarshal(res, &many); err != nil {
		return nil, err
	}
	return many, nil
}

// shutdownAndKill 优雅收尾：shutdown → exit → 树杀兜底。幂等。
func (s *server) shutdownAndKill() {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _ = s.call(ctx, "shutdown", nil) // 已 closed 则忽略
	_ = s.notify("exit", nil)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.failPending(errors.New("lsp: server closed"))
	s.sp.kill()
}

// fileURI 是本地路径 → file:// URI（Windows 盘符、空格转义都交给 url.URL）。
func fileURI(abs string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// uriPath 反解 file URI 为本地路径（解析失败原样返回）。
func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	p, err := url.PathUnescape(u.Path)
	if err != nil {
		return uri
	}
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:] // "/D:/x" → "D:/x"
	}
	return filepath.FromSlash(p)
}

func langID(ext string) string {
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cpp", ".hpp", ".cc":
		return "cpp"
	case ".java":
		return "java"
	default:
		return "plaintext"
	}
}
