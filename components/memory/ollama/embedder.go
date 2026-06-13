package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type Embedder struct {
	baseURL string
	model   string
	client  *http.Client
}

func NewEmbedder(baseURL, model string) *Embedder {
	return &Embedder{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float64 `json:"embedding"` // Ollama 返回 float64
}

func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody, _ := json.Marshal(ollamaEmbedRequest{
		Model:  e.model,
		Prompt: text,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", e.baseURL+"/api/embeddings", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var embResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embResp); err != nil {
		return nil, err
	}

	// 转换为 float32
	vec := make([]float32, len(embResp.Embedding))
	for i, v := range embResp.Embedding {
		vec[i] = float32(v)
	}
	return vec, nil
}
