# csdn-mcp 本地化改造说明

## 结论
按需求把 `csdn-mcp` 从**远程 StreamableHTTP 部署**改为**本地 stdio 运行**。先是零改动切换（见下），后续（2026-09-01 末）进一步**从 `main.go` 彻底移除了 StreamableHTTP 传输与 Bearer 鉴权代码**，成为纯 stdio 单二进制，无网络监听。

## 改动清单

| 项 | 改动前 | 改动后 |
|---|---|---|
| 传输方式 | StreamableHTTP（`http://62.234.218.164:18080/mcp` + Bearer） | 本地 stdio 进程 |
| 二进制 | `csdn-mcp.exe`（8/31 旧版，无假成功修复） | 重编译（含 `status`/`is_new`/`article_id` 修复） |
| Cookie | 存远程服务器内存（重启即失效） | 本地 `mcp.json` 的 `env.CSdn_COOKIE`，启动即载入 |
| 门禁 token | Bearer `mcp_db6f...`（公网防冒用） | 移除（本地无需） |

## 验证结果（stdio 握手实测）
- `initialize` → `serverInfo: {name: csdn-mcp, version: 0.1.0}`，协议 `2024-11-05`
- `tools/list` → 9 个工具全部注册：`bind_csdn` / `unbind_csdn` / `create_article` / `update_article` / `publish_article` / `list_articles` / `delete_article` / `get_article` / `upload_image`
- 启动日志：`已从环境变量载入默认 CSDN 凭证（default 绑定）` → 本地写能力就绪

## 收尾进度（已全部完成 ✅）

| 收尾项 | 状态 |
|---|---|
| 停用远程 systemd 服务 | ✅ `disabled` + `inactive`，18080 端口已释放 |
| 关闭腾讯云防火墙 18080 | ✅ 已删除规则（实例 ap-beijing，18888 等保留） |
| 回收公开测试文章 `164262209` | ✅ 已删除，匿名访问 404 下线 |
| 清理草稿测试稿 `164261912` / `164262120` | ✅ 已删除（`count.deleted 0→3`、`draft 12→10`） |

> 关键结论：CSDN 已发布文章**无法转回草稿**（400「已发布文章无法保存草稿！」），只能删除；删除接口为 `POST blog.csdn.net/phoenix/web/v1/articleListApi/del`（form 传 `articleId`）。

## get_article 正文提取质量修复（2026-09-01 补充）

`internal/csdn/extract.go` 的 `ExtractArticle` 重构了渲染层，解决三类提取噪声：

| 问题 | 修复前 | 修复后 |
|------|--------|--------|
| CSDN 自动「目录」 | 整段锚点链接混入正文 | 检测 `<h3>目录</h3>` 后用 TOC 状态机整段跳过，直到 `<hr/>` |
| `<table>` 表格 | 单元格被拍平成无列分隔的散行 | 渲染为标准 Markdown 表格（首行作表头 + `|---|` 分隔行） |
| 封面图假标题 | `<h3><img/></h3>` 渲染成 `### ![](…)` | 标题内仅含一张图片时降级为普通图片，不加 `#` |

验证：对真实文章 `163948772` 端到端抓取，`get_article` 返回已无「目录」、含两处正确 Markdown 表格、封面图正确降级；`go test ./...` 全过；`tools/list` 9 工具全部注册。

## 你还需要做的（仅此一项）
- **信任连接器**：到 WorkBuddy「连接器管理」→ 自定义连接器 → 找到 `csdn-mcp` → 点「信任」启用（配置改动后不会自动激活）。

## 注意事项
- Cookie 明文存在本机 `~/.workbuddy/mcp.json`，**不要外发该文件**；Cookie 会过期，失效后需更新配置里的 `CSDN_COOKIE`。
