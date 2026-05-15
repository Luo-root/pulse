package openai

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Luo-root/pulse/components/schema"
)

// streamResponse 解析 OpenAI SSE 流式响应
func streamResponse(resp *http.Response) *schema.StreamReader {
	reader := schema.NewStreamReader()

	go func() {
		defer func() {
			resp.Body.Close()
			reader.Close()
		}()

		const maxBufferSize = 1 << 20
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, maxBufferSize), maxBufferSize)

		var msg schema.Message
		var streamResp StreamResponse

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				return
			}

			streamResp = StreamResponse{}
			if err := json.Unmarshal([]byte(data), &streamResp); err != nil {
				reader.SetError(err)
				continue
			}

			if len(streamResp.Choices) == 0 {
				continue
			}
			choice := streamResp.Choices[0]

			// 设置角色
			if choice.Delta.Role != "" {
				msg.Role = schema.RoleType(choice.Delta.Role)
			}

			if choice.Delta.Content != "" {
				msg.Content = choice.Delta.Content
			} else {
				msg.Content = ""
			}

			if choice.Delta.ReasoningContent != "" {
				msg.ReasoningContent = choice.Delta.ReasoningContent
			} else {
				msg.ReasoningContent = ""
			}

			// 工具调用累加
			if len(choice.Delta.ToolCalls) > 0 {
				for _, tc := range choice.Delta.ToolCalls {
					idx := tc.Index
					for len(msg.ToolCalls) <= idx {
						msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{})
					}
					if tc.Function.Arguments != "" {
						msg.ToolCalls[idx].Function.Arguments += tc.Function.Arguments
					}
					if tc.ID != "" {
						msg.ToolCalls[idx].ID = tc.ID
					}
					if tc.Type != "" {
						msg.ToolCalls[idx].Type = tc.Type
					}
					if tc.Function.Name != "" {
						msg.ToolCalls[idx].Function.Name = tc.Function.Name
					}
				}
			}

			// Usage
			if streamResp.Choices[0].Usage != nil {
				reader.Usage = *streamResp.Choices[0].Usage
			}

			reader.Send(msg.Clone())
		}
	}()

	return reader
}
