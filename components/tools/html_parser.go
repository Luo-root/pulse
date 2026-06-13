package tools

import (
	"strings"

	"golang.org/x/net/html"
)

// htmlToText 将 HTML 转换为纯文本
// 使用 golang.org/x/net/html 解析器，能正确处理：
// - 嵌套标签
// - HTML 实体（&lt;, &gt;, &amp; 等）
// - 注释和 CDATA
// - 属性中的特殊字符
// - 畸形 HTML（容错解析）
func htmlToText(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		// 解析失败时返回原始字符串
		return s
	}

	var buf strings.Builder
	var extractText func(*html.Node)
	var skipTag = map[string]bool{
		"script":   true,
		"style":    true,
		"noscript": true,
		"iframe":   true,
		"canvas":   true,
		"svg":      true,
		"math":     true,
	}

	// 块级元素，前后需要换行
	var blockTag = map[string]bool{
		"div":        true,
		"p":          true,
		"h1":         true,
		"h2":         true,
		"h3":         true,
		"h4":         true,
		"h5":         true,
		"h6":         true,
		"li":         true,
		"tr":         true,
		"br":         true,
		"hr":         true,
		"article":    true,
		"section":    true,
		"header":     true,
		"footer":     true,
		"aside":      true,
		"nav":        true,
		"main":       true,
		"figure":     true,
		"figcaption": true,
		"blockquote": true,
		"pre":        true,
		"address":    true,
		"details":    true,
		"summary":    true,
		"fieldset":   true,
		"legend":     true,
	}

	lastWasBlock := true // 标记上一个输出是否是块级元素

	extractText = func(n *html.Node) {
		if n.Type == html.ElementNode {
			// 跳过 script/style/noscript 等标签及其子节点
			if skipTag[n.Data] {
				return
			}

			// 块级元素前加换行（如果不是开头）
			if blockTag[n.Data] && !lastWasBlock {
				buf.WriteByte('\n')
				lastWasBlock = true
			}

			// 处理特殊元素
			switch n.Data {
			case "br":
				buf.WriteByte('\n')
				return
			case "hr":
				if !lastWasBlock {
					buf.WriteByte('\n')
				}
				buf.WriteString("---\n")
				lastWasBlock = true
				return
			case "img":
				// 提取图片 alt 文本
				for _, attr := range n.Attr {
					if attr.Key == "alt" && attr.Val != "" {
						if !lastWasBlock {
							buf.WriteByte(' ')
						}
						buf.WriteString("[图片: ")
						buf.WriteString(attr.Val)
						buf.WriteString("]")
						lastWasBlock = false
						return
					}
				}
				return
			case "a":
				// 链接会包含文本，在子节点处理
			case "td", "th":
				// 表格单元格用制表符分隔
				if !lastWasBlock {
					buf.WriteString("\t")
				}
			}
		}

		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				if !lastWasBlock {
					buf.WriteByte(' ')
				}
				buf.WriteString(text)
				lastWasBlock = false
			}
		}

		// 递归处理子节点
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			extractText(c)
		}

		// 块级元素后加换行
		if n.Type == html.ElementNode && blockTag[n.Data] && !lastWasBlock {
			buf.WriteByte('\n')
			lastWasBlock = true
		}
	}

	extractText(doc)

	// 清理多余空行
	lines := strings.Split(buf.String(), "\n")
	var result []string
	var prevEmpty bool
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if !prevEmpty {
				result = append(result, "")
				prevEmpty = true
			}
		} else {
			result = append(result, trimmed)
			prevEmpty = false
		}
	}

	return strings.Join(result, "\n")
}
