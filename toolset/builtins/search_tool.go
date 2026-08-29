package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func (e *env) regWebSearch() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "web_search",
			Description: "Search the web. Returns a list of titles, URLs and snippets. Default backend is DuckDuckGo Lite; hosts may inject another Searcher.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "query":{"type":"string"},
    "limit":{"type":"integer","minimum":1,"maximum":20}
  },
  "required":["query"]
}`),
		},
		Fn:        e.webSearch,
		Risk:      toolset.RiskReadonly,
		PreviewFn: e.previewWebSearch,
	}
}

func (e *env) previewWebSearch(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
	var p struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(args, &p)
	return toolset.Preview{
		Kind:    toolset.KindOpaque,
		Action:  toolset.ActionRead,
		Subject: "search:" + p.Query,
		Opaque:  &toolset.OpaqueChange{Summary: "web_search", ArgsExcerpt: p.Query},
	}, nil
}

func (e *env) webSearch(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/web_search: invalid args: %w", err)
	}
	s := e.opt.Searcher
	if s == nil {
		return "", fmt.Errorf("builtins/web_search: no Searcher")
	}
	hits, err := s.Search(ctx, p.Query, p.Limit)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "(no results)", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d results:\n\n", len(hits))
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. %s\n   URL: %s\n   %s\n\n", i+1, h.Title, h.URL, h.Snippet)
	}
	return b.String(), nil
}
