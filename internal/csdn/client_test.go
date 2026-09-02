package csdn

// 单元测试覆盖三块最容易回归、且线上故障代价最高的逻辑：
//   1. x-ca 网关签名（签名错了 = 全部写请求 405）
//   2. CSDN 响应解析（尤其是“哑成功”判定：data 是字符串而非对象 = 没落库）
//   3. 文章页 HTML → 结构化正文提取
//
// 用 go test ./... 运行。

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// ---- 1. 签名 ----

func TestStripHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://bizapi.csdn.net/blog-console-api/v1/postedit/saveArticle", "/blog-console-api/v1/postedit/saveArticle"},
		{"https://blog.csdn.net/phoenix/web/v1/articleListApi/del", "/phoenix/web/v1/articleListApi/del"},
		{"https://bizapi.csdn.net/a/b?x=1&y=2", "/a/b?x=1&y=2"},
	}
	for _, c := range cases {
		if got := stripHost(c.in); got != c.want {
			t.Errorf("stripHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildStringToSignGolden 用固定输入锁定待签串格式：
// 一旦格式变化，线上会全部 405，必须靠这个用例立刻发现。
func TestBuildStringToSignGolden(t *testing.T) {
	h := http.Header{}
	h.Set("X-Ca-Key", "203803574")
	h.Set("X-Ca-Nonce", "fixed-nonce")
	got := buildStringToSign("POST", "/blog-console-api/v1/postedit/saveArticle", "*/*", "application/json;", "", nil, h)
	want := "POST\n" +
		"*/*\n" +
		"\n" +
		"application/json;\n" +
		"\n" +
		"x-ca-key:203803574\n" +
		"x-ca-nonce:fixed-nonce\n" +
		"/blog-console-api/v1/postedit/saveArticle"
	if got != want {
		t.Errorf("待签串不匹配:\n got=%q\nwant=%q", got, want)
	}
}

func TestBuildStringToSignSortedQuery(t *testing.T) {
	h := http.Header{}
	h.Set("X-Ca-Key", "k")
	h.Set("X-Ca-Nonce", "n")
	params := url.Values{"b": {"2"}, "a": {"1"}}
	got := buildStringToSign("GET", "/p", "*/*", "", "", params, h)
	if !strings.HasSuffix(got, "/p?a=1&b=2") {
		t.Errorf("查询参数未按字典序拼接: %q", got)
	}
}

// TestComputeHMACStable 验签结果必须稳定（同输入同输出），防止有人改成随机值。
func TestComputeHMACStable(t *testing.T) {
	a := computeHMAC("hello")
	b := computeHMAC("hello")
	if a != b || a == "" {
		t.Fatalf("HMAC 不稳定或为空: %q vs %q", a, b)
	}
	if a == computeHMAC("hello2") {
		t.Fatal("不同输入的 HMAC 不应相同")
	}
}

func TestUUIDV4(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u := uuidV4()
		if len(u) != 36 {
			t.Fatalf("UUID 长度异常: %q", u)
		}
		if u[14] != '4' {
			t.Fatalf("UUID 版本位不是 4: %q", u)
		}
		if seen[u] {
			t.Fatalf("UUID 重复: %q", u)
		}
		seen[u] = true
	}
}

// ---- 2. 响应解析 ----

func TestParsePublishResultRealLanded(t *testing.T) {
	// 真实响应中 article_id 是数字。
	body := []byte(`{"code":200,"msg":"保存成功。","data":{"article_id":164262209,"url":"https://blog.csdn.net/x/article/details/164262209"}}`)
	res := parsePublishResult(body)
	if !res.Landed {
		t.Fatalf("data 为对象应判定为已落库: %+v", res)
	}
	if res.ArticleID != "164262209" {
		t.Errorf("article_id 解析错误: %q", res.ArticleID)
	}
	if res.URL == "" {
		t.Error("url 未解析出来")
	}
}

// TestParsePublishResultStringIDs 列表接口用字符串数字，写接口理论上也可能变，
// 两种情况都要能识别，否则会误判为“没落库”。
func TestParsePublishResultStringIDs(t *testing.T) {
	body := []byte(`{"code":200,"msg":"保存成功。","data":{"article_id":"164262209","url":"https://x/y"}}`)
	res := parsePublishResult(body)
	if !res.Landed || res.ArticleID != "164262209" {
		t.Fatalf("字符串型 article_id 未识别: %+v", res)
	}
}

// TestParsePublishResultFakeSuccess 复现线上踩过的坑：
// 缺 status/is_new 时 CSDN 返回 code=200 且 data 是字符串 "成功"，但文章根本没落库。
func TestParsePublishResultFakeSuccess(t *testing.T) {
	body := []byte(`{"code":200,"data":"成功"}`)
	res := parsePublishResult(body)
	if res.Landed {
		t.Fatalf("data 为字符串不应判定为落库: %+v", res)
	}
	if res.ArticleID != "" {
		t.Errorf("不应解析出 article_id: %q", res.ArticleID)
	}
}

func TestParsePublishResultBusinessError(t *testing.T) {
	body := []byte(`{"code":400,"msg":"请设置文章标签"}`)
	res := parsePublishResult(body)
	if res.Code != 400 || res.Message != "请设置文章标签" {
		t.Errorf("业务错误解析失败: %+v", res)
	}
}

func TestParsePublishResultGarbage(t *testing.T) {
	res := parsePublishResult([]byte("<html>521</html>"))
	if res == nil {
		t.Fatal("非 JSON 响应不应 panic，且应返回结果")
	}
	if res.Raw == "" {
		t.Error("Raw 应保留原始响应以便排查")
	}
}

// TestParseListResult 用真实响应结构做样本。
// 关键：CSDN 列表接口把所有数字都序列化成字符串，早年按 int 解析导致列表恒为空。
func TestParseListResult(t *testing.T) {
	body := []byte(`{"code":200,"message":"success","traceId":"x","data":{
	  "count":{"all":72,"draft":12,"deleted":3,"publish":72,"private":13},
	  "page":1,"size":20,"total":72,
	  "list":[
	    {"articleId":"164264213","title":"A","postTime":"2026-09-01 17:02:50","status":"1",
	     "viewCount":"0","username":"abcefg_h"},
	    {"articleId":"164262120","title":"B","postTime":"2026-08-31 10:00:00","status":"2",
	     "viewCount":"12","username":"abcefg_h"}]}}`)
	res := parseListResult(body)
	if res.Error != "" {
		t.Fatalf("不应有解析错误: %s", res.Error)
	}
	if res.DraftCount != 12 || res.AllCount != 72 || res.DeletedCount != 3 {
		t.Errorf("计数解析错误: %+v", res)
	}
	if len(res.Articles) != 2 {
		t.Fatalf("列表条数错误: %d", len(res.Articles))
	}
	a := res.Articles[0]
	if a.ArticleID != "164264213" || a.Status != ListStatusPublished || a.StatusName != "published" {
		t.Errorf("首条解析错误: %+v", a)
	}
	if a.URL != "https://blog.csdn.net/abcefg_h/article/details/164264213" {
		t.Errorf("URL 拼接错误: %q", a.URL)
	}
	if res.Articles[1].Status != ListStatusDraft || res.Articles[1].ViewCount != 12 {
		t.Errorf("第二条解析错误: %+v", res.Articles[1])
	}
	if !res.Truncated {
		t.Error("总数 72 > 返回 2 条，应标记 Truncated")
	}
}

// TestParseListResultError 防止“解析失败却静默返回空列表”的回归。
func TestParseListResultError(t *testing.T) {
	res := parseListResult([]byte(`<html>502 Bad Gateway</html>`))
	if res.Error == "" {
		t.Fatal("解析失败必须写入 Error 字段，不能静默返回空列表")
	}
	if len(res.Articles) != 0 {
		t.Error("失败时不应产出文章条目")
	}
}

func TestParseListResultBusinessError(t *testing.T) {
	res := parseListResult([]byte(`{"code":401,"message":"Unauthorized"}`))
	if res.Error == "" || !strings.Contains(res.Error, "401") {
		t.Errorf("业务错误未被捕获: %+v", res)
	}
}

func TestParseListResultNullish(t *testing.T) {
	res := parseListResult([]byte(`{"code":200,"data":{"count":{"all":0},"list":null}}`))
	if res.Error != "" {
		t.Errorf("list 为 null 不应报错: %s", res.Error)
	}
	if len(res.Articles) != 0 {
		t.Error("list 为 null 时不应产出条目")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("短字符串不应截断: %q", got)
	}
	got := truncate("abcdef", 3)
	if !strings.HasPrefix(got, "abc") || !strings.Contains(got, "已截断") {
		t.Errorf("截断结果缺少提示: %q", got)
	}
}

func TestStatusName(t *testing.T) {
	cases := map[int]string{
		ListStatusDraft:     "draft",
		ListStatusPublished: "published",
		99:                  "unknown(99)",
	}
	for in, want := range cases {
		if got := StatusName(in); got != want {
			t.Errorf("StatusName(%d) = %q, want %q", in, got, want)
		}
	}
}

// ---- 3. HTML 提取 ----

const sampleArticleHTML = `<!DOCTYPE html><html><head>
<title>测试文章标题-CSDN博客</title>
<meta name="author" content="tester">
</head><body>
<div class="article-header-box">
  <h1 class="title-article">测试文章标题</h1>
  <div class="article-info-box">
     <span class="time">2026-08-31 10:00:00</span>
     <a class="follow-nickName">tester</a>
  </div>
  <div class="tags-box"><a class="tag-link">Go</a><a class="tag-link">MCP</a></div>
</div>
<article>
<div id="content_views" class="htmledit_views">
  <p>这是第一段正文。</p>
  <h2>小节标题</h2>
  <ul><li>要点一</li><li>要点二</li></ul>
  <pre><code class="language-go">fmt.Println("hello")</code></pre>
  <p>带 <code>行内代码</code> 与 <a href="https://example.com">链接</a> 的段落。</p>
  <script>var evil = 1;</script>
  <style>.x{color:red}</style>
</div>
</article>
</body></html>`

func TestExtractArticleBasic(t *testing.T) {
	art := ExtractArticle(sampleArticleHTML, 0)
	if art.Title != "测试文章标题" {
		t.Errorf("标题提取错误: %q", art.Title)
	}
	if art.Author != "tester" {
		t.Errorf("作者提取错误: %q", art.Author)
	}
	if art.PublishTime != "2026-08-31 10:00:00" {
		t.Errorf("发布时间提取错误: %q", art.PublishTime)
	}
	if len(art.Tags) != 2 || art.Tags[0] != "Go" || art.Tags[1] != "MCP" {
		t.Errorf("标签提取错误: %v", art.Tags)
	}
	// script/style 必须被剔除
	if strings.Contains(art.Content, "evil") || strings.Contains(art.Content, "color:red") {
		t.Errorf("script/style 未被剔除: %q", art.Content)
	}
}

func TestExtractArticleMarkdownShape(t *testing.T) {
	art := ExtractArticle(sampleArticleHTML, 0)
	c := art.Content
	want := []string{
		"这是第一段正文。",
		"## 小节标题",
		"- 要点一",
		"```go",
		`fmt.Println("hello")`,
		"`行内代码`",
		"[链接](https://example.com)",
	}
	for _, w := range want {
		if !strings.Contains(c, w) {
			t.Errorf("正文缺少 %q\n实际正文:\n%s", w, c)
		}
	}
}

func TestExtractArticleMaxChars(t *testing.T) {
	long := "<html><body><div id=\"content_views\">" + strings.Repeat("字", 5000) + "</div></body></html>"
	art := ExtractArticle(long, 100)
	if !art.Truncated {
		t.Error("超长正文应标记 Truncated")
	}
	if !strings.Contains(art.Content, "已截断") {
		t.Error("截断后应给出提示")
	}
}

func TestExtractArticleNoContentFallback(t *testing.T) {
	// 没有 #content_views 时不应 panic，且能退回到整页文本。
	art := ExtractArticle("<html><body><p>只有一段话</p></body></html>", 0)
	if !strings.Contains(art.Content, "只有一段话") {
		t.Errorf("回退提取失败: %q", art.Content)
	}
}

func TestExtractArticleEmpty(t *testing.T) {
	art := ExtractArticle("", 0)
	if art == nil {
		t.Fatal("空 HTML 不应返回 nil")
	}
}

// ---- 4. 加固：SSRF 校验 + 中文安全截断 ----

func TestValidateArticleRef(t *testing.T) {
	cases := []struct {
		u, id string
		ok    bool
	}{
		{"someuser", "123456", true},
		{"user_name-1", "164262209", true},
		{"x@evil.com", "123", false},        // SSRF：@ 把 host 改成 evil.com
		{"user/../other", "123", false},     // 路径注入
		{"user space", "123", false},        // 含空白
		{"user", "abc", false},              // article_id 非数字
		{"", "123", false},
		{"user", "", false},
	}
	for _, c := range cases {
		err := ValidateArticleRef(c.u, c.id)
		if c.ok && err != nil {
			t.Errorf("ValidateArticleRef(%q,%q) 应为合法，却报错: %v", c.u, c.id, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateArticleRef(%q,%q) 应被拒绝，却通过", c.u, c.id)
		}
	}
}

func TestTruncateRune(t *testing.T) {
	s := "中文测试abc123"
	// 截断后应以前 4 个 rune 开头，且不会产生乱码（仍合法 UTF-8）。
	got := truncate(s, 4)
	if !strings.HasPrefix(got, "中文测试") {
		t.Errorf("rune 截断前缀错误: %q", got)
	}
	if got := truncate(s, 100); got != s {
		t.Errorf("短串不应截断: %q", got)
	}
	got2 := truncate("你好世界", 2)
	if !strings.HasPrefix(got2, "你好") {
		t.Errorf("中文截断异常: %q", got2)
	}
	// 截断后字符串必须是合法 UTF-8（按 rune 截断保证这一点），且至少含前 2 个 rune。
	if len([]rune(got2)) < 2 {
		t.Errorf("截断后 rune 数异常: %q", got2)
	}
}
