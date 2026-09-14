// Package csdn 封装对 CSDN 接口的调用：写（草稿/发布/更新/删除）、列表与读（公开文章）。
//
// 写接口采用“用户自己上传的 Cookie（+ 自动计算的 x-ca-* 网关签名）”方式鉴权，
// 对应 CSDN 网页编辑器 postedit/saveArticle 的真实请求。
//
// 由于 CSDN 未提供标准的第三方 OAuth2 写文章 API，这是个人自我委托场景下
// 唯一可用的写通道（详见 README 的合规性说明）。
//
// 关于 x-ca-* 网关签名：
//
//	CSDN 的 API 网关（openresty）要求每个写请求携带 HMAC-SHA256 签名头，
//	否则返回 405/401。签名算法取自 CSDN 编辑器前端 JS（appKey / appSecret 为
//	编辑器固定公开值），本客户端每次请求按相同算法重算签名，因此对任意正文都有效。
package csdn

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"csdn-mcp/internal/auth"
)

const (
	defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

	// CSDN 编辑器固定公开凭证（取自前端 JS，所有用户共用同一套网关密钥）。
	csdnAppKey    = "203803574"
	csdnAppSecret = "9znpamsyl2c7cdrr9sas0le9vbc3r6ba"

	// 生产环境 X-Ca-Stage 为空；仅 test-/loc-/pre- 域名才为 "PRE"。
	csdnXCaStage = ""

	// ---- 端点 ----
	defaultPublishEndpoint = "https://bizapi.csdn.net/blog-console-api/v1/postedit/saveArticle"
	defaultListEndpoint    = "https://bizapi.csdn.net/blog/phoenix/console/v1/article/list"
	defaultDeleteEndpoint  = "https://blog.csdn.net/phoenix/web/v1/articleListApi/del"
	defaultArticleURLFmt   = "https://blog.csdn.net/%s/article/details/%s"

	// 图片上传（图床）走 CSDN 官方两步流程（已从 CSDN 编辑器前端 csdn-upload.js 与
	// md 编辑器 app chunk 反编译确认）：
	//   1) 先向 bizapi 申请一次性 OBS 直传凭证（policy/signature/accessId/customParam…）；
	//   2) 再把图片 multipart 直传到华为云 OBS（provider=obs），OBS 回 200 + ETag 即落库；
	//   3) 最后把 OBS 对象 key 交给 external/storage 登记，拿回公开图床 URL。
	// 注意：第 3 步依赖 CSDN 服务端对 OBS 对象的回调用以发布（见 UploadImage 注释）。
	// 中文补充：图床 = CSDN 给博客提供的图片托管服务；OBS = 华为云对象存储。
	defaultImageSignatureEndpoint = "https://bizapi.csdn.net/resource-api/v1/image/direct/upload/signature"
	defaultImageStorageEndpoint   = "https://bizapi.csdn.net/resource-api/v1/image/external/storage"
	// imageAppName 是 CSDN 图床的应用名（编辑器固定值）。
	// 中文：申请直传凭证时要把这个 appName 带上，告诉 CSDN 是「博客直传」场景。
	imageAppName = "direct_blog"

	// maxImageBytes 限制上传图片体积。CSDN 单张约限制 5MB，这里略放宽到 8MB 防误伤。
	// 中文：本地或远程下载的图片超过这个大小会直接拒绝上传，避免在网关层吃 OOM/超时。
	maxImageBytes = 8 << 20
)

// ---- 状态码 ----
// 注意：CSDN 的两套接口对“已发布/草稿”用了不同的数字，切勿混用。
const (
	// saveArticle（写接口）的 status：
	WriteStatusPublish = 0 // 0 = 正式发布（对外可见，不可逆）
	WriteStatusDraft   = 2 // 2 = 存入草稿箱（安全）

	// article/list（列表接口）返回的 status：
	ListStatusPublished = 1 // 1 = 已发布
	ListStatusDraft     = 2 // 2 = 草稿

	// saveArticle 的 is_new：
	IsNewCreate = 1 // 新建
	IsNewUpdate = 0 // 更新已有文章（必须同时传 article_id）
)

// maxRawLen 限制回传给大模型的原始响应长度，避免把整份 HTML/响应塞爆上下文。
const maxRawLen = 2000

// ArticleRequest 描述一次文章发布/草稿请求。
type ArticleRequest struct {
	Title   string
	Content string
	Tags    []string
	Type    string // original / repost / translated
	// ReadType 仅对“发布”动作有意义（public/private）。
	// 注意：即便 ReadType=private，CSDN 的 status=0 仍会对外可见；
	// 真正不公开请使用草稿（PubStatus=draft）。
	ReadType string
	// PubStatus: draft / published。
	PubStatus string
	// ArticleID 非空时表示“更新已有文章”（is_new=0），否则为新建（is_new=1）。
	ArticleID         string
	Description       string
	CreationStatement int
}

// PublishResult 是对 CSDN 写接口响应的归一化结果。
type PublishResult struct {
	ArticleID string `json:"article_id,omitempty"`
	URL       string `json:"url,omitempty"`
	Code      int    `json:"code"`
	Message   string `json:"message"`
	// Landed 是本客户端的二次判定：CSDN 存在“返回 code=200 且 data 是字符串 '成功'
	// 但实际没有落库”的哑成功，只有 data 为带 article_id 的对象才算真正写入。
	Landed bool   `json:"landed"`
	Raw    string `json:"raw_response,omitempty"`
}

// ArticleSummary 是列表接口返回的单篇文章摘要。
type ArticleSummary struct {
	ArticleID string `json:"article_id"`
	Title     string `json:"title"`
	PostTime  string `json:"post_time,omitempty"`
	Status    int    `json:"status"`
	// StatusName 是 status 的可读形式：draft / published / unknown(n)。
	StatusName string `json:"status_name"`
	URL        string `json:"url,omitempty"`
	ViewCount  int    `json:"view_count,omitempty"`
}

