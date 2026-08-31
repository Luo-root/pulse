package main

import (
	"context"
	"strings"
	"time"
)

// Document 是内存假检索命中。两套图（RAG 线性图 / DAG 分支图）共用同一
// 类型：Source 标注命中来源（local/web），空 Source 在 Search 内补。
type Document struct {
	Source  string
	Title   string
	Content string
}

// Retriever 可注入延迟/错误，便于并行与失败测试。
type Retriever interface {
	Search(ctx context.Context, query string, limit int) ([]Document, error)
}

type memoryRetriever struct {
	source string
	docs   []Document
	delay  time.Duration // 注入延迟：Timeout Aspect 教学用
	err    error         // 注入失败：错误传播教学用
}

func (r memoryRetriever) Search(ctx context.Context, query string, limit int) ([]Document, error) {
	if r.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.delay):
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	var out []Document
	for _, doc := range r.docs {
		hay := strings.ToLower(doc.Title + " " + doc.Content)
		if strings.Contains(hay, q) {
			d := doc
			if d.Source == "" {
				d.Source = r.source
			}
			out = append(out, d)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func defaultRetrievers() (local, web Retriever) {
	local = memoryRetriever{
		source: "local",
		docs: []Document{
			{Title: "kernel", Content: "卸载即还原，依赖响应式装载。"},
			{Title: "flow", Content: "一次运行一个世界，Requires 是 AND，Skip 是到达。"},
		},
	}
	web = memoryRetriever{
		source: "web",
		docs: []Document{
			{Title: "observer", Content: "E1 Waiting/Running/Finished；桥两条 Record。"},
			{Title: "yaml", Content: "E2 拓扑归属 A：YAML 拥有边，Factory 只给 Run。"},
		},
	}
	return local, web
}
