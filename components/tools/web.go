package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// SearchProvider 搜索提供商接口
type SearchProvider interface {
	Search(ctx context.Context, query string, opts SearchOptions) (*SearchResult, error)
}

// SearchOptions 搜索选项
type SearchOptions struct {
	Count int
	GL    string
	HL    string
}

// SearchResult 搜索结果
type SearchResult struct {
	Provider string             `json:"provider"`
	Query    string             `json:"query"`
	Count    int                `json:"count"`
	Results  []SearchResultItem `json:"results"`
}

// SearchResultItem 单条搜索结果
type SearchResultItem struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet"`
	Position int    `json:"position"`
}

// ============================================================================
// Serper.dev 实现
// ============================================================================

// SerperConfig Serper.dev 配置
type SerperConfig struct {
	APIKey  string
	BaseURL string // 默认 https://google.serper.dev/search
	GL      string // 默认搜索地区
	HL      string // 默认搜索语言
}

// SerperProvider 基于 Serper.dev 的搜索实现
type SerperProvider struct {
	config SerperConfig
	client *http.Client
}

func NewSerperProvider(config SerperConfig) *SerperProvider {
	if config.BaseURL == "" {
		config.BaseURL = "https://google.serper.dev/search"
	}
	if config.GL == "" {
		config.GL = "cn"
	}
	if config.HL == "" {
		config.HL = "zh-CN"
	}
	return &SerperProvider{
		config: config,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *SerperProvider) Search(ctx context.Context, query string, opts SearchOptions) (*SearchResult, error) {
	gl := s.config.GL
	if opts.GL != "" {
		gl = opts.GL
	}
	hl := s.config.HL
	if opts.HL != "" {
		hl = opts.HL
	}
	count := opts.Count
	if count <= 0 {
		count = 5
	}

	reqBody := map[string]any{
		"q":    query,
		"num":  count,
		"gl":   gl,
		"hl":   hl,
		"page": 1,
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.config.BaseURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-API-KEY", s.config.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("serper api error: status %d, body: %s", resp.StatusCode, string(body))
	}

	var serperResp serperSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&serperResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	result := &SearchResult{
		Provider: "serper_google",
		Query:    query,
		Count:    len(serperResp.Organic),
	}
	for _, item := range serperResp.Organic {
		result.Results = append(result.Results, SearchResultItem{
			Title:    item.Title,
			URL:      item.Link,
			Snippet:  item.Snippet,
			Position: item.Position,
		})
	}

	return result, nil
}

// Serper API 响应结构（内部使用，不导出）
type serperSearchResponse struct {
	Organic []struct {
		Title    string `json:"title"`
		Link     string `json:"link"`
		Snippet  string `json:"snippet"`
		Position int    `json:"position"`
	} `json:"organic"`
}

// ============================================================================
// WebSearch 工具
// ============================================================================

var defaultProvider SearchProvider

// SetDefaultSearchProvider 设置默认搜索 provider
func SetDefaultSearchProvider(provider SearchProvider) {
	defaultProvider = provider
}

// GetDefaultSearchProvider 获取默认搜索 provider，未设置时从环境变量自动创建 Serper
func GetDefaultSearchProvider() SearchProvider {
	if defaultProvider != nil {
		return defaultProvider
	}
	// 从环境变量自动创建
	apiKey := os.Getenv("SERPER_API_KEY")
	if apiKey == "" {
		return nil
	}
	return NewSerperProvider(SerperConfig{
		APIKey:  apiKey,
		BaseURL: os.Getenv("SERPER_API_URL"),
	})
}

// WebSearch 联网搜索工具
func WebSearch(ctx context.Context, args map[string]any) (any, error) {
	provider := GetDefaultSearchProvider()
	if provider == nil {
		return nil, fmt.Errorf("search provider not configured: set SERPER_API_KEY env or call SetDefaultSearchProvider()")
	}

	query, ok := args["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("query must be a non-empty string")
	}

	count := 5
	if c, ok := args["count"].(float64); ok && c > 0 && c <= 100 {
		count = int(c)
	}

	opts := SearchOptions{Count: count}
	if gl, ok := args["gl"].(string); ok && gl != "" {
		opts.GL = gl
	}
	if hl, ok := args["hl"].(string); ok && hl != "" {
		opts.HL = hl
	}

	result, err := provider.Search(ctx, query, opts)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"engine":  result.Provider,
		"query":   result.Query,
		"count":   count,
		"total":   len(result.Results),
		"results": result.Results,
		"status":  "success",
	}, nil
}

var webSearchParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"query": map[string]any{"type": "string", "description": "搜索关键词（必填）"},
		"count": map[string]any{"type": "number", "description": "返回结果数量（1-100，默认5）"},
		"gl":    map[string]any{"type": "string", "description": "搜索地区（可选）"},
		"hl":    map[string]any{"type": "string", "description": "搜索语言（可选）"},
	},
	"required": []string{"query"},
}

func RegisterWebTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name:        "web_search",
		Description: "联网搜索，返回最新网页搜索结果。需要配置 SERPER_API_KEY 环境变量或调用 SetDefaultSearchProvider",
		Parameters:  webSearchParams,
		Permission:  PermReadOnly,
		Category:    "network",
		Version:     "1.0.0",
		Tags:        []string{"network", "search", "web", "safe"},
		Timeout:     60 * time.Second,
	}, WebSearch)
}