// ListResult 是文章列表 + 计数。
type ListResult struct {
	DraftCount   int              `json:"draft_count"`
	AllCount     int              `json:"all_count"`
	DeletedCount int              `json:"deleted_count"`
	Count        int              `json:"count"`
	Articles     []ArticleSummary `json:"articles"`
	// Truncated 为 true 表示 CSDN 只回了第一页（该接口的 pageNum/pageSize 不生效），
	// 列表可能不完整，判重结果仅供参考。
	Truncated bool `json:"truncated"`
	// Error 非空表示解析响应失败，此时上面的计数都不可信。
	// 显式暴露而不是静默返回空列表，避免“看起来一篇都没有”这种误导。
	Error string `json:"error,omitempty"`
	Raw   string `json:"raw_response,omitempty"`
}

// DeleteResult 是删除接口的结果（CSDN 为软删除，进入回收站，可恢复）。
type DeleteResult struct {
	ArticleID string `json:"article_id"`
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Success   bool   `json:"success"`
	Raw       string `json:"raw_response,omitempty"`
}

// Client 是 CSDN HTTP 客户端。
type Client struct {
	publishEndpoint      string
	listEndpoint         string
	deleteEndpoint       string
	articleURLFmt        string
	imageSigEndpoint     string // 图床：申请 OBS 直传凭证 // 中文：第 1 步,拿一次性的上传「通行证」
	imageStorageEndpoint string // 图床：登记 OBS 对象、拿回公开 URL // 中文：第 3 步,把 OBS 对象登记成可外链的图床图片
	userAgent            string
	http                 *http.Client
	// maxRetries 控制对“可重试错误”（网络抖动 / 5xx / 429）的重试次数。
	maxRetries int
}

// NewClient 创建客户端；publishEndpoint 与 userAgent 为空时使用默认值。
func NewClient(publishEndpoint, userAgent string) *Client {
	if publishEndpoint == "" {
		publishEndpoint = defaultPublishEndpoint
	}
	if userAgent == "" {
		userAgent = defaultUA
	}
	return &Client{
		publishEndpoint:      publishEndpoint,
		listEndpoint:         defaultListEndpoint,
		deleteEndpoint:       defaultDeleteEndpoint,
		articleURLFmt:        defaultArticleURLFmt,
		imageSigEndpoint:     defaultImageSignatureEndpoint,
		imageStorageEndpoint: defaultImageStorageEndpoint,
		userAgent:            userAgent,
		http:                 &http.Client{Timeout: 30 * time.Second},
		maxRetries:           2,
	}
}

// ---- 写：草稿 / 发布 / 更新 ----

// saveArticleBody 对应 CSDN postedit/saveArticle 的 JSON 请求体。
// 关键字段：status(2=草稿/0=发布)、is_new(1=新建/0=更新)、not_auto_saved、source 等，
// 缺了 status/is_new 后端只返回哑值 "成功" 而不真正落库。
type saveArticleBody struct {
	Title             string   `json:"title"`
	Content           string   `json:"content"`
	MarkdownContent   string   `json:"markdowncontent"`
	ReadType          string   `json:"readType"`
	Level             int      `json:"level"`
	Tags              string   `json:"tags"`
	Status            int      `json:"status"`
	Categories        string   `json:"categories"`
	Type              string   `json:"type"`
	OriginalLink      string   `json:"original_link"`
	AuthorizedStatus  bool     `json:"authorized_status"`
	NotAutoSaved      string   `json:"not_auto_saved"`
	Source            string   `json:"source"`
	CoverImages       []string `json:"cover_images"`
	CoverType         int      `json:"cover_type"`
	IsNew             int      `json:"is_new"`
	VoteID            int      `json:"vote_id"`
	ResourceID        string   `json:"resource_id"`
	PubStatus         string   `json:"pubStatus"`
	CreatorActivityID string   `json:"creator_activity_id"`
	Description       string   `json:"description,omitempty"`
	CreationStatement int      `json:"creation_statement,omitempty"`
	ArticleID         string   `json:"article_id,omitempty"`
}

