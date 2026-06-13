package memnetai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

const (
	defaultMemNetAIBaseURL = "https://api.memnetai.com"
)

// Config MemNetAI 存储配置
type Config struct {
	// APIKey MemNetAI 平台颁发的 API Key（必填）
	APIKey string

	// BaseURL 服务地址，默认 https://api.memnetai.com
	BaseURL string

	// Namespace 命名空间，用于在同一项目内对记忆体进行逻辑隔离。
	// 记忆体的唯一性由「namespace + memoryAgentName」共同决定。
	// 例如可以用用户ID或智能体ID作为命名空间。
	Namespace string

	// Language 记忆语言，如 "zh"、"en" 等，默认 "zh"
	Language string

	// IsThirdPerson 是否以第三人称视角提取记忆摘要，默认 false（第一人称效果更佳）
	IsThirdPerson bool

	// AsyncMode 记忆是否使用异步模式。同步模式立即执行但上下文容量小；异步模式排队但支持大量上下文。
	AsyncMode bool

	// HTTPClient 自定义 HTTP 客户端，不传则使用默认（超时 200s）
	HTTPClient *http.Client
}

// Store 基于 MemNetAI 长记忆服务的 Store 实现。
//
// MemNetAI 是一个 AI 智能体长记忆服务平台，通过「记忆→回忆→思考→做梦」
// 的完整认知周期，为智能体提供接近人类记忆机制的长期记忆能力。
type Store struct {
	apiKey    string
	baseURL   string
	namespace string
	language  string

	isThirdPerson bool
	asyncMode     bool
	httpClient    *http.Client
}

// NewStore 创建 MemNetAI 记忆存储实例。
//
// 示例:
//
//	store, err := memory.NewStore(&memory.Config{
//	    APIKey:    "your-memnetai-api-key",
//	    Namespace: "user_123",
//	})
func NewStore(cfg *Config) (*Store, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("memnetai: api key is required")
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultMemNetAIBaseURL
	}

	lang := cfg.Language
	if lang == "" {
		lang = "zh"
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: 200 * time.Second, // 同步模式官方限制最长 200 秒
		}
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = "default"
	}

	return &Store{
		apiKey:        cfg.APIKey,
		baseURL:       baseURL,
		namespace:     ns,
		language:      lang,
		isThirdPerson: cfg.IsThirdPerson,
		asyncMode:     cfg.AsyncMode,
		httpClient:    client,
	}, nil
}

// ============================================================================
// 内部数据结构（与 MemNetAI API 对应）
// ============================================================================

// message 是 MemNetAI 的对话消息格式，基于 OpenAI 格式增强，
// 每条 user 消息可额外携带 character 字段标识发言者。
type message struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Character string `json:"character,omitempty"`
}

// memoriesRequest POST /v1/memories 请求体
type memoriesRequest struct {
	MemoryAgentName string    `json:"memoryAgentName"`
	Messages        []message `json:"messages"`
	Namespace       string    `json:"namespace"`
	Language        string    `json:"language"`
	IsThirdPerson   int       `json:"isThirdPerson"`
	Metadata        string    `json:"metadata,omitempty"`
	AsyncMode       int       `json:"asyncMode"`
}

// memoriesResponse POST /v1/memories 响应体
type memoriesResponse struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Tip    string `json:"tip"`
		TaskID string `json:"taskId"`
		Usage  struct {
			ConsumedPoints            string `json:"consumedPoints"`
			ConsumedMemoryCount       string `json:"consumedMemoryCount"`
			ConsumedRecallCount       string `json:"consumedRecallCount"`
			ConsumedThinkingCount     string `json:"consumedThinkingCount"`
			ConsumedDreamCount        string `json:"consumedDreamCount"`
			ConsumedCommonMemoryWords string `json:"consumedCommonMemoryWords"`
			HasRemainingQuota         bool   `json:"hasRemainingQuota"`
		} `json:"usage"`
		MemoriesInfo struct {
			NewMemoryCount int      `json:"newMemoryCount"`
			Summarys       []string `json:"summarys"`
		} `json:"memoriesInfo"`
	} `json:"data"`
}

