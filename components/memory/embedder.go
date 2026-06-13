package memory

import (
	"context"

	"github.com/Luo-root/pulse/components/memory/gorm"
)

// Embedder 文本转向量接口
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

func EmbedderFunc(e Embedder) gorm.EmbeddingFunc {
	return func(ctx context.Context, text string) ([]float32, error) {
		return e.Embed(ctx, text)
	}
}
