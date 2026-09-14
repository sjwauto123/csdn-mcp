// Package tools 注册 CSDN MCP Server 暴露给大模型调用的工具。
//
// 工具清单：
//   - create_article : 创建草稿（写，需用户 Cookie；按标题判重，避免重复）
//   - publish_article: 正式发布（写，需用户 Cookie；支持 dry_run 预览与草稿转发布）
//   - update_article : 更新已有文章（写，需用户 Cookie）
//   - list_articles  : 列出草稿/已发布文章（写权限，需 Cookie）
//   - delete_article : 删除文章（写，软删除进回收站）
//   - get_article    : 读取公开文章（读，无需凭证）
//   - bind_csdn      : 上传并绑定用户自己的 Cookie 凭证（自我委托）
//   - unbind_csdn    : 撤销并清除指定 binding_id 的凭证
//   - upload_image   : 把图片上传到 CSDN 图床并返回可外链的图床 URL
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"csdn-mcp/internal/auth"
	"csdn-mcp/internal/csdn"
)

// Register 把所有工具注册到 MCP Server。
func Register(s *server.MCPServer, store *auth.Store, client *csdn.Client) {
	s.AddTool(createArticleTool(), createArticleHandler(store, client))
	s.AddTool(publishArticleTool(), publishArticleHandler(store, client))
	s.AddTool(updateArticleTool(), updateArticleHandler(store, client))
	s.AddTool(listArticlesTool(), listArticlesHandler(store, client))
	s.AddTool(deleteArticleTool(), deleteArticleHandler(store, client))
	s.AddTool(getArticleTool(), getArticleHandler(client))
	s.AddTool(bindTool(), bindHandler(store))
	s.AddTool(unbindTool(), unbindHandler(store))
	s.AddTool(uploadImageTool(), uploadImageHandler(store, client))
}

// ---- 参数辅助 ----

func strArg(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// sliceArg 解析逗号分隔的标签列表，并做清洗（去空白、去重、去空项）。
// CSDN 服务端要求标签以英文逗号分隔，且数量 1~5 个。
// 兼容模型常把英文逗号输成中文逗号（，）的情况，统一按两种逗号切分。
func sliceArg(args map[string]any, key string) []string {
	raw, ok := args[key].(string)
	if !ok || raw == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，'
	}) {
		p := strings.TrimSpace(part)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// boolArg 兼容 JSON 布尔与字符串 "true"/"1"/"yes"。
func boolArg(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "y":
			return true
		}
	case float64:
		return v != 0
	case int:
		return v != 0
	}
	return false
}