// Publish 调用 CSDN 写接口（草稿/发布/更新）。需要用户自己的 Cookie 凭证。
//   - req.ArticleID 为空 → 新建（is_new=1）
//   - req.ArticleID 非空 → 更新（is_new=0 + article_id）
//
// 业务限制（来自服务端校验，已实测）：
//   - 发布（status=0）时 tags 必填且 1~5 个，否则 400「请设置文章标签」。
//   - 已发布文章无法再转回草稿（status=2），服务端返回 400「已发布文章无法保存草稿！」，
//     要下线只能调用 DeleteArticle 删除（软删除进回收站）。
func (c *Client) Publish(ctx context.Context, cred *auth.Credential, req ArticleRequest) (*PublishResult, error) {
	if cred == nil || cred.Cookie == "" {
		return nil, fmt.Errorf("缺少 CSDN 凭证：请先调用 bind_csdn 或配置 CSDN_COOKIE")
	}
	pubStatus := req.PubStatus
	if pubStatus == "" {
		pubStatus = "draft"
	}
	typ := req.Type
	if typ == "" {
		typ = "original"
	}
	readType := req.ReadType
	if readType == "" {
		readType = "public"
	}

	status := WriteStatusDraft
	if pubStatus == "published" {
		status = WriteStatusPublish
	}

	isNew := IsNewCreate
	if req.ArticleID != "" {
		isNew = IsNewUpdate
	}

	htmlContent, markdownContent := renderArticleContent(req.Content)
	body := saveArticleBody{
		Title:             req.Title,
		Content:           htmlContent,
		MarkdownContent:   markdownContent,
		ReadType:          readType,
		Level:             0,
		Tags:              strings.Join(req.Tags, ","),
		Status:            status,
		Categories:        "",
		Type:              typ,
		OriginalLink:      "",
		AuthorizedStatus:  false,
		NotAutoSaved:      "1",
		Source:            "pc_mdeditor",
		CoverImages:       []string{},
		CoverType:         1,
		IsNew:             isNew,
		VoteID:            0,
		ResourceID:        "",
		PubStatus:         pubStatus,
		CreatorActivityID: "",
		Description:       req.Description,
		CreationStatement: req.CreationStatement,
		ArticleID:         req.ArticleID,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	method := http.MethodPost
	contentType := "application/json;" // 注意：末尾分号，无 charset（与 CSDN 编辑器一致）
	accept := "*/*"
	endpoint := c.publishEndpoint
	path := stripHost(endpoint)

	h := c.baseHeaders(cred)
	h.Set("Content-Type", contentType)
	h.Set("Accept", accept)
	h.Set("Referer", "https://editor.csdn.net/md/")

	// 必须先放好 X-Ca-Key / X-Ca-Nonce 再算签名（它们参与待签串）。
	stringToSign := buildStringToSign(method, path, accept, contentType, "", nil, h)
	h.Set("X-Ca-Signature", computeHMAC(stringToSign))
	h.Set("X-Ca-Signature-Headers", "x-ca-key,x-ca-nonce")

	respBody, statusCode, err := c.doWithRetry(ctx, method, endpoint, h, payload, false)
	if err != nil {
		return nil, err
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("CSDN 返回 %d: %s", statusCode, truncate(string(respBody), 500))
	}
	res := parsePublishResult(respBody)
	// 业务码非 200 时，把服务端原文一起抛回去，方便定位（如 400「请设置文章标签」）。
	if res.Code != 0 && res.Code != 200 {
		msg := res.Message
		if msg == "" {
			msg = truncate(string(respBody), 300)
		}
		return res, fmt.Errorf("CSDN 业务错误 code=%d: %s", res.Code, msg)
	}
	return res, nil
}

// parsePublishResult 兼容 CSDN 多种响应结构，尽量提取 articleId / url，
// 并判定是否真正落库（data 为对象而非字符串 "成功"）。
func parsePublishResult(body []byte) *PublishResult {
	res := &PublishResult{Raw: truncate(string(body), maxRawLen)}
	var generic struct {
		Code    int             `json:"code"`
		Message string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &generic); err != nil {
		return res
	}
	res.Code = generic.Code
	res.Message = generic.Message
	if len(generic.Data) == 0 {
		return res
	}
	// data 可能是字符串（如 "成功"）或对象（含 articleId/url）。
	var asStr string
	if json.Unmarshal(generic.Data, &asStr) == nil {
		if res.Message == "" && asStr != "" {
			res.Message = asStr
		}
		return res // 字符串型 data = 哑成功，没有落库
	}
	var d struct {
		ArticleID json.Number `json:"article_id"`
		URL       string      `json:"url"`
	}
	if err := json.Unmarshal(generic.Data, &d); err == nil && d.ArticleID.String() != "" {
		res.ArticleID = d.ArticleID.String()
		res.URL = d.URL
		res.Landed = true
		return res
	}
	// 与列表接口一样，article_id 也可能是字符串形式的数字。
	var ds struct {
		ArticleID string `json:"article_id"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(generic.Data, &ds); err == nil && ds.ArticleID != "" {
		res.ArticleID = ds.ArticleID
		res.URL = ds.URL
		res.Landed = true
	}
	return res
}

// ---- 列表 ----

// ListArticles 列出当前账号的文章（草稿 + 已发布）。
//
// 已知限制：CSDN 该接口的 pageNum/pageSize 参数不生效，只返回第一页（约 20 条），
// 因此结果可能不完整；做标题判重时请以 Truncated 字段提示调用方。
func (c *Client) ListArticles(ctx context.Context, cred *auth.Credential) (*ListResult, error) {
	if cred == nil || cred.Cookie == "" {
		return nil, fmt.Errorf("缺少 CSDN 凭证：请先调用 bind_csdn 或配置 CSDN_COOKIE")
	}
	method := http.MethodGet
	accept := "*/*"
	path := stripHost(c.listEndpoint)

	h := c.baseHeaders(cred)
	h.Set("Accept", accept)
	h.Set("Referer", "https://mp.csdn.net/mp_blog/manage/article?spm=1011.2124.3001.5448")

	// GET 不带 body，Content-Type 一行为空（与浏览器实际请求一致）。
	stringToSign := buildStringToSign(method, path, accept, "", "", nil, h)
	h.Set("X-Ca-Signature", computeHMAC(stringToSign))
	h.Set("X-Ca-Signature-Headers", "x-ca-key,x-ca-nonce")

	respBody, statusCode, err := c.doWithRetry(ctx, method, c.listEndpoint, h, nil, true)
	if err != nil {
		return nil, err
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("CSDN 列表接口返回 %d: %s", statusCode, truncate(string(respBody), 500))
	}
	return parseListResult(respBody), nil
}

// parseListResult 解析文章列表响应。
//
// 重要：CSDN 该接口把**所有数字都序列化成 JSON 字符串**
// （"articleId":"164264213"、"status":"1"、"viewCount":"0"），
// 因此这里一律用 string 接收再自行转换——用 int / json.Number 会整体解码失败，
// 表现为“列表永远是空的”。
func parseListResult(body []byte) *ListResult {
	res := &ListResult{Articles: []ArticleSummary{}, Raw: truncate(string(body), maxRawLen)}
	var generic struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Data    struct {
			Count map[string]int `json:"count"`
			List  []struct {
				ArticleID string `json:"articleId"`
				Title     string `json:"title"`
				PostTime  string `json:"postTime"`
				Status    string `json:"status"`
				ViewCount string `json:"viewCount"`
				// 列表响应里没有现成的 url 字段，但给了 username，可自行拼出文章链接。
				Username string `json:"username"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &generic); err != nil {
		res.Error = fmt.Sprintf("解析列表响应失败: %v", err)
		return res
	}
	if generic.Code != 0 && generic.Code != 200 {
		res.Error = fmt.Sprintf("CSDN 列表接口业务错误 code=%d: %s", generic.Code, generic.Message)
		return res
	}
	if c := generic.Data.Count; c != nil {
		res.DraftCount = c["draft"]
		res.AllCount = c["all"]
		res.DeletedCount = c["deleted"]
	}
	for _, it := range generic.Data.List {
		if it.ArticleID == "" {
			continue
		}
		status := atoiOr(it.Status, -1)
		url := ""
		if it.Username != "" {
			url = fmt.Sprintf(defaultArticleURLFmt, it.Username, it.ArticleID)
		}
		res.Articles = append(res.Articles, ArticleSummary{
			ArticleID:  it.ArticleID,
			Title:      it.Title,
			PostTime:   it.PostTime,
			Status:     status,
			StatusName: StatusName(status),
			URL:        url,
			ViewCount:  atoiOr(it.ViewCount, 0),
		})
	}
	res.Count = len(res.Articles)
	// 接口不支持分页，拿到的条数少于总数即视为截断。
	res.Truncated = res.AllCount > res.Count && res.Count > 0
	return res
}

// atoiOr 把 CSDN 的字符串型数字转成 int，失败时用默认值。
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// StatusName 把列表接口的 status 数字映射为可读名称。
func StatusName(listStatus int) string {
	switch listStatus {
	case ListStatusDraft:
		return "draft"
	case ListStatusPublished:
		return "published"
	case 0:
		return "reviewing"
	case 4:
		return "deleted"
	case -1:
		return "unknown"
	default:
		return fmt.Sprintf("unknown(%d)", listStatus)
	}
}

// FindByTitle 在列表里查找标题完全匹配（去空白后比较）的文章。
// 找不到返回 nil。注意列表只覆盖第一页，未命中不代表一定不存在。
func (c *Client) FindByTitle(ctx context.Context, cred *auth.Credential, title string) (*ArticleSummary, *ListResult, error) {
	list, err := c.ListArticles(ctx, cred)
	if err != nil {
		return nil, nil, err
	}
	// 列表解析失败时必须报错，否则会误判为“没有同名文章”从而重复创建。
	if list.Error != "" {
		return nil, list, fmt.Errorf("%s", list.Error)
	}
	want := normalizeTitle(title)
	for i := range list.Articles {
		if list.Articles[i].Title == want {
			return &list.Articles[i], list, nil
		}
	}
	return nil, list, nil
}

func normalizeTitle(s string) string {
	return strings.TrimSpace(s)
}

// ---- 删除 ----

// DeleteArticle 删除（软删除，进入回收站，可恢复）指定文章。草稿与已发布文章均有效。
//
// 坑：该接口位于 blog.csdn.net 域，且只认 application/x-www-form-urlencoded，
// 用 JSON body 传参会得到 40001「参数articleId不能为空」。
func (c *Client) DeleteArticle(ctx context.Context, cred *auth.Credential, articleID string) (*DeleteResult, error) {
	if cred == nil || cred.Cookie == "" {
		return nil, fmt.Errorf("缺少 CSDN 凭证：请先调用 bind_csdn 或配置 CSDN_COOKIE")
	}
	articleID = strings.TrimSpace(articleID)
	if articleID == "" {
		return nil, fmt.Errorf("article_id 不能为空")
	}
	if _, err := strconv.Atoi(articleID); err != nil {
		return nil, fmt.Errorf("article_id 必须是纯数字，收到 %q", articleID)
	}

	form := url.Values{}
	form.Set("articleId", articleID)

	method := http.MethodPost
	contentType := "application/x-www-form-urlencoded;"
	accept := "*/*"
	path := stripHost(c.deleteEndpoint)

	h := c.baseHeaders(cred)
	h.Set("Content-Type", contentType)
	h.Set("Accept", accept)
	h.Set("Referer", "https://mp.csdn.net/mp_blog/manage/article?spm=1011.2124.3001.5448")
	// 该域不走 bizapi 的网关签名，实测无需 X-Ca-*；但保留无害。
	stringToSign := buildStringToSign(method, path, accept, contentType, "", nil, h)
	h.Set("X-Ca-Signature", computeHMAC(stringToSign))
	h.Set("X-Ca-Signature-Headers", "x-ca-key,x-ca-nonce")

	respBody, statusCode, err := c.doWithRetry(ctx, method, c.deleteEndpoint, h, []byte(form.Encode()), false)
	if err != nil {
		return nil, err
	}
	res := &DeleteResult{ArticleID: articleID, Raw: truncate(string(respBody), maxRawLen)}
	if statusCode != http.StatusOK {
		return res, fmt.Errorf("CSDN 删除接口返回 %d: %s", statusCode, truncate(string(respBody), 500))
	}
	var generic struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &generic); err != nil {
		return res, fmt.Errorf("解析删除响应失败: %w（原文: %s）", err, truncate(string(respBody), 300))
	}
	res.Code = generic.Code
	res.Message = generic.Message
	var ok bool
	if err := json.Unmarshal(generic.Data, &ok); err == nil {
		res.Success = ok
	}
	if !res.Success || (res.Code != 200) {
		msg := res.Message
		if msg == "" {
			msg = truncate(string(respBody), 300)
		}
		return res, fmt.Errorf("删除失败 code=%d: %s", res.Code, msg)
	}
	return res, nil
}

