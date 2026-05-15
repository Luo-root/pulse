package anthropic

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Luo-root/pulse/components/schema"
)

// streamResponse 处理 Anthropic 流式响应
func streamResponse(resp *http.Response) *schema.StreamReader {
	reader := schema.NewStreamReader()

	go func() {
		defer func() {
			resp.Body.Close()
			reader.Close()
		}()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)

		var currentMsg schema.Message
		var currentToolCall *schema.ToolCall

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			// SSE 格式：event: xxx\ndata: xxx
			if strings.HasPrefix(line, "event: ") {
				// 事件类型，暂时不处理
				continue
			}

			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				return
			}

			// 解析事件
			var event StreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				reader.SetError(err)
				continue
			}

			// 根据事件类型处理
			switch event.Type {
			case "message_start":
				// 消息开始，初始化
				currentMsg = schema.Message{
					Role: schema.AssistantRole,
				}

			case "content_block_start":
				// 内容块开始
				if event.ContentBlock != nil {
					switch event.ContentBlock.Type {
					case "tool_use":
						currentToolCall = &schema.ToolCall{
							ID:   event.ContentBlock.ID,
							Type: "function",
							Function: schema.FunctionCall{
								Name: event.ContentBlock.Name,
							},
						}
					}
				}

			case "content_block_delta":
				// 内容块增量
				if event.Delta != nil {
					switch event.Delta.Type {
					case "text_delta":
						currentMsg.Content += event.Delta.Text
						// 发送文本 chunk
						reader.Send(schema.Message{
							Role:    schema.AssistantRole,
							Content: event.Delta.Text,
						})

					case "thinking_delta":
						currentMsg.ReasoningContent += event.Delta.Thinking
						// 发送思考 chunk
						reader.Send(schema.Message{
							Role:             schema.AssistantRole,
							ReasoningContent: event.Delta.Thinking,
						})

					case "input_json_delta":
						if currentToolCall != nil {
							currentToolCall.Function.Arguments += event.Delta.PartialJSON
						}
					}
				}

			case "content_block_stop":
				// 内容块结束
				if currentToolCall != nil {
					currentMsg.ToolCalls = append(currentMsg.ToolCalls, *currentToolCall)
					// 发送工具调用
					reader.Send(schema.Message{
						Role:      schema.AssistantRole,
						ToolCalls: []schema.ToolCall{*currentToolCall},
					})
					currentToolCall = nil
				}

			case "message_delta":
				// 消息级别更新（stop_reason, usage 等）
				if event.Usage != nil {
					reader.Usage = schema.Usage{
						PromptTokens: uint64(event.Usage.InputTokens),
						Completion:   uint64(event.Usage.OutputTokens),
						TotalTokens:  uint64(event.Usage.InputTokens + event.Usage.OutputTokens),
					}
				}

			case "message_stop":
				// 消息结束
				return
			}
		}
	}()

	return reader
}

// StreamEvent 流式事件
type StreamEvent struct {
	Type         string        `json:"type"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
	Delta        *StreamDelta  `json:"delta,omitempty"`
	Usage        *StreamUsage  `json:"usage,omitempty"`
}

// StreamDelta 流式增量
type StreamDelta struct {
	Type        string `json:"type"` // "text_delta", "thinking_delta", "input_json_delta"
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

// StreamUsage 流式 Usage
type StreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
