package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

const defaultMaxFetchBytes = 256 * 1024

func (e *env) regWebFetch() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "web_fetch",
			Description: "Fetch an http(s) URL and return readable text. HTML is stripped to text. file/ftp/data schemes and cloud-metadata addresses are rejected.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "url":{"type":"string","description":"Absolute http(s) URL"}
  },
  "required":["url"]
}`),
		},
		Fn:        e.webFetch,
		Risk:      toolset.RiskReadonly,
		PreviewFn: e.previewWebFetch,
	}
}

func (e *env) previewWebFetch(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolset.Preview{}, err
	}
	u, err := parseHTTPURL(p.URL)
	if err != nil {
		return toolset.Preview{}, err
	}
	return toolset.Preview{
		Kind:    toolset.KindNetwork,
		Action:  toolset.ActionNetwork,
		Subject: u.String(),
		Network: &toolset.NetworkChange{Method: http.MethodGet, URL: u.String(), HostClass: "http"},
	}, nil
}

func (e *env) webFetch(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/web_fetch: invalid args: %w", err)
	}
	u, err := parseHTTPURL(p.URL)
	if err != nil {
		return "", err
	}
	if err := checkHost(u.Host, e.opt.BlockPrivate); err != nil {
		return "", err
	}

	client := e.opt.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "pulse-web-fetch/1.0")
	req.Header.Set("Accept", "text/html,text/plain,application/json,*/*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("builtins/web_fetch: %w", err)
	}
	defer resp.Body.Close()
	max := e.opt.MaxFetchBytes
	if max <= 0 {
		max = defaultMaxFetchBytes
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return "", fmt.Errorf("builtins/web_fetch: read: %w", err)
	}
	trunc := len(raw) > max
	if trunc {
		raw = raw[:max]
		for len(raw) > 0 && !utf8.Valid(raw) {
			raw = raw[:len(raw)-1]
		}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	body := string(raw)
	if strings.Contains(ct, "html") || looksLikeHTML(body) {
		body = htmlToText(body)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "status=%d url=%s bytes=%d\n---\n", resp.StatusCode, u.String(), len(raw))
	b.WriteString(strings.TrimSpace(body))
	b.WriteByte('\n')
	if trunc {
		fmt.Fprintf(&b, "\n[truncated: max_bytes=%d]\n", max)
	}
	return b.String(), nil
}
