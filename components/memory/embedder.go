package memory

import "context"

// Embedder 文本转向量接口
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

func EmbedderFunc(e Embedder) EmbeddingFunc {
	return func(ctx context.Context, text string) ([]float32, error) {
		return e.Embed(ctx, text)
	}
}