// intArg 兼容 JSON number 与字符串数字。
func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func jsonText(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// getCred 取凭证。
func getCred(store *auth.Store, args map[string]any) (*auth.Credential, error) {
	return store.Get(strArg(args, "binding_id", "default"))
}

// validateArticle 在调用前做本地校验，避免把明显会被服务端拒绝的请求发出去。
// forPublish=true 时按发布标准校验（标签必填）。
func validateArticle(title, content string, tags []string, description string, forPublish bool) []string {
	var errs []string
	if strings.TrimSpace(title) == "" {
		errs = append(errs, "title 不能为空")
	}
	if strings.TrimSpace(content) == "" {
		errs = append(errs, "content 不能为空")
	}
	if forPublish {
		// CSDN 服务端硬校验：发布必须有 1~5 个标签，否则 400「请设置文章标签」。
		if len(tags) == 0 {
			errs = append(errs, "发布必须有至少一个标签（CSDN 服务端校验：1~5 个）")
		} else if len(tags) > 5 {
			errs = append(errs, fmt.Sprintf("标签最多 5 个，当前 %d 个", len(tags)))
		}
	} else if len(tags) > 5 {
		errs = append(errs, fmt.Sprintf("标签最多 5 个，当前 %d 个", len(tags)))
	}
	if len([]rune(description)) > 256 {
		errs = append(errs, fmt.Sprintf("description 最多 256 字，当前 %d 字", len([]rune(description))))
	}
	return errs
}

// ---- 工具定义 ----

func createArticleTool() mcp.Tool {
	return mcp.NewTool("create_article",
		mcp.WithDescription("在 CSDN 创建一篇博客草稿（安全：不会对外公开）。需要用户自己的 Cookie 凭证（自我委托模式）。默认会先按标题检查是否已有同题文章，避免重复创建；确认要重复创建请传 force=true。"),
		mcp.WithString("title", mcp.Required(), mcp.Description("文章标题")),
		mcp.WithString("content", mcp.Required(), mcp.Description("文章正文（Markdown 会自动渲染为 CSDN 展示用 HTML，保留标题/表格/代码等排版；若本身已是 HTML 请以 < 开头以便正确识别）")),
		mcp.WithString("tags", mcp.Description("逗号分隔的标签，最多5个")),
		mcp.WithString("type", mcp.Description("original/repost/translated，默认 original")),
		mcp.WithString("readType", mcp.Description("public/private，默认 public（草稿阶段不对外可见）")),
		mcp.WithBoolean("force", mcp.Description("已存在同标题文章时仍强制新建，默认 false")),
		mcp.WithString("binding_id", mcp.Description("凭证绑定ID，默认 default")),
	)
}

func publishArticleTool() mcp.Tool {
	return mcp.NewTool("publish_article",
		mcp.WithDescription("在 CSDN 正式发布一篇博客（对外可见，且无法直接转回草稿，只能删除下线）。推荐流程：先 create_article 建草稿 → 确认无误后再 publish_article(article_id=草稿ID) 发布。直接新建并发布需要显式传 confirm=true。"),
		mcp.WithString("title", mcp.Description("文章标题；传 article_id 时也必须完整提供（CSDN 保存接口为覆盖式，缺字段会清空原标题）")),
		mcp.WithString("content", mcp.Description("文章正文（Markdown 会自动渲染为 HTML）；传 article_id 时也必须完整提供（CSDN 保存接口为覆盖式，缺字段会清空原正文）")),
		mcp.WithString("article_id", mcp.Description("把已存在的草稿转为正式发布；省略则表示新建并发布")),
		mcp.WithString("tags", mcp.Description("逗号分隔的标签，发布时必填且1~5个")),
		mcp.WithString("description", mcp.Description("文章摘要，最多256字")),
		mcp.WithString("readType", mcp.Description("public/private，默认 public。注意：CSDN 的 status=0 发布后通常对外可见，private 不能保证不公开")),
		mcp.WithString("creation_statement", mcp.Description("创作声明：0无/1AI辅助/2网络整合/3个人观点")),
		mcp.WithBoolean("dry_run", mcp.Description("只做本地校验并预览，不真正调用 CSDN 接口，默认 false")),
		mcp.WithBoolean("confirm", mcp.Description("未传 article_id 时，必须显式传 true 才会直接新建并发布；否则自动降级为创建草稿")),
		mcp.WithString("binding_id", mcp.Description("凭证绑定ID，默认 default")),
	)
}

func updateArticleTool() mcp.Tool {
	return mcp.NewTool("update_article",
		mcp.WithDescription("更新 CSDN 上已有文章（覆盖式：请传完整的 title 与 content）。默认更新后仍存为草稿（publish=false）；传 publish=true 则把草稿正式发布。注意：已发布文章无法再转回草稿，要下线请用 delete_article。"),
		mcp.WithString("article_id", mcp.Required(), mcp.Description("要更新的文章 ID")),
		mcp.WithString("title", mcp.Description("新的完整标题（覆盖式；必须与 content 同时提供，否则对应字段会被清空）")),
		mcp.WithString("content", mcp.Description("新的完整正文（Markdown 会自动渲染为 HTML；覆盖式；必须与 title 同时提供，否则对应字段会被清空）")),
		mcp.WithString("tags", mcp.Description("逗号分隔的标签，最多5个；发布时必填")),
		mcp.WithString("description", mcp.Description("文章摘要，最多256字")),
		mcp.WithString("readType", mcp.Description("public/private，默认 public")),
		mcp.WithString("type", mcp.Description("original/repost/translated，默认 original")),
		mcp.WithBoolean("publish", mcp.Description("true=更新后直接正式发布；false=保存为草稿，默认 false")),
		mcp.WithString("binding_id", mcp.Description("凭证绑定ID，默认 default")),
	)
}

func listArticlesTool() mcp.Tool {
	return mcp.NewTool("list_articles",
		mcp.WithDescription("列出当前 CSDN 账号的文章（草稿 + 已发布）。注意：CSDN 该接口不支持分页，只返回第一页约 20 条，结果可能不完整。"),
		mcp.WithString("status_filter", mcp.Description("按状态过滤：all（默认）/ draft / published")),
		mcp.WithNumber("limit", mcp.Description("最多返回多少条，默认 20")),
		mcp.WithString("binding_id", mcp.Description("凭证绑定ID，默认 default")),
	)
}

func deleteArticleTool() mcp.Tool {
	return mcp.NewTool("delete_article",
		mcp.WithDescription("删除 CSDN 文章（软删除，进入回收站，可恢复）。草稿与已发布文章均可删除，这是让已发布文章下线的唯一方式。需要 confirm=true 才真正执行。"),
		mcp.WithString("article_id", mcp.Required(), mcp.Description("要删除的文章 ID（纯数字）")),
		mcp.WithBoolean("confirm", mcp.Description("确认删除，必须显式传 true 才会执行")),
		mcp.WithString("binding_id", mcp.Description("凭证绑定ID，默认 default")),
	)
}

func getArticleTool() mcp.Tool {
	return mcp.NewTool("get_article",
		mcp.WithDescription("读取 CSDN 公开文章并返回结构化内容（标题/作者/发布时间/标签/Markdown 风格正文），无需任何凭证。默认截断到 8000 字符以避免占用过多上下文。"),
		mcp.WithString("username", mcp.Required(), mcp.Description("CSDN 用户名")),
		mcp.WithString("article_id", mcp.Required(), mcp.Description("文章 ID（URL 中 details/ 后的数字）")),
		mcp.WithNumber("max_chars", mcp.Description("正文最大字符数，默认 8000")),
		mcp.WithBoolean("raw", mcp.Description("true 时返回原始 HTML（同样受 max_chars 截断），默认 false")),
	)
}

func bindTool() mcp.Tool {
	return mcp.NewTool("bind_csdn",
		mcp.WithDescription("上传并绑定用户自己的 CSDN Cookie 凭证（自我委托）。凭证仅保存在本进程内存中，不会落盘。"),
		mcp.WithString("cookie", mcp.Required(), mcp.Description("用户自己的 CSDN 登录 Cookie")),
		mcp.WithString("binding_id", mcp.Description("绑定ID，默认 default")),
		mcp.WithString("extra_headers", mcp.Description("可选，JSON 字符串形式的额外请求头。注意：x-ca-* 签名头由客户端自动计算，无需也不应手动提供；受保护的鉴权/身份头（Cookie/User-Agent/X-Ca-*）会被忽略")),
		mcp.WithBoolean("persist", mcp.Description("true 时尝试把凭证加密落盘（需同时设置环境变量 CSDN_KEY 与 CSDN_CREDENTIAL_FILE），默认 false（仅内存）")),
	)
}

func unbindTool() mcp.Tool {
	return mcp.NewTool("unbind_csdn",
		mcp.WithDescription("撤销并清除指定 binding_id 的 CSDN 凭证（自我委托模式下允许用户随时移除自己的凭证）。仅从本进程内存移除，不落盘；重启即失效的加密文件需另行删除。"),
		mcp.WithString("binding_id", mcp.Description("要撤销的绑定ID，默认 default")),
	)
}

// ---- 处理函数 ----

func createArticleHandler(store *auth.Store, client *csdn.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		title := strArg(args, "title", "")
		content := strArg(args, "content", "")
		tags := sliceArg(args, "tags")
		if errs := validateArticle(title, content, tags, "", false); len(errs) > 0 {
			return mcp.NewToolResultError("参数校验失败: " + strings.Join(errs, "; ")), nil
		}
		cred, err := getCred(store, args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// 幂等：同标题已存在则不再创建（除非 force）。
		warn := ""
		if !boolArg(args, "force") {
			hit, list, err := client.FindByTitle(ctx, cred, title)
			if err != nil {
				// 判重失败不阻断创建，只带一句警告继续。
				warn = fmt.Sprintf("警告：按标题判重失败（%v），已跳过判重直接创建。\n", err)
			} else if hit != nil {
				msg := fmt.Sprintf("已存在同标题文章，未重复创建：\n%s\n\n"+
					"如需覆盖内容请用 update_article(article_id=%s)；确认要再建一篇同名草稿请传 force=true。",
					jsonText(hit), hit.ArticleID)
				if list != nil && list.Truncated {
					msg += fmt.Sprintf("\n注意：列表接口只返回第一页（%d/%d 条），判重结果可能不完整。", list.Count, list.AllCount)
				}
				return mcp.NewToolResultText(msg), nil
			}
		}

		res, err := client.Publish(ctx, cred, csdn.ArticleRequest{
			Title:     title,
			Content:   content,
			Tags:      tags,
			Type:      strArg(args, "type", "original"),
			ReadType:  strArg(args, "readType", "public"),
			PubStatus: "draft",
		})
		if err != nil {
			if res != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%v\n服务端返回: %s", err, jsonText(res))), nil
			}
			return mcp.NewToolResultError(err.Error()), nil
		}
		if !res.Landed {
			return mcp.NewToolResultError(fmt.Sprintf("疑似未落库：CSDN 返回 200 但 data 不是文章对象（哑成功）。\n服务端返回: %s", jsonText(res))), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("%s草稿已创建（未对外公开）：\n%s\n\n确认内容无误后，可用 publish_article(article_id=%s) 正式发布。", warn, jsonText(res), res.ArticleID)), nil
	}
}

