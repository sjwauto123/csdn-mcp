package csdn

// 本文件负责把 CSDN 文章页的原始 HTML 转成结构化、近似 Markdown 的正文，
// 避免把几十 KB 的 HTML 整个塞给大模型（既浪费 token 又难以阅读）。

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// defaultMaxChars 是正文默认截断长度（约 8K 字符 ≈ 3~4K token）。
const defaultMaxChars = 8000

// ArticleContent 是 get_article 的结构化返回。
type ArticleContent struct {
	ArticleID   string   `json:"article_id,omitempty"`
	Title       string   `json:"title"`
	Author      string   `json:"author,omitempty"`
	PublishTime string   `json:"publish_time,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	// Content 是转成 Markdown 风格的正文（图片/链接已保留）。
	Content string `json:"content"`
	// ContentChars 与 Truncated 说明正文是否被截断。
	ContentChars int  `json:"content_chars"`
	Truncated    bool `json:"truncated"`
	URL          string `json:"url,omitempty"`
}

// skipTags 这些标签的内容直接丢弃。
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true,
	"iframe": true, "template": true, "button": true, "input": true,
	"select": true, "textarea": true, "form": true, "link": true, "meta": true,
}

// blockTags 这些标签渲染前后各补一个换行，保证段落结构。
// 注意：table / tr 已被 renderTable 单独接管，不会走到这里。
var blockTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "tr": true,
	"ul": true, "ol": true, "blockquote": true, "pre": true,
	"figure": true, "figcaption": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "header": true, "footer": true,
}

// renderState 在整棵子树遍历过程中携带的状态（目前只用于跳过目录 TOC）。
type renderState struct {
	inTOC bool
}

// ExtractArticle 从 CSDN 文章页 HTML 中提取结构化内容。
// maxChars <= 0 时使用默认值 8000。
func ExtractArticle(rawHTML string, maxChars int) *ArticleContent {
	if maxChars <= 0 {
		maxChars = defaultMaxChars
	}
	root, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return &ArticleContent{Content: "HTML 解析失败: " + err.Error()}
	}
	art := &ArticleContent{
		Title:       findTitle(root),
		Author:      findAuthor(root),
		PublishTime: findPublishTime(root, rawHTML),
		Tags:        findTags(root),
		ArticleID:   findArticleID(rawHTML),
	}
	node := findContentNode(root)
	if node == nil {
		node = root
	}
	var sb strings.Builder
	st := &renderState{}
	renderNode(node, &sb, st)
	art.Content = tidy(sb.String())
	if len([]rune(art.Content)) > maxChars {
		runes := []rune(art.Content)
		art.Content = string(runes[:maxChars]) + fmt.Sprintf("\n\n...（正文已截断，共 %d 字符；可调大 max_chars 获取完整内容）", len(runes))
		art.Truncated = true
	}
	art.ContentChars = len([]rune(art.Content))
	return art
}

// ---- 定位 ----

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, substr string) bool {
	return strings.Contains(strings.ToLower(attr(n, "class")), strings.ToLower(substr))
}

func walk(n *html.Node, fn func(*html.Node) bool) {
	if fn(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

// findContentNode 定位正文容器。CSDN 常见容器优先级：
// #content_views / .htmledit_views / #article_content / <article>。
func findContentNode(root *html.Node) *html.Node {
	var found, articleNode, bodyNode *html.Node
	walk(root, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return false
		}
		id, cls := attr(n, "id"), attr(n, "class")
		if id == "content_views" || strings.Contains(cls, "htmledit_views") ||
			id == "article_content" || strings.Contains(cls, "article_content") {
			found = n
			return true
		}
		switch n.Data {
		case "article":
			if articleNode == nil {
				articleNode = n
			}
		case "body":
			if bodyNode == nil {
				bodyNode = n
			}
		}
		return false
	})
	if found != nil {
		return found
	}
	if articleNode != nil {
		return articleNode
	}
	return bodyNode
}

func findTitle(root *html.Node) string {
	var title string
	// 1) 带 title-article 类的 h1（CSDN 文章页主标题）
	walk(root, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "h1" && strings.TrimSpace(collectText(n)) != "" {
			if hasClass(n, "title-article") {
				title = strings.TrimSpace(collectText(n))
				return true
			}
			if title == "" {
				title = strings.TrimSpace(collectText(n))
			}
		}
		return false
	})
	if title != "" {
		return title
	}
	// 2) <title> 标签（形如 "xxx-CSDN博客"）
	walk(root, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "title" {
			title = strings.TrimSpace(collectText(n))
			return true
		}
		return false
	})
	return strings.TrimSuffix(title, "-CSDN博客")
}

func findAuthor(root *html.Node) string {
	var author string
	walk(root, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return false
		}
		cls := attr(n, "class")
		if strings.Contains(cls, "follow-nickName") || strings.Contains(cls, "profile-nickName") || strings.Contains(cls, "user-name") {
			if t := strings.TrimSpace(collectText(n)); t != "" {
				author = t
				return true
			}
		}
		if n.Data == "meta" && attr(n, "name") == "author" {
			author = attr(n, "content")
			return true
		}
		return false
	})
	return author
}

var (
	rePublishTime = regexp.MustCompile(`"?publishTime"?\s*[:=]\s*"?(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)
	reMetaTime    = regexp.MustCompile(`(?i)<meta[^>]+(?:property|name)=["']article:published_time["'][^>]+content=["']([^"']+)["']`)
	reDateText    = regexp.MustCompile(`\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2}`)
)

