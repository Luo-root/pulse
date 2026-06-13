package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OpenAIEmbedder OpenAI 兼容的 Embedding 实现
// 支持 OpenAI、Ollama（新版本 /v1/embeddings）、vLLM、LocalAI 等兼容端点
type OpenAIEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// OpenAIEmbedderConfig OpenAI Embedder 配置
type OpenAIEmbedderConfig struct {
	BaseURL string // 如 "https://api.openai.com/v1" 或 "http://localhost:11434/v1"
	APIKey  string // API Key（Ollama 等本地服务可留空）
	Model   string // 模型名，如 "text-embedding-3-small"
	Timeout time.Duration
}

// NewOpenAIEmbedder 创建 OpenAI 兼容 Embedder
func NewOpenAIEmbedder(config *OpenAIEmbedderConfig) *OpenAIEmbedder {
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &OpenAIEmbedder{
		baseURL: config.BaseURL,
		apiKey:  config.APIKey,
		model:   config.Model,
		client:  &http.Client{Timeout: timeout},
	}
}

type openAIEmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type openAIEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// Embed 实现 Embedder 接口
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody, _ := json.Marshal(openAIEmbedRequest{
		Model: e.model,
		Input: text,
	})

	url := e.baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]any
		json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("embed API error %d: %v", resp.StatusCode, errBody)
	}

	var embedResp openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(embedResp.Data) == 0 {
		return nil, fmt.Errorf("embed API returned empty data")
	}

	return embedResp.Data[0].Embedding, nil
}

// EmbedBatch 批量嵌入（一次请求多个文本）
func (e *OpenAIEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody, _ := json.Marshal(openAIEmbedBatchRequest{
		Model: e.model,
		Input: texts,
	})

	url := e.baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed batch request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]any
		json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("embed API error %d: %v", resp.StatusCode, errBody)
	}

	var embedResp openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	results := make([][]float32, len(embedResp.Data))
	for _, d := range embedResp.Data {
		if d.Index < len(results) {
			results[d.Index] = d.Embedding
		}
	}
	return results, nil
}

type openAIEmbedBatchRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}