func publishArticleHandler(store *auth.Store, client *csdn.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		articleID := strArg(args, "article_id", "")
		title := strArg(args, "title", "")
		content := strArg(args, "content", "")
		tags := sliceArg(args, "tags")
		description := strArg(args, "description", "")
		dryRun := boolArg(args, "dry_run")

		// 传了 article_id 时，Title/Content 仍必须提供完整内容：CSDN 的 saveArticle
		// 是覆盖式接口，缺字段会把原标题/正文清空，且草稿正文无法通过接口读回。
		if articleID != "" && (title == "" || content == "") {
			return mcp.NewToolResultError("传 article_id 时 title 与 content 也必须给全：CSDN 保存接口是覆盖式的，缺字段会清空原文；而草稿正文无法通过接口读回，请复用你创建草稿时的正文。"), nil
		}
		if errs := validateArticle(title, content, tags, description, true); len(errs) > 0 {
			return mcp.NewToolResultError("参数校验失败: " + strings.Join(errs, "; ")), nil
		}

		cs := intArg(args, "creation_statement", 0)

		if dryRun {
			preview := map[string]any{
				"dry_run":       true,
				"action":        map[bool]string{true: "发布已有草稿", false: "新建并发布"}[articleID != ""],
				"article_id":    articleID,
				"title":         title,
				"tags":          tags,
				"read_type":     strArg(args, "readType", "public"),
				"content_chars": len([]rune(content)),
				"description":   description,
				"risks": []string{
					"发布后文章对外可见",
					"已发布文章无法转回草稿（CSDN 服务端限制），下线只能 delete_article 删除（进回收站，可恢复）",
					"readType=private 不能保证不公开",
				},
			}
			return mcp.NewToolResultText("预览（未调用 CSDN 接口）：\n" + jsonText(preview)), nil
		}

		cred, err := getCred(store, args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// 未指定 article_id 且未显式确认 → 降级为创建草稿，避免误公开发布。
		if articleID == "" && !boolArg(args, "confirm") {
			res, err := client.Publish(ctx, cred, csdn.ArticleRequest{
				Title:             title,
				Content:           content,
				Tags:              tags,
				Type:              strArg(args, "type", "original"),
				ReadType:          strArg(args, "readType", "public"),
				PubStatus:         "draft",
				Description:       description,
				CreationStatement: cs,
			})
			if err != nil {
				if res != nil {
					return mcp.NewToolResultError(fmt.Sprintf("%v\n服务端返回: %s", err, jsonText(res))), nil
				}
				return mcp.NewToolResultError(err.Error()), nil
			}
			if !res.Landed {
				return mcp.NewToolResultError(fmt.Sprintf("疑似未落库：CSDN 返回 200 但 data 不是文章对象（哑成功）。\n服务端返回: %s", jsonText(res))), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("未收到显式确认，已降级为创建草稿（未对外公开）：\n%s\n\n"+
				"确认无误后调用 publish_article(article_id=%s) 正式发布；或重新调用本工具并传 confirm=true 直接新建并发布。",
				jsonText(res), res.ArticleID)), nil
		}

		res, err := client.Publish(ctx, cred, csdn.ArticleRequest{
			Title:             title,
			Content:           content,
			Tags:              tags,
			Type:              strArg(args, "type", "original"),
			ReadType:          strArg(args, "readType", "public"),
			PubStatus:         "published",
			ArticleID:         articleID,
			Description:       description,
			CreationStatement: cs,
		})
		if err != nil {
			if res != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%v\n服务端返回: %s", err, jsonText(res))), nil
			}
			return mcp.NewToolResultError(err.Error()), nil
		}
		if !res.Landed {
			return mcp.NewToolResultError(fmt.Sprintf("疑似未生效：CSDN 返回 200 但 data 不是文章对象（哑成功）。\n服务端返回: %s", jsonText(res))), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("文章已发布（对外可见，且无法直接转回草稿）：\n%s", jsonText(res))), nil
	}
}