// ---- 图片上传（图床）----

// ImageResult 是图片上传的归一化结果。
// 中文：无论走主路径（OBS 回调直接吐 URL）还是兜底路径（external/storage 登记），
// 最终都给调用方一份统一形态；失败时 FilePath 仍带上，方便人工到 CSDN 后台排查对象是否已落 OBS。
type ImageResult struct {
	URL      string `json:"url"`
	Code     int    `json:"code"`
	Message  string `json:"message"`
	FilePath string `json:"file_path,omitempty"` // OBS 对象 key，发布未生效时用于排查
	Raw      string `json:"raw_response,omitempty"`
}

// imageSignature 是 /image/direct/upload/signature 返回的 OBS 直传凭证。
// 中文：这张「通行证」是 OBS 校验请求合法性的依据，只能用一次；客户端拿到后要把
// 字段原样回填到 multipart 表单里（key/policy/signature/callback…）直传到 Host。
type imageSignature struct {
	Provider         string                 `json:"provider"`
	AccessID         string                 `json:"accessId"`
	Policy           string                 `json:"policy"`
	Signature        string                 `json:"signature"`
	Host             string                 `json:"host"`
	CallbackURL      string                 `json:"callbackUrl"`
	FilePath         string                 `json:"filePath"`
	CallbackBody     string                 `json:"callbackBody"`
	CallbackBodyType string                 `json:"callbackBodyType"`
	CustomParam      map[string]interface{} `json:"customParam"`
	SecurityToken    string                 `json:"securityToken"`
}

