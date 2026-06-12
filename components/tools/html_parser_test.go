package tools

import (
	"strings"
	"testing"
)

// ============================================================================
// htmlToTextV2 测试
// ============================================================================

func TestHTMLToTextV2(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains []string
		excludes []string
	}{
		{
			name:     "基本标签",
			input:    "<h1>标题</h1><p>段落</p>",
			contains: []string{"标题", "段落"},
			excludes: []string{"<h1>", "<p>"},
		},
		{
			name:     "移除 script",
			input:    `<p>安全</p><script>alert('xss')</script><p>也安全</p>`,
			contains: []string{"安全", "也安全"},
			excludes: []string{"alert", "<script>"},
		},
		{
			name:     "移除 style",
			input:    `<p>文本</p><style>body{color:red}</style>`,
			contains: []string{"文本"},
			excludes: []string{"color:red"},
		},
		{
			name:     "移除 noscript",
			input:    `<p>主体</p><noscript>请启用JS</noscript>`,
			contains: []string{"主体"},
			excludes: []string{"请启用JS"},
		},
		{
			name:     "嵌套标签",
			input:    `<div><span><b>粗体</b> 和 <i>斜体</i></span></div>`,
			contains: []string{"粗体", "斜体"},
			excludes: []string{"<div>", "<span>", "<b>"},
		},
		{
			name:     "空白规范化",
			input:    "<p>  有空格  </p>\n\n\n<p>  换行  </p>",
			contains: []string{"有空格", "换行"},
		},
		{
			name:     "空输入",
			input:    "",
			contains: nil,
		},
		{
			name:     "纯文本直通",
			input:    "没有标签",
			contains: []string{"没有标签"},
		},
		{
			name:     "自闭合标签",
			input:    "第一行<br/>第二行<hr/>第三行",
			contains: []string{"第一行", "第二行", "第三行"},
		},
		{
			name:     "多层 script/style 交替",
			input:    `<p>A</p><script>var x=1;</script><p>B</p><style>.c{}</style><p>C</p>`,
			contains: []string{"A", "B", "C"},
			excludes: []string{"var x", ".c{"},
		},
		{
			name:     "HTML 实体",
			input:    "<p>&lt;div&gt; &amp; &quot;test&quot;</p>",
			contains: []string{"<div>", "&", "\"test\""},
			excludes: []string{"&lt;", "&gt;", "&amp;", "&quot;"},
		},
		{
			name:     "嵌套 script 标签",
			input:    `<script>var x = "</script><p>这段不会被移除</p><script>";</script>`,
			contains: []string{"这段不会被移除"},
			excludes: []string{"var x"},
		},
		{
			name:     "图片 alt 文本",
			input:    `<p>文本</p><img src="test.png" alt="测试图片"><p>结尾</p>`,
			contains: []string{"文本", "[图片: 测试图片]", "结尾"},
		},
		{
			name:     "表格",
			input:    `<table><tr><td>A</td><td>B</td></tr><tr><td>C</td><td>D</td></tr></table>`,
			contains: []string{"A", "B", "C", "D"},
		},
		{
			name:     "链接",
			input:    `<p>点击 <a href="http://example.com">这里</a> 访问</p>`,
			contains: []string{"点击", "这里", "访问"},
		},
		{
			name:     "注释",
			input:    `<p>可见</p><!-- 这是注释 --><p>也可见</p>`,
			contains: []string{"可见", "也可见"},
			excludes: []string{"这是注释"},
		},
		{
			name:     "复杂嵌套",
			input:    `<article><header><h1>标题</h1></header><section><p>段落1</p><p>段落2</p></section></article>`,
			contains: []string{"标题", "段落1", "段落2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := htmlToTextV2(tt.input)

			if len(tt.contains) == 0 && len(tt.excludes) == 0 {
				return
			}

			for _, want := range tt.contains {
				if !strings.Contains(result, want) {
					t.Errorf("结果应包含 %q\n实际: %q", want, result)
				}
			}
			for _, excl := range tt.excludes {
				if strings.Contains(result, excl) {
					t.Errorf("结果不应包含 %q\n实际: %q", excl, result)
				}
			}
		})
	}
}

// TestHTMLToTextV2vsV1 对比新旧版本的输出
func TestHTMLToTextV2vsV1(t *testing.T) {
	input := `<!DOCTYPE html>
<html>
<head><title>测试</title></head>
<body>
<h1>主标题</h1>
<p>这是一段<b>加粗</b>和<i>斜体</i>的文本。</p>
<script>console.log("test");</script>
<p>另一段文本，包含 <a href="http://example.com">链接</a>。</p>
<img src="test.png" alt="测试图片">
</body>
</html>`

	v1Result := htmlToText(input)
	v2Result := htmlToTextV2(input)

	t.Logf("V1 结果:\n%s", v1Result)
	t.Logf("V2 结果:\n%s", v2Result)

	// V2 应该能正确处理 HTML 实体
	if strings.Contains(v2Result, "&lt;") || strings.Contains(v2Result, "&gt;") {
		t.Error("V2 应该解码 HTML 实体")
	}

	// V2 应该提取图片 alt 文本
	if !strings.Contains(v2Result, "[图片: 测试图片]") {
		t.Error("V2 应该提取图片 alt 文本")
	}
}
