package mcp

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/Luo-root/pulse/toolset"
)

const maxOpaqueArgs = 2048

// DefaultPreview 所有 MCP 工具共用的 opaque 卡片：不猜远端是改文件还是跑命令。
func DefaultPreview(source, upstream string, risk toolset.Risk) toolset.PreviewFn {
	return func(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
		excerpt := string(args)
		if excerpt == "null" {
			excerpt = ""
		}
		if utf8.RuneCountInString(excerpt) > maxOpaqueArgs {
			runes := []rune(excerpt)
			excerpt = string(runes[:maxOpaqueArgs]) + "…"
		}
		return toolset.Preview{
			Kind:    toolset.KindOpaque,
			Action:  toolset.ActionFromRisk(risk),
			Subject: source + "/" + upstream,
			Opaque: &toolset.OpaqueChange{
				Summary:     "MCP " + upstream,
				ArgsExcerpt: excerpt,
			},
		}, nil
	}
}