// UploadImage 把一张图片上传到 CSDN 图床，返回可在博客中直接引用的图床 URL。
// 需要用户自己的 Cookie 凭证。流程（从 CSDN 编辑器前端反编译确认）：
//
//  1. 向 bizapi 申请一次性 OBS 直传凭证（policy/signature/accessId/customParam…）；
//  2. 把图片 multipart 直传到华为云 OBS（provider=obs），OBS 回 200 + ETag 即对象落库；
//  3. 把 OBS 对象 key 交给 external/storage 登记，拿回公开图床 URL。
//
// 注意：第 3 步的公开 URL 依赖 CSDN 服务端对 OBS 对象的回调用（OBS→cb-api）把对象
// 发布到图床；若该回调用未生效（例如非浏览器/网关未触发），对象会停在 OBS 私有态，
// 此时本函数会返回 FilePath 并明确报错，而不是编造一个打不开的 URL。
// 中文补充：UploadImage 是整个 upload_image 工具的主入口；HTTP 调用按本函数串联，
// 任何一步失败都会冒泡到 MCP 客户端（tools 层）展示给最终用户。
func (c *Client) UploadImage(ctx context.Context, cred *auth.Credential, data []byte, filename, contentType string) (*ImageResult, error) {
	if cred == nil || cred.Cookie == "" {
		return nil, fmt.Errorf("缺少 CSDN 凭证：请先调用 bind_csdn 或配置 CSDN_COOKIE")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("图片数据为空")
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("图片过大（%d 字节），CSDN 单张约限制 5MB", len(data))
	}
	if contentType == "" {
		// 没拿到 MIME 时用 Go 标准库按文件头魔数识别，避免 OBS 那边类型校验不过。
		contentType = http.DetectContentType(data)
	}

	// 1) 申请 OBS 直传凭证
	sig, err := c.requestImageSignature(ctx, cred, imageSuffix(filename, contentType))
	if err != nil {
		return nil, err
	}

	// 2) 直传 OBS（对象落库，私有）
	obsBody, err := c.uploadToOBS(ctx, cred, sig, data, filename, contentType)
	if err != nil {
		return nil, err
	}

	// 若 OBS 把回调结果直接回传到响应体（生产环境常见），优先直接取 URL。
	// 中文：生产环境下 OBS 会同步触发 cb-api 回调，把 {url} 直接放进 OBS 的 HTTP 响应体，
	// 客户端不用再调第 3 步；这是「主路径」。
	if url := extractImageURL(obsBody); url != "" {
		return &ImageResult{URL: url, Code: 200, FilePath: sig.FilePath, Raw: truncate(string(obsBody), maxRawLen)}, nil
	}

	// 3) 登记并拿回公开 URL（编辑器即 transferImg -> external/storage）
	// 中文：OBS 偶尔不会同步回传回调结果（对象停在私有态），此时主动调 external/storage
	// 让 CSDN 把对象登记为公开图床图；这是「兜底路径」。
	url, raw, err := c.registerImage(ctx, cred, sig)
	if err != nil {
		// 中文：兜底路径也失败时，把 OBS 端已有的对象 key 一并返回，方便用户到 CSDN 后台
		// 找到这张图手动重发或删除，绝不伪造一个看似能打开的图床地址。
		return &ImageResult{
			Code:     500,
			Message:  "图片已上传至 CSDN OBS，但服务端未返回公开图床 URL（发布回调用未生效）",
			FilePath: sig.FilePath,
			Raw:      truncate(string(obsBody), maxRawLen),
		}, fmt.Errorf("CSDN 图片发布失败：%s（OBS key=%s）", err, sig.FilePath)
	}
	return &ImageResult{URL: url, Code: 200, FilePath: sig.FilePath, Raw: raw}, nil
}

// imageSuffix 从文件名或 MIME 推断图片后缀（不含点），供 signature 的 imageSuffix 字段使用。
// 中文：OBS 凭证里要带后缀，CSDN 据此生成 filePath；优先按文件扩展名猜，否则按 MIME。
func imageSuffix(filename, contentType string) string {
	if ext := path.Ext(filename); ext != "" {
		return strings.TrimPrefix(ext, ".")
	}
	switch {
	case strings.Contains(contentType, "png"):
		return "png"
	case strings.Contains(contentType, "jpeg"), strings.Contains(contentType, "jpg"):
		return "jpg"
	case strings.Contains(contentType, "gif"):
		return "gif"
	case strings.Contains(contentType, "webp"):
		return "webp"
	case strings.Contains(contentType, "bmp"):
		return "bmp"
	}
	return "png"
}

// requestImageSignature 申请一次性 OBS 直传凭证（x-ca 网关签名）。
// 中文：这是图床上传的第 1 步，相当于「申请一张一次性的 OBS 直传通行证」；
// 凭证里包含 policy（策略）、signature（OBS 校验用的 HMAC）、filePath（对象 key 模板）。
func (c *Client) requestImageSignature(ctx context.Context, cred *auth.Credential, suffix string) (*imageSignature, error) {
	method := http.MethodPost
	accept := "application/json, text/plain, */*"
	ctype := "application/json;charset=UTF-8"
	payload, _ := json.Marshal(map[string]string{
		"imageTemplate": "",
		"appName":       imageAppName,
		"imageSuffix":   suffix,
	})
	path := stripHost(c.imageSigEndpoint)
	h := c.baseHeaders(cred)
	h.Set("Content-Type", ctype)
	h.Set("Accept", accept)
	h.Set("Referer", "https://editor.csdn.net/md/")
	sts := buildStringToSign(method, path, accept, ctype, "", nil, h)
	h.Set("X-Ca-Signature", computeHMAC(sts))
	h.Set("X-Ca-Signature-Headers", "x-ca-key,x-ca-nonce")

	respBody, statusCode, err := c.doWithRetry(ctx, method, c.imageSigEndpoint, h, payload, false)
	if err != nil {
		return nil, err
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("CSDN 图床凭证接口返回 %d: %s", statusCode, truncate(string(respBody), 500))
	}
	var generic struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &generic); err != nil {
		return nil, fmt.Errorf("解析图床凭证响应失败: %w", err)
	}
	if generic.Code != 200 || len(generic.Data) == 0 {
		return nil, fmt.Errorf("CSDN 图床凭证接口业务错误 code=%d: %s", generic.Code, generic.Msg)
	}
	var sig imageSignature
	if err := json.Unmarshal(generic.Data, &sig); err != nil {
		return nil, fmt.Errorf("解析图床凭证数据失败: %w", err)
	}
	if sig.Host == "" || sig.FilePath == "" || sig.Policy == "" || sig.Signature == "" {
		return nil, fmt.Errorf("CSDN 图床凭证不完整（缺少 host/filePath/policy/signature）")
	}
	return &sig, nil
}

