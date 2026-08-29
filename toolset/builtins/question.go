package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

// Question 是模型向人提出的一问。
type Question struct {
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

// Asker 由宿主注入：阻塞直到人回答。nil 时 question 工具 Execute 返回明确错误。
type Asker interface {
	Ask(ctx context.Context, q Question) (string, error)
}

func (e *env) regQuestion() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "question",
			Description: "Ask the human a clarifying question. Optional options are multiple-choice. This is not tool-call approval (HITL); it is a question during the turn.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "text":{"type":"string","description":"Question to show the human"},
    "options":{"type":"array","items":{"type":"string"},"description":"Optional choices"}
  },
  "required":["text"]
}`),
		},
		Fn:        e.question,
		Risk:      toolset.RiskReadonly,
		PreviewFn: e.previewQuestion,
	}
}

func (e *env) previewQuestion(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
	var p Question
	if err := json.Unmarshal(args, &p); err != nil {
		return toolset.Preview{}, err
	}
	return toolset.Preview{
		Kind:    toolset.KindOpaque,
		Action:  toolset.ActionRead,
		Subject: "question",
		Opaque:  &toolset.OpaqueChange{Summary: "ask: " + p.Text, ArgsExcerpt: string(args)},
	}, nil
}

func (e *env) question(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p Question
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/question: invalid args: %w", err)
	}
	if strings.TrimSpace(p.Text) == "" {
		return "", fmt.Errorf("builtins/question: text is required")
	}
	if e.opt.Asker == nil {
		return "", fmt.Errorf("builtins/question: no Asker configured")
	}
	ans, err := e.opt.Asker.Ask(ctx, p)
	if err != nil {
		return "", fmt.Errorf("builtins/question: %w", err)
	}
	return strings.TrimSpace(ans), nil
}