func findPublishTime(root *html.Node, rawHTML string) string {
	// 1) 页面内嵌 JSON 中的 publishTime
	if m := rePublishTime.FindStringSubmatch(rawHTML); len(m) > 1 {
		return strings.Replace(m[1], "T", " ", 1)
	}
	// 2) meta 标签
	if m := reMetaTime.FindStringSubmatch(rawHTML); len(m) > 1 {
		return m[1]
	}
	// 3) 带 time 类名的元素文本
	var t string
	walk(root, func(n *html.Node) bool {
		if n.Type == html.ElementNode && hasClass(n, "time") {
			txt := strings.TrimSpace(collectText(n))
			if m := reDateText.FindString(txt); m != "" {
				t = m
				return true
			}
		}
		return false
	})
	if t != "" {
		return t
	}
	// 4) 兜底：正文里第一个日期串
	if m := reDateText.FindString(rawHTML); m != "" {
		return m
	}
	return ""
}

func findTags(root *html.Node) []string {
	var tags []string
	seen := map[string]bool{}
	walk(root, func(n *html.Node) bool {
		if n.Type != html.ElementNode || n.Data != "a" {
			return false
		}
		cls := attr(n, "class")
		if !strings.Contains(cls, "tag-link") && !strings.Contains(cls, "article-tag") {
			return false
		}
		t := strings.TrimSpace(collectText(n))
		if t != "" && !seen[t] {
			seen[t] = true
			tags = append(tags, t)
		}
		return false
	})
	return tags
}

var reArticleID = regexp.MustCompile(`/article/details/(\d+)`)

func findArticleID(rawHTML string) string {
	if m := reArticleID.FindStringSubmatch(rawHTML); len(m) > 1 {
		return m[1]
	}
	return ""
}

// ---- 渲染 ----

func headingLevel(tag string) (int, bool) {
	if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
		return int(tag[1] - '0'), true
	}
	return 0, false
}

func parentTag(n *html.Node) string {
	if n.Parent != nil {
		return strings.ToLower(n.Parent.Data)
	}
	return ""
}

// collectText 原样收集节点下的所有文本（用于 <pre> 代码块与标题/作者等短字段）。
func collectText(n *html.Node) string {
	var sb strings.Builder
	walk(n, func(x *html.Node) bool {
		if x.Type == html.TextNode {
			sb.WriteString(x.Data)
		}
		return false
	})
	return sb.String()
}

// isSingleImage 判定一段渲染结果是否「仅由一张图片组成」（无文字）。
// CSDN 常把封面图塞进 <h3> 里，这种空标题应降级为普通图片而非 Markdown 标题。
var reSingleImage = regexp.MustCompile(`^!\[[^\]]*\]\([^)]*\)$`)

func isSingleImage(s string) bool {
	return reSingleImage.MatchString(strings.TrimSpace(s))
}