func updateArticleHandler(store *auth.Store, client *csdn.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		articleID := strArg(args, "article_id", "")
		if articleID == "" {
			return mcp.NewToolResultError("article_id 必填"), nil
		}
		title := strArg(args, "title", "")
		content := strArg(args, "content", "")
		tags := sliceArg(args, "tags")
		description := strArg(args, "description", "")
		publish := boolArg(args, "publish")

		// CSDN 保存接口为整篇覆盖式：title/content 任一为空都会把对应字段清空，
		// 且草稿正文无法通过接口读回，因此两者必须同时给出完整内容（与 publish_article 一致）。
		if title == "" || content == "" {
			return mcp.NewToolResultError("title 与 content 都必须提供完整内容：CSDN 保存接口为覆盖式，任一为空都会清空对应字段，且草稿正文无法通过接口读回，请复用你创建/上次更新时的正文。"), nil
		}
		if errs := validateArticle(title, content, tags, description, publish); len(errs) > 0 {
			return mcp.NewToolResultError("参数校验失败: " + strings.Join(errs, "; ")), nil
		}

		cred, err := getCred(store, args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		req2 := csdn.ArticleRequest{
			Title:       title,
			Content:     content,
			Tags:        tags,
			Type:        strArg(args, "type", "original"),
			ReadType:    strArg(args, "readType", "public"),
			ArticleID:   articleID,
			Description: description,
		}
		if publish {
			req2.PubStatus = "published"
		} else {
			req2.PubStatus = "draft"
		}
		res, err := client.Publish(ctx, cred, req2)
		if err != nil {
			if res != nil {
				hint := ""
				if strings.Contains(err.Error(), "已发布文章无法保存草稿") {
					hint = "\n提示：已发布文章无法转回草稿（CSDN 服务端限制）。要让它下线，请用 delete_article 删除（软删除进回收站，可恢复）。"
				}
				return mcp.NewToolResultError(fmt.Sprintf("%v\n服务端返回: %s%s", err, jsonText(res), hint)), nil
			}
			return mcp.NewToolResultError(err.Error()), nil
		}
		if !res.Landed {
			return mcp.NewToolResultError(fmt.Sprintf("疑似未生效：CSDN 返回 200 但 data 不是文章对象（哑成功）。\n服务端返回: %s", jsonText(res))), nil
		}
		action := "草稿已更新"
		if publish {
			action = "文章已更新并正式发布"
		}
		return mcp.NewToolResultText(fmt.Sprintf("%s：\n%s", action, jsonText(res))), nil
	}
}

