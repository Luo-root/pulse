package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/Luo-root/pulse/memory/index"
	"github.com/Luo-root/pulse/textsplit"
	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// 默认配置（票 #86 裁决）。
const (
	// DefaultBaseURL 是官方端点；vLLM/Ollama/网关用 WithBaseURL 覆盖。
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultBatchSize 是单请求输入条数上限（官方单请求 ≤ 2048 条，
	// 64 保守起批——批量分块/限流的最后兜底在 provider 侧）。
	DefaultBatchSize = 64
	// DefaultMaxInputChars 是单条输入字符预算（rune 数）：text-embedding-3
	// 系输入上限 8191 token；CJK ≈1-2 字符/token、英文 ≈4 字符/token——
	// 8000 字符在 CJK 场景贴近上限、英文场景远低于上限（宁短勿错）。
	// 字符 ≠ token；精确预算需要 tokenizer，本包不引（textsplit.Size
	// 的注入式设计留给宿主）。
	DefaultMaxInputChars = 8000
)

// Config 是适配器配置。
type Config struct {
	// BaseURL 是 API 端点（空 = 官方 https://api.openai.com/v1）。
	BaseURL string
	// Model 是 embedding 模型名（必填，如 text-embedding-3-small）。
	Model string
	// APIKey 是鉴权密钥（必填）。宿主从 env/配置注入——不落库、不
	// 提交、不打日志。
	APIKey string
	// HTTPClient 可选（超时/代理）；nil = SDK 默认传输。
	HTTPClient *http.Client
	// BatchSize 是单请求输入条数上限（<=0 取 DefaultBatchSize）。
	BatchSize int
	// MaxInputChars 是单条输入字符预算（rune 数；<=0 取
	// DefaultMaxInputChars）。超长在分隔符边界截断（textsplit）。
	MaxInputChars int
	// Retries 是 SDK 传输层自动重试次数；默认 0（关闭）——重试与
	// failover 属上层编排职责（llm/openai 先例），宿主显式开启才生效
	//（SDK 内置指数退避 + Retry-After）。
	Retries int
	// OnTruncate 可选：超长截断回调（original/kept 为 rune 数，与
	// MaxInputChars 同口径）。一 item 一向量模型下截断 = 尾部内容在
	// 向量召回中静默不可见（canonical 不受影响）——回调是最小可观测
	// 面；nil = 静默。指标体系归 D4。
	OnTruncate func(original, kept int)
}

// Provider 是 index.EmbeddingProvider 的 OpenAI 兼容实现（SDK 薄包装）。
type Provider struct {
	client   sdk.Client
	cfg      Config
	batch    int
	maxChars int
}

// 编译期断言：满足 index.EmbeddingProvider。
var _ index.EmbeddingProvider = (*Provider)(nil)

// New 创建 OpenAI 兼容 embeddings provider。Model/APIKey 必填。
func New(cfg Config) (*Provider, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("index/openai: model is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("index/openai: api key is required")
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = DefaultBatchSize
	}
	maxChars := cfg.MaxInputChars
	if maxChars <= 0 {
		maxChars = DefaultMaxInputChars
	}
	o := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithMaxRetries(cfg.Retries), // 默认 0：显式关闭 SDK 自动重试（llm/openai 先例）
	}
	if cfg.BaseURL != "" {
		o = append(o, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		o = append(o, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &Provider{
		client:   sdk.NewClient(o...),
		cfg:      cfg,
		batch:    batch,
		maxChars: maxChars,
	}, nil
}

// Embed 实现 index.EmbeddingProvider：批量分批（输出顺序与输入恒等，
// 按响应 Index 对齐）；超长输入在分隔符边界截断（textsplit）。
func (p *Provider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out := make([][]float32, len(texts))
	for lo := 0; lo < len(texts); lo += p.batch {
		hi := lo + p.batch
		if hi > len(texts) {
			hi = len(texts)
		}
		inputs := make([]string, hi-lo)
		for i, t := range texts[lo:hi] {
			inputs[i] = p.truncate(t)
		}
		res, err := p.client.Embeddings.New(ctx, sdk.EmbeddingNewParams{
			Model:          sdk.EmbeddingModel(p.cfg.Model),
			Input:          sdk.EmbeddingNewParamsInputUnion{OfArrayOfStrings: inputs},
			EncodingFormat: sdk.EmbeddingNewParamsEncodingFormatFloat,
		})
		if err != nil {
			return nil, p.mapErr(err)
		}
		if len(res.Data) != len(inputs) {
			return nil, fmt.Errorf("%w: embed %d texts got %d vectors", index.ErrProviderShape, len(inputs), len(res.Data))
		}
		for _, d := range res.Data {
			if d.Index < 0 || int(d.Index) >= len(inputs) {
				return nil, fmt.Errorf("%w: vector index %d out of range", index.ErrProviderShape, d.Index)
			}
			if len(d.Embedding) == 0 {
				return nil, fmt.Errorf("%w: empty vector at index %d", index.ErrProviderShape, d.Index)
			}
			vec := make([]float32, len(d.Embedding))
			for i, v := range d.Embedding {
				vec[i] = float32(v)
			}
			out[lo+int(d.Index)] = vec
		}
	}
	return out, nil
}

// truncate 把超长输入裁到 MaxInputChars 预算内（textsplit 取分隔符
// 边界切点——段落 > 句读 > 空白 > 硬切）；截断发生时触发 OnTruncate
// （rune 数口径）。
func (p *Provider) truncate(t string) string {
	if utf8.RuneCountInString(t) <= p.maxChars {
		return t
	}
	kept := t
	chunks, err := textsplit.Split(t, textsplit.Options{MaxLen: p.maxChars})
	if err == nil && len(chunks) > 0 {
		kept = chunks[0].Text
	}
	// Split 的参数恒合法（MaxLen 固定为正），err 分支纯防御——回退为
	// 不截断交由服务端报错，不静默丢内容。
	if p.cfg.OnTruncate != nil {
		p.cfg.OnTruncate(utf8.RuneCountInString(t), utf8.RuneCountInString(kept))
	}
	return kept
}

// mapErr 把 SDK 错误转成结构化错误：API 错误带 status + message；
// ctx 取消/超时保留 errors.Is 链。API key 不进任何错误信息。
func (p *Provider) mapErr(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		return fmt.Errorf("index/openai: embeddings status %d: %s", apiErr.StatusCode, apiErr.Message)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("index/openai: embeddings: %w", err)
	}
	return fmt.Errorf("index/openai: embeddings: %v", err)
}