// uploadToOBS 把图片 multipart 直传到华为云 OBS。该请求是到 OBS 域名的纯表单上传，
// 不带 x-ca 网关签名（凭证已在表单字段 policy/signature/AccessKeyId 中）。
// 中文：图床第 2 步。本函数把 imageSignature 凭证按 OBS/OSS 两种 provider 的字段约定
// 拼成 multipart 表单，把图片二进制塞到 file 字段直传到 sig.Host；这一步只要 HTTP 200
// 就视为对象已落 OBS（私有态），无需关心响应内容——回调结果由 UploadImage 后续解析。
func (c *Client) uploadToOBS(ctx context.Context, cred *auth.Credential, sig *imageSignature, data []byte, filename, contentType string) ([]byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// flat 是写表单字段的小工具，把 (name, value) 写成 multipart 一行。
	flat := func(name, value string) error {
		return w.WriteField(name, value)
	}
	if err := flat("key", sig.FilePath); err != nil {
		return nil, err
	}
	if err := flat("policy", sig.Policy); err != nil {
		return nil, err
	}
	if err := flat("signature", sig.Signature); err != nil {
		return nil, err
	}
	if err := flat("callbackBody", sig.CallbackBody); err != nil {
		return nil, err
	}
	if err := flat("callbackBodyType", sig.CallbackBodyType); err != nil {
		return nil, err
	}
	if sig.Provider == "obs" {
		// 中文：OBS（华为云）分支：回调用 callbackUrl + AccessKeyId + 可选 STS token。
		if err := flat("callbackUrl", sig.CallbackURL); err != nil {
			return nil, err
		}
		if err := flat("AccessKeyId", sig.AccessID); err != nil {
			return nil, err
		}
		if sig.SecurityToken != "" {
			// 中文：仅临时 STS 凭证才会带 securityToken；长期 AK/SK 场景该字段为空。
			if err := flat("x-amz-security-token", sig.SecurityToken); err != nil {
				return nil, err
			}
		}
	} else {
		// 中文：OSS（阿里云）分支：回调字段名不一样（callback / OSSAccessKeyId），
		// 还要带 success_action_status=200 让 OBS 把响应体写成正常 JSON。
		if err := flat("callback", sig.CallbackURL); err != nil {
			return nil, err
		}
		if err := flat("OSSAccessKeyId", sig.AccessID); err != nil {
			return nil, err
		}
		if err := flat("success_action_status", "200"); err != nil {
			return nil, err
		}
	}
	for k, v := range sig.CustomParam {
		// 中文：CSDN 自定义回调字段统一加 x: 前缀，回调时一并回写到 cb-api。
		if err := flat("x:"+k, fmt.Sprintf("%v", v)); err != nil {
			return nil, err
		}
	}
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	body := buf.Bytes()
	ct := w.FormDataContentType()

	// OBS 直传到独立域名，仅需 Content-Type / Accept，不带 x-ca 与 Cookie。
	// 中文：这一步到的是 OBS 域名（不在 CSDN 网关后面），所以不需要 CSDN 的 x-ca 网关签名，
	// 也不应该带 Cookie（避免把用户凭证泄漏给 OBS 域）。
	h := make(http.Header)
	h.Set("Content-Type", ct)
	h.Set("Accept", "*/*")

	respBody, statusCode, err := c.doWithRetry(ctx, http.MethodPost, sig.Host, h, body, false)
	if err != nil {
		return nil, err
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("CSDN OBS 直传返回 %d: %s", statusCode, truncate(string(respBody), 500))
	}
	return respBody, nil
}

// registerImage 调用 external/storage 把 OBS 对象登记为公开图床图片，返回公开 URL。
// 中文：图床第 3 步（兜底路径）。OBS 直传后没同步回传 URL 时，主动告诉 CSDN「这个 OBS 对象
// key 就是我刚上传的那张图，请登记成公开图床图」；CSDN 会在内部把对象发布到 img-blog 子域。
func (c *Client) registerImage(ctx context.Context, cred *auth.Credential, sig *imageSignature) (string, string, error) {
	method := http.MethodPost
	accept := "application/json, text/plain, */*"
	ctype := "application/json;charset=UTF-8"
	payload, _ := json.Marshal(map[string]interface{}{
		"uniqueId":  fmt.Sprintf("csdn-mcp_%s", uuidV4()),
		"imgUrl":    sig.FilePath,
		"type":      "blog",
		"rtype":     "article",
		"isCrawler": 0,
		"nocache":   2,
	})
	path := stripHost(c.imageStorageEndpoint)
	h := c.baseHeaders(cred)
	h.Set("Content-Type", ctype)
	h.Set("Accept", accept)
	h.Set("Referer", "https://editor.csdn.net/md/")
	sts := buildStringToSign(method, path, accept, ctype, "", nil, h)
	h.Set("X-Ca-Signature", computeHMAC(sts))
	h.Set("X-Ca-Signature-Headers", "x-ca-key,x-ca-nonce")

	respBody, statusCode, err := c.doWithRetry(ctx, method, c.imageStorageEndpoint, h, payload, false)
	if err != nil {
		return "", "", err
	}
	if statusCode != http.StatusOK {
		return "", "", fmt.Errorf("CSDN 图床登记接口返回 %d: %s", statusCode, truncate(string(respBody), 500))
	}
	url := extractImageURL(respBody)
	if url == "" {
		return "", truncate(string(respBody), maxRawLen), fmt.Errorf("登记接口未返回 url（原文: %s）", truncate(string(respBody), 300))
	}
	return url, truncate(string(respBody), maxRawLen), nil
}

// extractImageURL 从 CSDN 响应体里尽量取出图床 URL（data.url / data.imageUrl / 嵌套）。
// 中文：CSDN 不同接口（OBS 回调 / external/storage / 老的 saveImage）的 data 字段形态不同
// （有的 url，有的 imageUrl，有的包一层），本函数只做「够用就行」的兜底解析。
func extractImageURL(body []byte) string {
	var generic struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &generic); err != nil {
		return ""
	}
	if len(generic.Data) == 0 {
		return ""
	}
	var obj struct {
		URL      string `json:"url"`
		ImageURL string `json:"imageUrl"`
	}
	if json.Unmarshal(generic.Data, &obj) == nil {
		if obj.URL != "" {
			return obj.URL
		}
		if obj.ImageURL != "" {
			return obj.ImageURL
		}
	}
	return ""
}

