# csdn-mcp

用 Go 实现的 **CSDN MCP Server**，让大模型（Claude / Cursor / 你自己的 Agent）能够查询 CSDN 文章、并以**用户自己上传的 Cookie** 创建草稿、发布文章。

技术栈：[mark3labs/mcp-go](https://github.com/mark3labs/mcp-go)。单二进制，**固定以 stdio 本地运行**（客户端直接拉起子进程通信，无网络监听）。

---

## 工具清单

| 工具 | 类型 | 是否需凭证 | 说明 |
|------|------|-----------|------|
| `get_article` | 读 | 否 | 读取公开文章正文（username + article_id，二者均做格式校验以防范 SSRF），返回结构化 Markdown：自动剥离 CSDN 自动生成的「目录」、把 `<table>` 还原为 Markdown 表格、保留代码块/图片/链接，默认截断到 8000 字符 |
| `create_article` | 写 | 是（用户 Cookie） | 创建草稿（不对外公开）；默认按标题判重避免重复，确认要再建同名草稿请传 `force=true` |
| `update_article` | 写 | 是 | 覆盖式更新已有文章；**title 与 content 必须同时提供完整内容**（任一为空会清空对应字段，且草稿正文无法通过接口读回）；已发布文章无法转回草稿，要下线请用 `delete_article` |
| `publish_article` | 写 | 是 | 正式发布（对外可见，且无法转回草稿）；支持 `dry_run` 预览；未传 `article_id` 直接新建并发布需 `confirm=true`，否则降级为草稿；**传 `article_id` 时 `title` 与 `content` 也必须完整提供**（覆盖式接口，缺字段会把原标题/正文清空，且草稿正文无法读回） |
| `list_articles` | 写 | 是 | 列出账号下草稿 + 已发布文章（含草稿/总数/回收站计数）；注意：CSDN 列表接口不支持分页，只回第一页约 20 条 |
| `delete_article` | 写 | 是 | 软删除文章进回收站（草稿/已发布均可，可恢复）；必须 `confirm=true` 才真正执行，否则只返回预览 |
| `bind_csdn` | 绑定 | 用户自行提供 | 运行时上传并绑定自己的 Cookie 凭证（仅存内存，不落盘）；可选 `persist=true` 加密落盘（需 `CSDN_KEY` + `CSDN_CREDENTIAL_FILE`）。**注意：`persist` 仅对 `default` 绑定生效，非 default 绑定会被忽略**，否则重启后凭证会错位成 default |
| `unbind_csdn` | 绑定 | 用户自行提供 | 撤销并清除指定 binding_id 的凭证（仅内存移除，随时找回对账号的控制权） |

> **正文格式**：`create_article` / `update_article` / `publish_article` 的 `content` 接受 **Markdown**，发布时由 [goldmark](https://github.com/yuin/goldmark) 自动渲染为 CSDN 展示用的 HTML（若内容本身已是 HTML，请以 `<` 开头以便正确识别）；原始 Markdown 源码同时存入 `markdowncontent`，保留编辑器内可编辑能力。这样标题 / 表格 / 代码块等才会被正确排版，而非把 `#`、`**` 当成纯文本显示。

---

## 关于鉴权与合规性（务必先读）

CSDN **没有**面向个人开发者的、文档完善的标准 OAuth2 写文章 API（已核实 `openapi.csdn.net` 对自动抓取返回 403，且网传的 OAuth 发布接口无可验证来源）。
因此写功能只能走 CSDN 网页编辑器实际使用的接口，凭证就是**用户自己的登录态**。

本项目采用 **「自我委托」模式**：

- 用户**自己**登录 CSDN，把**自己的** Cookie 交给**自己的**工具使用；
- 不冒充他人、不批量爬取、不代他人操作；
- 单用户本地场景下合规风险低。

⚠️ **高风险灰区**：把本服务作为远程服务器、**托管多名其他用户的 Cookie**，会变成“凭证保管人”，合规与安全风险显著上升。

**本项目只支持本地 stdio 单用户**：当前构建已移除全部网络传输与远程部署能力，客户端直接拉起本进程、通过标准输入/输出通信，**不存在任何网络监听**——因此“公网冒用账号发/删文章”这类远程暴露面在架构上已被消除，Cookie 只来自用户自己的环境变量，不跨账户共享。若将来确有远程多用户需求，必须重做传输层并为每用户独立进程实例（各自 `CSDN_COOKIE`），切勿依赖 `binding_id` 做隔离（`binding_id` 只是字符串，不能替代进程级隔离）。

---

## 三种“上传 Cookie”的方式

任选其一即可（按优先级）：

### 1. 环境变量（推荐，最简单）
```bash
export CSDN_COOKIE="你的CSDN登录Cookie"
go run .            # 默认 stdio
```

### 2. 配置文件
复制 `config.json.example` 为 `config.json`，填入 `csdn.default_cookie`（以及可选的 `extra_headers` 里的 `x-ca-*`）。

### 3. 运行时调用 `bind_csdn` 工具
适合临时上传：由用户本人在对话里调用 `bind_csdn`，传入自己的 Cookie。
```json
{
  "cookie": "你的CSDN登录Cookie",
  "binding_id": "default",
  "persist": false
}
```

### 如何拿到 Cookie（用户本人操作）
1. 浏览器登录 CSDN；
2. 打开 `https://editor.csdn.net/md/`，随便写点什么点“保存草稿”；
3. F12 → Network → 找到 `saveArticle` 请求 → 复制其中的 `Cookie` 请求头；
4. `x-ca-*` 网关签名头由本客户端按固定算法**自动计算**，无需也不应手动提供；若仅 Cookie 调用被网关拒绝，多半是 Cookie 过期或缺少关键字段，请刷新 Cookie，而非手动补 `x-ca-*`。`extra_headers` 仅用于承载业务自定义头，受保护的鉴权/身份头（Cookie / User-Agent / X-Ca-*）会被忽略。

---

## 运行

```bash
# 本地 stdio（Claude Desktop / Cursor / MCP Inspector）
export CSDN_COOKIE="..."
go run .
```

### 接入 Claude Desktop（示例）
```json
{
  "mcpServers": {
    "csdn": {
      "command": "go",
      "args": ["run", "/绝对路径/csdn-mcp"],
      "env": { "CSDN_COOKIE": "你的CSDN登录Cookie" }
    }
  }
}
```

### 用 MCP Inspector 调试
```bash
npx @modelcontextprotocol/inspector go run .
```

---

## 生产环境补强建议

- **加密落盘（可选）**：默认凭证仅存于内存，重启即失。若希望落盘，调用 `bind_csdn(persist=true)` 并同时设置环境变量 `CSDN_KEY`（主密钥，任意字符串）与 `CSDN_CREDENTIAL_FILE`（输出路径），凭证会以 AES-GCM 加密写入该文件（权限 0600）；启动时若未提供 `CSDN_COOKIE` 会自动从该文件恢复。**注意：`persist` 仅对 `default` 绑定生效，非 default 绑定不会被落盘/恢复**；明文 Cookie 仍可能存在于 `mcp.json` 的 `env` 段，请确保该文件权限为 0600 且不外发。
- **限流**：CSDN 接口有频控，建议单次 5–10s、每日 ≤5 篇；当前客户端已对非幂等写请求**禁用自动重试**，避免重复发文。
- **撤销**：已提供 `unbind_csdn` 工具，可随时清除指定 binding 的凭证。
- **架构**：当前版本仅本地 stdio，无网络暴露面；远程多用户隔离需求（如有）须通过独立进程实例实现，不能依赖 `binding_id`。

---

## 目录结构

```
csdn-mcp/
├── main.go                 # 装配（固定 stdio 传输）
├── config.json.example     # 配置示例
├── internal/
│   ├── auth/store.go       # 凭证存储（按 binding_id）+ 脱敏 + 可选 AES-GCM 加密落盘
│   ├── csdn/client.go      # CSDN 写/读 HTTP 客户端（注入 Cookie + x-ca-* 网关签名）
│   ├── csdn/extract.go     # 文章页 HTML → 结构化 Markdown 提取（剥离目录/表格还原/代码块）
│   ├── csdn/markdown.go    # 正文 Markdown → CSDN 展示用 HTML 渲染（goldmark，GFM 扩展）
│   └── tools/tools.go      # MCP 工具注册与处理函数（另含 auth/store_test.go、csdn/client_test.go、csdn/markdown_test.go、tools/tools_test.go 单测）
└── README.md
```