func listArticlesHandler(store *auth.Store, client *csdn.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		cred, err := getCred(store, args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		list, err := client.ListArticles(ctx, cred)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if list.Error != "" {
			return mcp.NewToolResultError(fmt.Sprintf("%s\n原始响应前 500 字符: %s", list.Error, list.Raw)), nil
		}
		filter := strings.ToLower(strArg(args, "status_filter", "all"))
		limit := intArg(args, "limit", 20)
		if limit <= 0 {
			limit = 20
		}
		out := make([]csdn.ArticleSummary, 0, len(list.Articles))
		for _, a := range list.Articles {
			switch filter {
			case "draft":
				if a.Status != csdn.ListStatusDraft {
					continue
				}
			case "published":
				if a.Status != csdn.ListStatusPublished {
					continue
				}
			}
			out = append(out, a)
			if len(out) >= limit {
				break
			}
		}
		view := map[string]any{
			"total_draft":   list.DraftCount,
			"total_all":     list.AllCount,
			"total_deleted": list.DeletedCount,
			"shown":         len(out),
			"truncated":     list.Truncated,
			"articles":      out,
		}
		// 提示放进 JSON 字段而不是追加在 JSON 后面，保证输出可被程序直接解析。
		if list.Truncated {
			view["notice"] = "CSDN 列表接口不支持分页，只返回第一页，以上可能不是全部文章。"
		}
		return mcp.NewToolResultText(jsonText(view)), nil
	}
}