func renderNode(n *html.Node, sb *strings.Builder, st *renderState) {
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
		return
	}
	if n.Type == html.DocumentNode {
		// html.Parse 的根节点是文档节点，需继续下钻（否则整页都渲染不出来）。
		renderChildren(n, sb, st)
		return
	}
	if n.Type != html.ElementNode {
		return
	}
	tag := strings.ToLower(n.Data)
	if skipTags[tag] {
		return
	}

	// ---- 目录（TOC）跳过状态机 ----
	// CSDN 文章页会自动注入一段「目录」：<h3>目录</h3> + 一连串 <p><a href="#...">…</a></p>，
	// 直到 <hr/> 才结束。这段不是正文，应当整段剔除，只保留真正的章节标题。
	if st.inTOC {
		if tag == "hr" {
			st.inTOC = false
			return // 消费掉 TOC 与正文之间的分隔线
		}
		if _, ok := headingLevel(tag); ok {
			// 遇到下一个真正的章节标题：TOC 结束，正常渲染该标题。
			st.inTOC = false
		} else {
			return // 其余 TOC 噪音（锚点链接等）全部跳过
		}
	}
	if level, ok := headingLevel(tag); ok {
		txt := strings.TrimSpace(collectText(n))
		if txt == "目录" {
			st.inTOC = true
			return
		}
		// 标题内若只有一张图片（封面图被包进 h3），降级为普通图片。
		var inner strings.Builder
		renderChildren(n, &inner, st)
		t := strings.TrimSpace(inner.String())
		if t == "" || isSingleImage(t) {
			if t != "" {
				sb.WriteString(t)
			}
			sb.WriteString("\n\n")
			return
		}
		sb.WriteString("\n\n" + strings.Repeat("#", level) + " " + t + "\n\n")
		return
	}

	switch tag {
	case "br":
		sb.WriteString("\n")
		return
	case "hr":
		sb.WriteString("\n\n---\n\n")
		return
	case "img":
		src := attr(n, "src")
		if src == "" {
			return
		}
		sb.WriteString(fmt.Sprintf("![%s](%s)", attr(n, "alt"), src))
		return
	case "pre":
		code := collectText(n)
		lang := detectLang(n)
		sb.WriteString("\n\n```" + lang + "\n")
		sb.WriteString(strings.TrimRight(code, "\n"))
		sb.WriteString("\n```\n\n")
		return
	case "code":
		if parentTag(n) == "pre" {
			return // 已由 <pre> 整体处理
		}
		var inner strings.Builder
		renderChildren(n, &inner, st)
		t := strings.TrimSpace(inner.String())
		if t != "" {
			sb.WriteString("`" + t + "`")
		}
		return
	case "a":
		var inner strings.Builder
		renderChildren(n, &inner, st)
		txt := strings.TrimSpace(inner.String())
		if txt == "" {
			return
		}
		href := attr(n, "href")
		if href != "" && !strings.HasPrefix(strings.ToLower(href), "javascript:") {
			sb.WriteString(fmt.Sprintf("[%s](%s)", txt, href))
		} else {
			sb.WriteString(txt)
		}
		return
	case "table":
		renderTable(n, sb, st)
		return
	}

	switch tag {
	case "li":
		sb.WriteString("\n- ")
		renderChildren(n, sb, st)
		sb.WriteString("\n")
		return
	case "blockquote":
		sb.WriteString("\n\n> ")
		var inner strings.Builder
		renderChildren(n, &inner, st)
		sb.WriteString(strings.ReplaceAll(strings.TrimSpace(inner.String()), "\n", "\n> "))
		sb.WriteString("\n\n")
		return
	}

	if blockTags[tag] {
		sb.WriteString("\n")
		renderChildren(n, sb, st)
		sb.WriteString("\n")
		return
	}
	renderChildren(n, sb, st)
}

// renderTable 把 <table> 渲染成 Markdown 表格（首行作为表头，带分隔行）。
// 自动下沉 thead/tbody/tfoot，只取 <tr> 中的 <td>/<th>。
func renderTable(n *html.Node, sb *strings.Builder, st *renderState) {
	var rows [][]*html.Node
	var walkRows func(p *html.Node)
	walkRows = func(p *html.Node) {
		for c := p.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch strings.ToLower(c.Data) {
			case "tr":
				if cells := cellsOf(c); len(cells) > 0 {
					rows = append(rows, cells)
				}
			case "thead", "tbody", "tfoot":
				walkRows(c)
			}
		}
	}
	walkRows(n)
	if len(rows) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("\n")
	for i, row := range rows {
		b.WriteString("| ")
		for j, cell := range row {
			if j > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(cellText(cell, st))
		}
		b.WriteString(" |\n")
		if i == 0 {
			b.WriteString("|")
			for j := range row {
				b.WriteString("---")
				if j < len(row)-1 {
					b.WriteString("|")
				}
			}
			b.WriteString("|\n")
		}
	}
	sb.WriteString(b.String())
	sb.WriteString("\n")
}

// cellsOf 取一行 <tr> 中的 <td>/<th> 单元格。
func cellsOf(tr *html.Node) []*html.Node {
	var cells []*html.Node
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			d := strings.ToLower(c.Data)
			if d == "td" || d == "th" {
				cells = append(cells, c)
			}
		}
	}
	return cells
}

// cellText 渲染单元格内容并把内部换行/多余空白收敛成单行（Markdown 单元格内不能有换行）。
func cellText(cell *html.Node, st *renderState) string {
	var b strings.Builder
	renderChildren(cell, &b, st)
	s := strings.ReplaceAll(b.String(), "\n", " ")
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// detectLang 从 <pre>/<code> 的 class 里猜语言（如 language-go / lang-python）。
var reLang = regexp.MustCompile(`(?i)(?:language|lang|highlight-|brush:\s*)-?([a-z0-9+#]+)`)

func detectLang(n *html.Node) string {
	candidates := []string{attr(n, "class"), attr(n, "data-lang")}
	if c := n.FirstChild; c != nil && strings.EqualFold(c.Data, "code") {
		candidates = append(candidates, attr(c, "class"))
	}
	for _, s := range candidates {
		if m := reLang.FindStringSubmatch(s); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

func renderChildren(n *html.Node, sb *strings.Builder, st *renderState) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderNode(c, sb, st)
	}
}

var (
	reSpaces    = regexp.MustCompile(`[\t ]+`)
	reManyNL    = regexp.MustCompile(`\n{3,}`)
	reTrailSp   = regexp.MustCompile(`[ \t]+\n`)
	reBlankLine = regexp.MustCompile(`\n[\p{Zs}\x{00a0}]+\n`)
)

// tidy 收敛空白：行尾去空格、连续空行压成一段、去掉只含空白的行。
func tidy(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = reSpaces.ReplaceAllString(s, " ")
	s = reTrailSp.ReplaceAllString(s, "\n")
	s = reBlankLine.ReplaceAllString(s, "\n\n")
	s = reManyNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