// recallRequest POST /v1/recall 请求体
type recallRequest struct {
	MemoryAgentName                       string   `json:"memoryAgentName"`
	Query                                 string   `json:"query"`
	Namespace                             string   `json:"namespace"`
	Character                             string   `json:"character,omitempty"`
	RecallDeep                            int      `json:"recallDeep"`
	IsIncludeLinkedNewMemoriesFromInvalid int      `json:"isIncludeLinkedNewMemoriesFromInvalid"`
	IsUsingAssociativeThinking            int      `json:"isUsingAssociativeThinking"`
	IsUsingCommonSenseDatabase            int      `json:"isUsingCommonSenseDatabase"`
	IsUsingGlobalCommonSenseDatabase      int      `json:"isUsingGlobalCommonSenseDatabase"`
	IsUsingMemoryAgentCommonSenseDatabase int      `json:"isUsingMemoryAgentCommonSenseDatabase"`
	CommonSenseDatabaseIdList             []string `json:"commonSenseDatabaseIdList,omitempty"`
	IsReturningDetailedMemoryInfo         int      `json:"isReturningDetailedMemoryInfo"`
}

// memorySummary 记忆摘要条目
type memorySummary struct {
	MemorySummaryID          string `json:"memorySummaryId"`
	MemorySummaryText        string `json:"memorySummaryText"`
	MaxImpression            int    `json:"maxImpression"`
	CharactersInMemory       string `json:"charactersInMemory"`
	MemnetaiContextID        string `json:"memnetaiContextId"`
	ContextType              int    `json:"contextType"`
	MemoryStatus             int    `json:"memoryStatus"`
	LinkedNewMemorySummaryID string `json:"linkedNewMemorySummaryId"`
	MemoryChangeLog          string `json:"memoryChangeLog"`
	MetaData                 string `json:"metaData"`
	Score                    int    `json:"score"`
	CreateTime               string `json:"createTime"`
	UpdateTime               string `json:"updateTime"`
}

// recallResponse POST /v1/recall 响应体
type recallResponse struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		MemoryPrompt            string          `json:"memoryPrompt"`
		MemorySummaryList       []memorySummary `json:"memorySummaryList"`
		AssociativeThinkingList []any           `json:"associativeThinkingList"`
		CommonSenseList         []any           `json:"commonSenseList"`
		Usage                   struct {
			ConsumedPoints            string `json:"consumedPoints"`
			ConsumedMemoryCount       string `json:"consumedMemoryCount"`
			ConsumedRecallCount       string `json:"consumedRecallCount"`
			ConsumedThinkingCount     string `json:"consumedThinkingCount"`
			ConsumedDreamCount        string `json:"consumedDreamCount"`
			ConsumedCommonMemoryWords string `json:"consumedCommonMemoryWords"`
			HasRemainingQuota         bool   `json:"hasRemainingQuota"`
		} `json:"usage"`
	} `json:"data"`
}

// ============================================================================
// Store 接口实现
// ============================================================================

// Save 将对话上下文提交到 MemNetAI 进行长期记忆。
//
// MemNetAI 会对对话进行多维度深度分析：语义提取、实体关系构建、
// 历史记忆冲突检测与修正、关系图重组等。该过程较为耗时（同步模式最长 200s）。
//
// 最佳实践：推荐以 16 条对话为单位触发记忆，且记忆完成后不要立即从会话中移除上下文，
// 待若干轮对话后再移除，可获得更自然稳定的交互效果。
func (s *Store) Save(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	// 将 pulse Message 转换为 MemNetAI 消息格式
	memMsgs := make([]message, 0, len(msgs))
	for _, msg := range msgs {
		mm := message{
			Role:    string(msg.Role),
			Content: msg.TextContent(),
		}
		// role=user 时，若 Message.Name 有值，则作为 character（发言者）
		if msg.Role == schema.UserRole && msg.Name != "" {
			mm.Character = msg.Name
		}
		memMsgs = append(memMsgs, mm)
	}

	reqBody := memoriesRequest{
		MemoryAgentName: sessionID,
		Messages:        memMsgs,
		Namespace:       s.namespace,
		Language:        s.language,
		IsThirdPerson:   boolToInt(s.isThirdPerson),
		AsyncMode:       boolToInt(s.asyncMode),
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("memnetai: marshal save request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/v1/memories", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+s.apiKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("memnetai: save request failed: %w", err)
	}
	defer resp.Body.Close()

	var result memoriesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("memnetai: decode save response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("memnetai: save failed, status=%d, code=%s, msg=%s",
			resp.StatusCode, result.Code, result.Msg)
	}

	// 业务层错误码判断（兼容多种成功标识）
	if result.Code != "" && result.Code != "200" && result.Code != "0" {
		return fmt.Errorf("memnetai: save failed, code=%s, msg=%s", result.Code, result.Msg)
	}

	return nil
}

