package builtins

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// SearchHit 是一条搜索结果。
type SearchHit struct {
	Title   string
	URL     string
	Snippet string
}

// Searcher 可替换的搜索后端。nil 时 Register 使用 DuckDuckGo Lite。
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]SearchHit, error)
}

type ddgSearcher struct {
	client   *http.Client
	endpoint string
}

func newDDGSearcher(client *http.Client, endpoint string) *ddgSearcher {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if endpoint == "" {
		endpoint = "https://lite.duckduckgo.com/lite/"
	}
	return &ddgSearcher{client: client, endpoint: endpoint}
}

func (s *ddgSearcher) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("builtins/web_search: query is required")
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > DefaultSearchMax {
		limit = DefaultSearchMax
	}
	u := s.endpoint
	if !strings.Contains(u, "?") {
		u = strings.TrimRight(u, "/") + "/?q=" + url.QueryEscape(query)
	} else {
		u = u + url.QueryEscape(query)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pulse-web-search/1.0")
	req.Header.Set("Accept", "text/html")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("builtins/web_search: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	content := string(body)
	if resp.StatusCode == http.StatusAccepted ||
		strings.Contains(content, "anomaly-modal") ||
		strings.Contains(content, "Unfortunately, bots use DuckDuckGo too") {
		return nil, fmt.Errorf("builtins/web_search: DuckDuckGo rate-limited or blocked this client")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("builtins/web_search: status %s", resp.Status)
	}
	return parseDDGLite(content, limit)
}

func parseDDGLite(htmlContent string, limit int) ([]SearchHit, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return nil, fmt.Errorf("builtins/web_search: parse html: %w", err)
	}
	var hits []SearchHit
	var cur *SearchHit
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" && hasClass(n, "result-link") {
			if cur != nil && cur.URL != "" && len(hits) < limit {
				hits = append(hits, *cur)
			}
			cur = &SearchHit{Title: nodeText(n), URL: hrefOf(n)}
		}
		if n.Type == html.ElementNode && n.Data == "td" && hasClass(n, "result-snippet") && cur != nil {
			cur.Snippet = nodeText(n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if len(hits) >= limit {
				return
			}
			walk(c)
		}
	}
	walk(doc)
	if cur != nil && cur.URL != "" && len(hits) < limit {
		hits = append(hits, *cur)
	}
	return hits, nil
}

func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, f := range strings.Fields(a.Val) {
				if f == class {
					return true
				}
			}
		}
	}
	return false
}

func hrefOf(n *html.Node) string {
	for _, a := range n.Attr {
		if a.Key == "href" {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}