// DownloadImageForUpload 下载远程图片到内存，供后续 UploadImage 上传到 CSDN 图床。
// 做轻量 SSRF 防护：仅允许 http/https，且拒绝指向内网/环回地址的主机。
// 中文：当 upload_image 工具传入的是 image_url（远程）而不是 image_path（本地）时，
// 先把远程图片拉下来塞进内存，再交给 UploadImage 走图床流程；为防工具被滥用探测内网，
// 拒绝 127.x / 10.x / 192.168.x / 172.16-31.x 这类私网/环回地址。
func (c *Client) DownloadImageForUpload(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("image_url 非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("image_url 仅支持 http/https，收到 %q", u.Scheme)
	}
	// SSRF 防护：拒绝内网/环回字面量 IP。域名（如 localhost）无法静态判定，放行。
	if host := u.Hostname(); isPrivateHost(host) {
		return nil, "", fmt.Errorf("image_url 指向内网/环回地址，已拒绝: %s", host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "image/*,*/*")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("下载图片返回 %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("图片过大（%d 字节），CSDN 单张约限制 5MB", len(data))
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" || !strings.HasPrefix(ctype, "image/") {
		ctype = http.DetectContentType(data)
	}
	fn := path.Base(u.Path)
	if fn == "" || fn == "/" {
		fn = "image.png"
	}
	return data, fn, nil
}

// isPrivateHost 判断主机是否为环回/私网/未指定地址（SSRF 防护）。
// 仅对 IP 字面量做静态判定；域名无法静态判断，放行（由 CSDN 网关/用户自行负责）。
// 中文：host 可能带端口（127.0.0.1:8080），先剥掉端口再解析；
// IsLoopback 拦 127.x、IsPrivate 拦 10/172.16-31/192.168、IsUnspecified 拦 0.0.0.0。
func isPrivateHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified()
}

