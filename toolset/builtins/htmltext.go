package builtins

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

func looksLikeHTML(s string) bool {
	head := s
	if len(head) > 512 {
		head = head[:512]
	}
	low := strings.ToLower(head)
	return strings.Contains(low, "<!doctype html") || strings.Contains(low, "<html")
}

func htmlToText(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	var b strings.Builder
	skip := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			name := strings.ToLower(n.Data)
			if name == "script" || name == "style" || name == "noscript" {
				skip++
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				skip--
				return
			}
			if skip == 0 && (name == "p" || name == "div" || name == "br" || name == "li" || name == "tr" || strings.HasPrefix(name, "h")) {
				if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
					b.WriteByte('\n')
				}
			}
		}
		if skip == 0 && n.Type == html.TextNode {
			t := strings.TrimSpace(n.Data)
			if t != "" {
				if b.Len() > 0 {
					last := rune(b.String()[b.Len()-1])
					if !unicode.IsSpace(last) {
						b.WriteByte(' ')
					}
				}
				b.WriteString(t)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.TrimSpace(b.String())
}
