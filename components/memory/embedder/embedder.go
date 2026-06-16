// Package embedder 提供文本转向量的实现
// 支持 OpenAI 兼容 API 和 Ollama 原生 API
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Embedder 文本转向量接口
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ============================================================================
// OpenAI 兼容 Embedder
// 支持 OpenAI、vLLM、LocalAI、Ollama（/v1/embeddings）等兼容端点
// ============================================================================

type OpenAIConfig struct {
	BaseURL string // 如 "https://api.openai.com/v1" 或 "http://localhost:11434/v1"
	APIKey  string
	Model   string
	Timeout time.Duration
}

type openAIClient struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func NewOpenAI(config *OpenAIConfig) Embedder {
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &openAIClient{
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

func (e *openAIClient) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody, _ := json.Marshal(openAIEmbedRequest{Model: e.model, Input: text})
	url := e.baseURL + "/embeddings"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("embed: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]any
		json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("embed: API error %d: %v", resp.StatusCode, errBody)
	}

	var embedResp openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(embedResp.Data) == 0 {
		return nil, fmt.Errorf("embed: empty response data")
	}
	return embedResp.Data[0].Embedding, nil
}

// ============================================================================
// Ollama 原生 Embedder
// 使用 Ollama 原生 /api/embeddings 端点
// ============================================================================

type OllamaConfig struct {
	BaseURL string // 如 "http://localhost:11434"
	Model   string // 如 "nomic-embed-text"
	Timeout time.Duration
}

type ollamaClient struct {
	baseURL string
	model   string
	client  *http.Client
}

func NewOllama(config *OllamaConfig) Embedder {
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &ollamaClient{
		baseURL: config.BaseURL,
		model:   config.Model,
		client:  &http.Client{Timeout: timeout},
	}
}

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float64 `json:"embedding"`
}

func (e *ollamaClient) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody, _ := json.Marshal(ollamaEmbedRequest{Model: e.model, Prompt: text})

	req, err := http.NewRequestWithContext(ctx, "POST", e.baseURL+"/api/embeddings", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("ollama embed: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed: HTTP %d", resp.StatusCode)
	}

	var embResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embResp); err != nil {
		return nil, fmt.Errorf("ollama embed: decode response: %w", err)
	}

	vec := make([]float32, len(embResp.Embedding))
	for i, v := range embResp.Embedding {
		vec[i] = float32(v)
	}
	return vec, nil
}