// parseImageUploadResult 兼容 CSDN 图片上传的多种响应结构，尽量提取图床 URL。
// 可能形态：data 为对象含 url / 字符串型 url / 含 url 的对象数组。
// 中文：与 extractImageURL 类似但更宽松——也兼容「data 是裸 URL」「data 是 [{...}]」等历史接口形态，
// 主要给单元测试和老 saveImage 兼容路径用，不参与当前主路径的 UploadImage。
func parseImageUploadResult(body []byte) *ImageResult {
	res := &ImageResult{Raw: truncate(string(body), maxRawLen)}
	var generic struct {
		Code    int             `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &generic); err != nil {
		return res
	}
	res.Code = generic.Code
	if generic.Msg != "" {
		res.Message = generic.Msg
	} else {
		res.Message = generic.Message
	}
	if len(generic.Data) == 0 {
		return res
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(generic.Data, &obj) == nil && obj.URL != "" {
		res.URL = obj.URL
		return res
	}
	var arr []struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(generic.Data, &arr) == nil && len(arr) > 0 {
		res.URL = arr[0].URL
		return res
	}
	var str string
	if json.Unmarshal(generic.Data, &str) == nil && str != "" {
		res.URL = str
	}
	return res
}

// ---- 读：公开文章 ----

// reUsername 限定 CSDN username 的合法字符，用于防止 get_article 的 URL 注入/SSRF。
var reUsername = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateArticleRef 校验 get_article 引用的 username / article_id。
// 不做此校验时，username 可拼出 "x@evil.com" 这类 URL，导致请求被解析到第三方/内网主机（SSRF）。
func ValidateArticleRef(username, articleID string) error {
	if username == "" || articleID == "" {
		return fmt.Errorf("username 与 article_id 均为必填")
	}
	if !reUsername.MatchString(username) {
		return fmt.Errorf("username 非法：只允许字母/数字/_/-，收到 %q", username)
	}
	if _, err := strconv.Atoi(articleID); err != nil {
		return fmt.Errorf("article_id 必须是纯数字，收到 %q", articleID)
	}
	return nil
}

// GetArticle 读取公开文章（无需任何凭证），返回结构化内容。
// wantRaw=true 时 Content 字段直接放原始 HTML（同样受 maxChars 截断）。
func (c *Client) GetArticle(ctx context.Context, username, articleID string, maxChars int, wantRaw bool) (*ArticleContent, error) {
	if err := ValidateArticleRef(username, articleID); err != nil {
		return nil, err
	}
	u := fmt.Sprintf(c.articleURLFmt, username, articleID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// 补上浏览器特征头：CSDN 会对过于“机器人化”的请求偶发返回 521 风控页。
	httpReq.Header.Set("User-Agent", c.userAgent)
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	httpReq.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	httpReq.Header.Set("Referer", "https://blog.csdn.net/")
	httpReq.Header.Set("Upgrade-Insecure-Requests", "1")
	respBody, statusCode, err := c.doWithRetry(ctx, http.MethodGet, u, httpReq.Header, nil, true)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusNotFound {
		return nil, fmt.Errorf("文章不存在或已下线（404）：%s", u)
	}
	if statusCode == 521 {
		return nil, fmt.Errorf("CSDN 返回 521：这是 CSDN 对自动化请求的风控页，不代表文章不存在。" +
			"请稍后重试，或确认该文章 URL 可正常访问；若持续出现，说明当前出口 IP 已被限流")
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("CSDN 返回 %d（若为 521 通常是 CSDN 对非浏览器 UA 的风控，非业务错误）", statusCode)
	}
	if wantRaw {
		// 调试用途：原样返回 HTML，同样截断以控制上下文占用。
		// 注意：truncate 按 rune（字符）截断，因此 Truncated 判定也要用 rune 数，
		// 不能用 len(respBody)（字节数）——中文 UTF-8 一字 3 字节会让后者恒为 true。
		bodyRunes := []rune(string(respBody))
		return &ArticleContent{
			ArticleID:    articleID,
			URL:          u,
			Content:      truncate(string(bodyRunes), maxChars),
			ContentChars: len(bodyRunes),
			Truncated:    len(bodyRunes) > maxChars,
		}, nil
	}
	art := ExtractArticle(string(respBody), maxChars)
	art.URL = u
	if art.ArticleID == "" {
		art.ArticleID = articleID
	}
	return art, nil
}

// ---- 通用请求 ----

// protectedHeaders 是禁止通过 extra_headers 覆盖的鉴权/身份关键头。
// 若允许用户随意覆盖这些头，会导致签名用错 key/nonce（网关 405）或被冒用身份。
var protectedHeaders = map[string]bool{
	"cookie":                 true,
	"user-agent":             true,
	"x-ca-key":               true,
	"x-ca-nonce":             true,
	"x-ca-signature":         true,
	"x-ca-signature-headers": true,
	"x-ca-stage":             true,
}

// baseHeaders 构造公共请求头（UA + Cookie + 用户自定义头 + x-ca 基础头）。
func (c *Client) baseHeaders(cred *auth.Credential) http.Header {
	h := make(http.Header)
	h.Set("User-Agent", c.userAgent)
	h.Set("X-Ca-Key", csdnAppKey)
	h.Set("X-Ca-Stage", csdnXCaStage)
	h.Set("X-Ca-Nonce", uuidV4())
	if cred != nil {
		h.Set("Cookie", cred.Cookie)
		// 仅允许"业务自定义头"；禁止覆盖鉴权/身份关键头（大小写不敏感）。
		// x-ca-* 签名头由本客户端按固定算法自动计算，用户无需也不应手动提供。
		for k, v := range cred.ExtraHeaders {
			if protectedHeaders[strings.ToLower(k)] {
				continue
			}
			h.Set(k, v)
		}
	}
	return h
}

// doWithRetry 发起请求。allowRetry=true 时对可重试错误（网络错误 / 5xx / 429）做指数退避重试；
// allowRetry=false 则不重试，直接返回首次结果。
//
// 重要：写接口（Publish/Delete）一律传 false——它们是**非幂等**操作，若首次请求其实已落库但响应
// 在返回路上丢失（表现为 5xx/网络断），重试会重复创建文章/重复提交。读接口（GetArticle/ListArticles）
// 是幂等的，可传 true。4xx 业务错误永远不重试（重试无意义且写接口可能重复提交）。
func (c *Client) doWithRetry(ctx context.Context, method, endpoint string, header http.Header, payload []byte, allowRetry bool) ([]byte, int, error) {
	maxAttempt := 0
	if allowRetry {
		maxAttempt = c.maxRetries
	}
	var lastErr error
	var lastBody []byte
	var lastCode int
	for attempt := 0; attempt <= maxAttempt; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(500*attempt) * time.Millisecond
			select {
			case <-ctx.Done():
				return lastBody, lastCode, ctx.Err()
			case <-time.After(backoff):
			}
		}
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return nil, 0, err
		}
		req.Header = header.Clone()

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue // 网络类错误，可重试
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastBody, lastCode = body, resp.StatusCode
		// 只对明确可恢复的服务端状态重试。
		// 注意：CSDN 的 521 是风控页，重试只会放大风控，必须直接失败并给出说明。
		switch resp.StatusCode {
		case http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
			http.StatusTooManyRequests:
			lastErr = fmt.Errorf("CSDN 返回 %d，可重试", resp.StatusCode)
			continue
		}
		return body, resp.StatusCode, nil
	}
	return lastBody, lastCode, lastErr
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	// 按 rune（字符）截断，避免把多字节 UTF-8（如中文）从中间切断产生乱码。
	return string(runes[:n]) + fmt.Sprintf("...(已截断，共%d字符)", len(runes))
}

// ---- x-ca 网关签名实现 ----

// stripHost 去掉 URL 的协议与主机部分，仅保留路径（及查询串），
// 等价于 CSDN 前端的正则：去掉 (http(s)?://)?(www\.)?[host].csdn.net 前缀。
func stripHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	p := u.Path
	if u.RawQuery != "" {
		p = p + "?" + u.RawQuery
	}
	return p
}

// buildStringToSign 构造 CSDN 网关待签字符串（与编辑器前端 Kfe 函数一致）：
//
//	METHOD\nACCEPT\n\nCONTENTTYPE\nDATE\n
//	x-ca-key:...\n
//	x-ca-nonce:...\n
//	PATH?QUERY
//
// Content-MD5 行恒为空；DATE 行在生产环境为空（前端不发送 Date 头）。
func buildStringToSign(method, path, accept, contentType, date string, params url.Values, headers http.Header) string {
	var b strings.Builder
	b.WriteString(method)
	b.WriteString("\n")
	b.WriteString(accept)
	b.WriteString("\n")
	b.WriteString("\n") // Content-MD5（恒为空）
	b.WriteString(contentType)
	b.WriteString("\n")
	b.WriteString(date)
	b.WriteString("\n")

	// 参与签名的 x-ca-* 头：仅 x-ca-key 与 x-ca-nonce（其余在签名计算后被排除）。
	canon := map[string]string{}
	for k, vs := range headers {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-ca-") {
			continue
		}
		switch lk {
		case "x-ca-key", "x-ca-nonce":
			if len(vs) > 0 {
				canon[lk] = vs[0]
			}
		}
	}
	keys := make([]string, 0, len(canon))
	for k := range canon {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(":")
		b.WriteString(canon[k])
		b.WriteString("\n")
	}

	// 路径 + 排序后的查询参数。
	p := path
	if len(params) > 0 {
		qkeys := make([]string, 0, len(params))
		for k := range params {
			qkeys = append(qkeys, k)
		}
		sort.Strings(qkeys)
		var qs strings.Builder
		for _, k := range qkeys {
			v := params[k]
			val := ""
			if len(v) > 0 {
				val = v[0]
			}
			if qs.Len() > 0 {
				qs.WriteString("&")
			}
			qs.WriteString(k)
			qs.WriteString("=")
			qs.WriteString(val)
		}
		p = p + "?" + qs.String()
	}
	b.WriteString(p)
	return b.String()
}

// computeHMAC 返回 Base64(HMAC-SHA256(secret, stringToSign))。
func computeHMAC(stringToSign string) string {
	mac := hmac.New(sha256.New, []byte(csdnAppSecret))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// uuidV4 生成 RFC4122 v4 UUID（无外部依赖）。
func uuidV4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
