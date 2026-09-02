package csdn

import "testing"

func TestRenderArticleContentMarkdown(t *testing.T) {
	md := "# 标题\n\n## 小节\n\n这是 **加粗** 与 `代码`。\n\n| 工具 | 说明 |\n|------|------|\n| get_article | 读 |\n"
	html, src := renderArticleContent(md)
	if src != md {
		t.Fatalf("markdowncontent 应保留原始 markdown，得到: %q", src)
	}
	for _, want := range []string{"<h1", "<h2", "<strong", "<code", "<table"} {
		if !contains(html, want) {
			t.Errorf("渲染后的 HTML 缺少 %q；实际: %s", want, html)
		}
	}
}

func TestRenderArticleContentHTMLPassthrough(t *testing.T) {
	raw := "<div><h2>已有 HTML</h2><p>原样保留</p></div>"
	html, src := renderArticleContent(raw)
	if html != raw || src != raw {
		t.Fatalf("以 < 开头的 HTML 应原样透传；得到 html=%q src=%q", html, src)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