// Recall 根据查询文本从 MemNetAI 召回相关记忆。
//
// MemNetAI 的回忆是一个立体认知重构过程：知识调用、历史回溯、实体关系联想、
// 时间线索修正、记忆强度衰减（拟合艾宾浩斯曲线）。
//
// 返回的 memoryPrompt 已封装为可直接注入 System 提示词的记忆内容。
// 如果 topK > 0，会对 memorySummaryList 做切片限制。
func (s *Store) Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	reqBody := recallRequest{
		MemoryAgentName:                       sessionID,
		Query:                                 query,
		Namespace:                             s.namespace,
		RecallDeep:                            1, // 默认深入回忆，结果更全面
		IsIncludeLinkedNewMemoriesFromInvalid: 0,
		IsUsingAssociativeThinking:            1, // 默认开启联想思考
		IsUsingCommonSenseDatabase:            1, // 默认开启常识库
		IsUsingGlobalCommonSenseDatabase:      1,
		IsUsingMemoryAgentCommonSenseDatabase: 1,
		IsReturningDetailedMemoryInfo:         0, // 默认返回简化信息，只取 memoryPrompt
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("memnetai: marshal recall request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/v1/recall", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+s.apiKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memnetai: recall request failed: %w", err)
	}
	defer resp.Body.Close()

	var result recallResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("memnetai: decode recall response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memnetai: recall failed, status=%d, code=%s, msg=%s",
			resp.StatusCode, result.Code, result.Msg)
	}

	if result.Code != "" && result.Code != "200" && result.Code != "0" {
		return nil, fmt.Errorf("memnetai: recall failed, code=%s, msg=%s", result.Code, result.Msg)
	}

	// 构造返回的 Message 列表
	var messages []*schema.Message

	// 1. memoryPrompt 是最核心的回忆结果，直接作为 System 消息注入
	if result.Data.MemoryPrompt != "" {
		messages = append(messages, schema.SystemMessage(result.Data.MemoryPrompt))
	}

	// 2. 将 memorySummaryList 也加入上下文（可选增强）
	list := result.Data.MemorySummaryList
	if topK > 0 && len(list) > topK {
		list = list[:topK]
	}

	for _, summary := range list {
		if summary.MemorySummaryText == "" {
			continue
		}
		messages = append(messages, &schema.Message{
			Role:    schema.SystemRole,
			Content: summary.MemorySummaryText,
			Name:    summary.MemorySummaryID, // 用 Name 携带记忆 ID，便于追溯
		})
	}

	return messages, nil
}

// GetSession MemNetAI 目前未提供获取完整会话历史的独立接口。
// 该方法返回空列表，如需历史回溯请通过 Recall 实现。
func (s *Store) GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	// MemNetAI 的设计理念是「记忆→回忆」，不暴露原始对话历史存储。
	// 若需要获取某 session 的相关记忆，可调用 Recall(ctx, sessionID, "", 0)。
	return []*schema.Message{}, nil
}

// ClearSession MemNetAI 目前未提供公开的清空记忆接口。
// 该方法返回 nil，如需清理请在 MemNetAI 后台操作。
func (s *Store) ClearSession(ctx context.Context, sessionID string) error {
	// TODO: 若 MemNetAI 后续开放记忆清除或任务管理相关接口，可在此实现。
	return nil
}

// Close MemNetAI 基于 HTTP 服务，无需额外关闭资源。
func (s *Store) Close() error {
	return nil
}

// ============================================================================
// 辅助函数
// ============================================================================

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