func deleteArticleHandler(store *auth.Store, client *csdn.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		articleID := strArg(args, "article_id", "")
		if articleID == "" {
			return mcp.NewToolResultError("article_id 必填"), nil
		}
		if !boolArg(args, "confirm") {
			return mcp.NewToolResultText(fmt.Sprintf(
				"未执行删除。确认要删除文章 %s 吗？\n"+
					"- 该操作为软删除，文章进入回收站，可在 CSDN 后台恢复\n"+
					"- 已发布文章删除后将立即对外不可见\n"+
					"确认请重新调用并传 confirm=true。", articleID)), nil
		}
		cred, err := getCred(store, args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		res, err := client.DeleteArticle(ctx, cred, articleID)
		if err != nil {
			if res != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%v\n服务端返回: %s", err, jsonText(res))), nil
			}
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("文章已删除（软删除，进回收站，可恢复）：\n%s", jsonText(res))), nil
	}
}

func getArticleHandler(client *csdn.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		username := strArg(args, "username", "")
		articleID := strArg(args, "article_id", "")
		// 先校验再发请求，避免把任意字符串拼进 URL 造成 SSRF/内网探测。
		if err := csdn.ValidateArticleRef(username, articleID); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		maxChars := intArg(args, "max_chars", 8000)
		if maxChars <= 0 {
			maxChars = 8000
		}
		art, err := client.GetArticle(ctx, username, articleID, maxChars, boolArg(args, "raw"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if boolArg(args, "raw") {
			return mcp.NewToolResultText(art.Content), nil
		}
		return mcp.NewToolResultText(jsonText(art)), nil
	}
}

func bindHandler(store *auth.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id := strArg(args, "binding_id", "default")
		cookie := strArg(args, "cookie", "")
		if cookie == "" {
			return mcp.NewToolResultError("cookie 不能为空"), nil
		}
		extra := map[string]string{}
		if raw, ok := args["extra_headers"].(string); ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &extra); err != nil {
				return mcp.NewToolResultError("extra_headers 不是合法 JSON: " + err.Error()), nil
			}
		}
		if err := store.Bind(id, cookie, extra); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		msg := fmt.Sprintf(
			"已绑定 CSDN 凭证到 %q（cookie 脱敏: %s）。凭证仅存于本进程内存，不落盘。",
			id, auth.Credential{Cookie: cookie}.Masked())

		// 可选加密持久化：仅当明确 persist=true 且配置了密钥与路径时落盘（AES-GCM，文件权限 0600）。
		// 注意：加密文件只在启动时按 "default" 绑定恢复（见 main.loadEncryptedDefault），
		// 因此 persist 只对 default 绑定有意义；非 default 绑定 persist 会被忽略并提示，
		// 否则重启后凭证会错位成 default。
		if boolArg(args, "persist") {
			if id != "default" {
				msg += "\n注意：persist=true 仅支持 default 绑定（加密文件不支持多绑定区分），已忽略落盘，凭证仍在内存。"
			} else {
				key := os.Getenv("CSDN_KEY")
				path := os.Getenv("CSDN_CREDENTIAL_FILE")
				if key == "" || path == "" {
					msg += "\n注意：persist=true 但缺少 CSDN_KEY 或 CSDN_CREDENTIAL_FILE，未落盘，凭证仍在内存。"
				} else if err := auth.PersistDefault(path, &auth.Credential{Cookie: cookie, ExtraHeaders: extra}, key); err != nil {
					msg += fmt.Sprintf("\n持久化失败: %v（凭证仍在内存，可用 unbind_csdn 撤销）", err)
				} else {
					msg += fmt.Sprintf("\n凭证已加密落盘（AES-GCM，0600）：%s", path)
				}
			}
		}
		return mcp.NewToolResultText(msg), nil
	}
}

