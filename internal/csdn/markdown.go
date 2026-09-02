package csdn

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

// mdRenderer 把模型给的 Markdown 渲染为 CSDN content 字段所需的 HTML。
// 启用 GFM 扩展（表格 / 删除线 / 任务列表 / 链接自动识别 / 脚注），贴近技术博客常见写法。
var mdRenderer = goldmark.New(
	goldmark.WithExtensions(
		extension.Table,
		extension.Strikethrough,
		extension.TaskList,
		extension.Linkify,
		extension.Footnote,
	),
	goldmark.WithRendererOptions(
		html.WithHardWraps(),
	),
)

// renderArticleContent 把工具收到的正文转成 CSDN 需要的两个字段：
//   - html：展示用 HTML，填入 saveArticle 的 content；
//   - markdown：原始 Markdown 源码，填入 markdowncontent（保留编辑器里的可编辑能力）。
//
// CSDN 的 content 是“展示用 HTML”，markdowncontent 才是“原始 Markdown 源码”。
// 之前两个字段都填原始 Markdown，导致 # 标题 / **加粗** 等语法被当成纯文本原样显示，
// 表现出来就是“博客格式不对”。若正文本身已是 HTML（以 < 开头），则原样作为展示内容，
// 源码也存同一份，避免把现成 HTML 再被 markdown 解析器转义。
func renderArticleContent(content string) (htmlContent, markdownContent string) {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "<") {
		return content, content
	}
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(content), &buf); err != nil {
		// Markdown 解析理论上不会失败，兜底原样返回，避免整篇发布中断。
		return content, content
	}
	return buf.String(), content
}
