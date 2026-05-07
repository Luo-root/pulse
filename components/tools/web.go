package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

const SERPER_API_KEY = "b768255f92cf64a884c6f7215fe232cb3a9636e7"

// =================================================================

// SERPER_API_URL Serper API 端点（固定，无需修改）
const SERPER_API_URL = "https://google.serper.dev/search"

// WebSearch 真实联网搜索（基于 Serper.dev Google 搜索）
func WebSearch(ctx context.Context, args map[string]any) (any, error) {
	// 1. 参数校验
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("query must be a non-empty string")
	}

	// 可选参数处理
	count := 5 // 默认返回5条结果
	if c, ok := args["count"].(float64); ok && c > 0 && c <= 100 {
		count = int(c)
	}

	gl := "cn" // 搜索地区：中国
	if g, ok := args["gl"].(string); ok && g != "" {
		gl = g
	}

	hl := "zh-CN" // 搜索语言：中文
	if h, ok := args["hl"].(string); ok && h != "" {
		hl = h
	}

	// 2. 构造 Serper API 请求体
	reqBody := map[string]any{
		"q":    query,
		"num":  count,
		"gl":   gl,
		"hl":   hl,
		"page": 1,
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request failed: %w", err)
	}

	// 3. 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, "POST", SERPER_API_URL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}

	// 设置 Serper API 认证头（关键！）
	req.Header.Set("X-API-KEY", SERPER_API_KEY)
	req.Header.Set("Content-Type", "application/json")

	// 4. 发送请求（带超时）
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request failed: %w", err)
	}
	defer resp.Body.Close()

	// 5. 检查 API 响应状态
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("serper api error: status %d, body: %s", resp.StatusCode, string(body))
	}

	// 6. 解析 Serper API 响应
	var serperResp SerperSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&serperResp); err != nil {
		return nil, fmt.Errorf("parse serper response failed: %w", err)
	}

	// 7. 提取并格式化搜索结果（统一格式，Agent 无需修改）
	var results []map[string]any
	for _, item := range serperResp.Organic {
		results = append(results, map[string]any{
			"title":    item.Title,
			"url":      item.Link,
			"snippet":  item.Snippet,
			"position": item.Position,
		})
	}

	// 8. 封装返回结果
	return map[string]any{
		"engine":  "serper_google",
		"query":   query,
		"count":   count,
		"gl":      gl,
		"hl":      hl,
		"total":   len(results),
		"results": results,
		"status":  "success",
	}, nil
}

// webSearchParams 联网搜索参数定义（JSON Schema）
var webSearchParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"query": map[string]any{"type": "string", "description": "搜索关键词（必填）"},
		"count": map[string]any{"type": "number", "description": "返回结果数量（1-100，默认5）"},
		"gl":    map[string]any{"type": "string", "description": "搜索地区（可选，默认cn）"},
		"hl":    map[string]any{"type": "string", "description": "搜索语言（可选，默认zh-CN）"},
	},
	"required": []string{"query"},
}

// RegisterWebTools 注册联网搜索工具
func RegisterWebTools(registry *schema.ToolRegistry) {
	registry.MustRegister(schema.ToolMetadata{
		Name:        "web_search",
		Description: "真实联网搜索（基于 Serper.dev Google 搜索），返回最新的网页搜索结果",
		Parameters:  webSearchParams,
		Permission:  schema.PermReadOnly,
		Category:    "network",
		Version:     "1.0.0",
		Tags:        []string{"network", "search", "web", "safe", "google"},
		Timeout:     60 * time.Second,
	}, WebSearch)
}

// ====================== Serper API 响应结构体（无需修改） ======================

type SerperSearchResponse struct {
	SearchParameters struct {
		Q      string `json:"q"`
		Gl     string `json:"gl"`
		Hl     string `json:"hl"`
		Num    int    `json:"num"`
		Type   string `json:"type"`
		Engine string `json:"engine"`
	} `json:"searchParameters"`
	Organic []struct {
		Title    string `json:"title"`
		Link     string `json:"link"`
		Snippet  string `json:"snippet"`
		Position int    `json:"position"`
	} `json:"organic"`
}