func unbindHandler(store *auth.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id := strArg(args, "binding_id", "default")
		if err := store.Unbind(id); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("已撤销并清除绑定 %q 的凭证（仅内存移除；若曾加密落盘，请另行删除对应文件）。", id)), nil
	}
}

// uploadImageTool 定义 upload_image 工具的 schema（名称、参数、说明）。
// 中文：MCP 工具的 Description 字段会原样作为「大模型看到的使用说明」渲染给客户端，
// 因此用一段连贯中文描述工作流，把参数约束（image_path / image_url 二选一、拒绝内网）
// 和常见用法（拿到 URL 后 ![](URL) 插图）写在描述里。
func uploadImageTool() mcp.Tool {
	return mcp.NewTool("upload_image",
		mcp.WithDescription("把一张图片上传到 CSDN 图床，返回可在博客中直接引用的图床 URL（形如 https://img-blog.csdnimg.cn/...）。拿到 URL 后，把它放进 create_article / publish_article 的 content 正文里，用 Markdown 语法 ![](URL) 即可插入图片。支持两种来源：本地图片路径（image_path）或远程图片 URL（image_url），二选一。需要用户自己的 Cookie 凭证（自我委托模式）。"),
		mcp.WithString("image_path", mcp.Description("本地图片文件路径（与 image_url 二选一），例如 /tmp/diagram.png")),
		mcp.WithString("image_url", mcp.Description("远程图片 URL（与 image_path 二选一），会自动下载后上传到 CSDN 图床；仅支持 http/https，且拒绝内网地址")),
		mcp.WithString("binding_id", mcp.Description("凭证绑定ID，默认 default")),
	)
}

// uploadImageHandler 是 upload_image 工具的请求处理函数。
// 中文：流程=取凭证 → 二选一（远程下载/本地读文件）→ 调 UploadImage 上图床 → 把 URL 拼成
// 「直接可用的 Markdown 图片语法」作为 usage 字段返回，方便模型下一步拼进正文。
func uploadImageHandler(store *auth.Store, client *csdn.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		cred, err := getCred(store, args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		pathArg := strArg(args, "image_path", "")
		urlArg := strArg(args, "image_url", "")
		if pathArg == "" && urlArg == "" {
			return mcp.NewToolResultError("image_path 与 image_url 至少提供一个"), nil
		}

		var data []byte
		var filename, ctype string
		switch {
		case urlArg != "":
			// 中文：image_url 走远程下载分支，内部已做 SSRF 防护（拒绝内网/环回）。
			data, filename, err = client.DownloadImageForUpload(ctx, urlArg)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		default:
			// 本地文件：防止把 URL 误传给路径参数。
			// 中文：模型有时会把 https://... 直接塞到 image_path,这里拦截并提示改用 image_url。
			if strings.HasPrefix(pathArg, "http://") || strings.HasPrefix(pathArg, "https://") || strings.HasPrefix(pathArg, "file://") {
				return mcp.NewToolResultError("image_path 应为本地文件路径；若传远程图片请用 image_url"), nil
			}
			data, err = os.ReadFile(pathArg)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("读取本地图片失败: %v", err)), nil
			}
			ctype = http.DetectContentType(data)
			filename = filepath.Base(pathArg)
		}

		res, err := client.UploadImage(ctx, cred, data, filename, ctype)
		if err != nil {
			// 中文：UploadImage 在 OBS 已落对象但发布未生效时,会同时返回 err 和 res
			// （res.FilePath 是 OBS 对象 key）——一并回显给模型,方便用户到 CSDN 后台排查。
			if res != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%v\n服务端返回: %s", err, jsonText(res))), nil
			}
			return mcp.NewToolResultError(err.Error()), nil
		}
		out := map[string]any{
			"url":          res.URL,
			"usage":        "把该 URL 以 Markdown 图片语法 ![](" + res.URL + ") 放入正文即可在文章中显示",
			"raw_response": res.Raw,
		}
		return mcp.NewToolResultText(jsonText(out)), nil
	}
}
