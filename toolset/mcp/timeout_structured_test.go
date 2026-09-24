package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/toolset"
	mcpsrc "github.com/Luo-root/pulse/toolset/mcp"
)

// hangingClient 模拟「对端不响应」：ListTools 一直等到 ctx 结束。
type hangingClient struct{}

func (hangingClient) ListTools(ctx context.Context) ([]mcpsrc.Tool, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hangingClient) CallTool(context.Context, string, json.RawMessage) (string, error) {
	return "", nil
}

func (hangingClient) Close() error { return nil }

// TestSDKClientStructuredContentFallback：#251-1——只回 structuredContent 的 server
// 不能产出空结果。走低层 handler：typed 包装会自动把结构化结果再填进 Content，
// 那样就测不到「Content 为空」这一档。
func TestSDKClientStructuredContentFallback(t *testing.T) {
	client, cleanup := startInMemoryPair(t, func(s *sdkmcp.Server) {
		s.AddTool(&sdkmcp.Tool{
			Name:        "deploy",
			InputSchema: map[string]any{"type": "object"},
		}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{
				StructuredContent: map[string]any{"deployed": true, "replicas": 3},
			}, nil
		})
	})
	defer cleanup()

	out, err := client.CallTool(context.Background(), "deploy", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("只回 structuredContent 的结果不能是空串（修复前这里恒为空）")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("结果应是 JSON 文本，得到 %q", out)
	}
	if got["deployed"] != true || got["replicas"] != float64(3) {
		t.Fatalf("结构化结果丢失: %s", out)
	}
}

// TestPluginListToolsTimeoutBounds：#251-2——装配期必须有上限：对端不响应时
// kernel.Use 在 Config.Timeout 内返回错误（并带上旋钮值），而不是永久阻塞。
func TestPluginListToolsTimeoutBounds(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
	plugin, err := mcpsrc.Plugin(reg, mcpsrc.Config{
		ID:          "hang",
		Client:      hangingClient{},
		DefaultRisk: toolset.RiskReadonly,
		Timeout:     100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, useErr := kernel.Use(host, plugin)
	if useErr == nil {
		t.Fatal("对端不响应时装配必须失败")
	}
	if !errors.Is(useErr, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", useErr)
	}
	if !strings.Contains(useErr.Error(), "Config.Timeout=100ms") {
		t.Fatalf("错误应带上旋钮值: %v", useErr)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("kernel.Use 用时 %s，应≈Config.Timeout", d)
	}
}

// TestSyncParentDeadlineWins：调用方给了更紧的 deadline 时父 ctx 优先
// （WithTimeout 取较早者），不能被 Config.Timeout 拉长。
func TestSyncParentDeadlineWins(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID:          "hang",
		Client:      hangingClient{},
		DefaultRisk: toolset.RiskReadonly,
		Timeout:     30 * time.Second, // 远大于父 ctx 的 50ms
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = src.Sync(host, ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Sync 用时 %s：父 ctx 的 deadline 必须优先", d)
	}
}

// TestSyncNonTimeoutErrorKeepsMessage：非超时错误不该被套上超时说明——别把真实的
// 列举失败（能力未协商、协议错）误报成超时。
func TestSyncNonTimeoutErrorKeepsMessage(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	reg := toolset.NewRegistry()
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID:          "boom",
		Client:      &mockClient{failList: errors.New("capability not negotiated")},
		DefaultRisk: toolset.RiskReadonly,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = src.Sync(host, context.Background())
	if err == nil || !strings.Contains(err.Error(), "capability not negotiated") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "Config.Timeout") {
		t.Fatalf("非超时错误不该带超时说明: %v", err)
	}
}
