// Command csdn-mcp 是一个用 Go 实现的 CSDN MCP Server。
//
// 设计要点：
//   - 本地以 stdio 传输运行（Claude Desktop / Cursor 等客户端直接拉起子进程通信）。
//   - 写文章使用“用户自己上传的 Cookie”凭证（自我委托模式），由 internal/auth 按 bindingID 管理。
//   - 读文章（get_article）无需任何凭证。
//
// 合规性：本服务只持有一个用户“自己的”登录态，用于该用户本人操作自己的账号，
// 不属于冒充或批量爬取。生产环境部署到远程多用户时，应为每个用户独立绑定其本人凭证，
// 并做好加密存储与随时撤销（详见 README）。
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/server"

	"csdn-mcp/internal/auth"
	"csdn-mcp/internal/csdn"
	"csdn-mcp/internal/tools"
)

// CSDNConfig 对应配置文件里的 csdn 段。
type CSDNConfig struct {
	PublishEndpoint string            `json:"publish_endpoint"`
	DefaultCookie   string            `json:"default_cookie"`
	ExtraHeaders    map[string]string `json:"extra_headers"`
	UserAgent       string            `json:"user_agent"`
}

// ServerConfig 对应配置文件里的 server 段（仅保留标识信息，传输固定为 stdio）。
type ServerConfig struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Config 是程序整体配置。
type Config struct {
	Server ServerConfig `json:"server"`
	CSDN   CSDNConfig   `json:"csdn"`
}

func defaultConfig() *Config {
	return &Config{
		Server: ServerConfig{Name: "csdn-mcp", Version: "0.1.0"},
		CSDN:   CSDNConfig{PublishEndpoint: "https://bizapi.csdn.net/blog-console-api/v1/postedit/saveArticle"},
	}
}

// loadConfig 读取 config.json，并用环境变量覆盖关键字段。
//
// config.json 按以下顺序查找（configCandidates 实际把 CSDN_CONFIG 提到最高优先级），
// 避免 stdio 模式下因工作目录不同（客户端启动进程的目录往往不是程序所在目录）而静默读不到配置：
//  1. CSDN_CONFIG 环境变量指定的绝对路径（若有，最高优先级，便于容器/按需指定）
//  2. 可执行文件所在目录
//  3. 当前工作目录
func loadConfig() *Config {
	cfg := defaultConfig()
	for _, p := range configCandidates() {
		if data, err := os.ReadFile(p); err == nil {
			// 解析失败时尝试下一个候选文件，避免一个坏文件挡住其余正确配置。
			tmp := defaultConfig()
			if err := json.Unmarshal(data, tmp); err != nil {
				log.Printf("WARN: 解析配置 %s 失败: %v，尝试下一个候选", p, err)
				continue
			}
			cfg = tmp
			log.Printf("INFO: 已载入配置文件 %s", p)
			break
		}
	}
	if v := os.Getenv("CSDN_COOKIE"); v != "" {
		cfg.CSDN.DefaultCookie = v
	}
	return cfg
}

func configCandidates() []string {
	cands := []string{}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "config.json"))
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "config.json"))
	}
	if p := os.Getenv("CSDN_CONFIG"); p != "" {
		cands = append([]string{p}, cands...)
	}
	return cands
}

// loadEncryptedDefault 尝试从加密凭证文件恢复 default 凭证（需同时设置 CSDN_KEY 与
// CSDN_CREDENTIAL_FILE）。仅作为环境变量/配置文件未提供 Cookie 时的补充来源；失败返回 (nil, false)。
func loadEncryptedDefault() (*auth.Credential, bool) {
	key := os.Getenv("CSDN_KEY")
	path := os.Getenv("CSDN_CREDENTIAL_FILE")
	if key == "" || path == "" {
		return nil, false
	}
	cred, err := auth.RestoreDefault(path, key)
	if err != nil {
		log.Printf("WARN: 读取加密凭证文件 %s 失败: %v", path, err)
		return nil, false
	}
	return cred, true
}

func main() {
	cfg := loadConfig()

	store := auth.NewStore()
	if cfg.CSDN.DefaultCookie != "" {
		if err := store.LoadDefault(cfg.CSDN.DefaultCookie, cfg.CSDN.ExtraHeaders); err != nil {
			log.Printf("WARN: 默认凭证载入失败: %v", err)
		} else {
			log.Println("INFO: 已从配置/环境变量载入默认 CSDN 凭证（default 绑定）")
		}
	} else if cred, ok := loadEncryptedDefault(); ok {
		// 环境变量/配置未给 Cookie，但存在此前 bind_csdn(persist=true) 加密落盘的文件时，作为补充来源。
		if err := store.LoadDefault(cred.Cookie, cred.ExtraHeaders); err != nil {
			log.Printf("WARN: 加密凭证文件载入失败: %v", err)
		} else {
			log.Printf("INFO: 已从加密文件载入默认 CSDN 凭证（%s）", os.Getenv("CSDN_CREDENTIAL_FILE"))
		}
	} else {
		log.Println("WARN: 未提供 CSDN Cookie，写工具将不可用；可在连接后调用 bind_csdn 工具上传你自己的 Cookie")
	}

	client := csdn.NewClient(cfg.CSDN.PublishEndpoint, cfg.CSDN.UserAgent)

	s := server.NewMCPServer(cfg.Server.Name, cfg.Server.Version, server.WithToolCapabilities(true))
	tools.Register(s, store, client)

	// 本地固定以 stdio 传输运行：客户端（Claude Desktop / Cursor）直接拉起本进程并通过
	// 标准输入/输出交换 MCP 消息，无需任何网络监听，天然隔离。
	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("STDIO 服务异常: %v", err)
	}
}
