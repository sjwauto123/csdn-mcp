// Package auth 维护用户自己上传的 CSDN 登录凭证（自我委托模式）。
//
// 重要合规说明：
//   - 这里存储的是“用户本人的” Cookie，即用户自己登录后把凭证交给自己的工具使用，
//     不是代替/冒充他人，也不是批量爬取。属于个人自我委托，合规风险低。
//   - 凭证在内存中以明文存在；生产环境应加密落盘（见 README 的补强建议）。
//   - 切勿把凭证打印到日志；对外展示一律使用 Masked()。
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Credential 是用户自己上传的 CSDN 登录凭证。
// Cookie 为必填；ExtraHeaders 为可选，用于承载从浏览器复制的 x-ca-* 等签名头。
type Credential struct {
	Cookie       string            `json:"cookie"`
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
}

// Masked 返回脱敏后的 Cookie，用于日志展示，避免泄漏完整凭证。
func (c Credential) Masked() string {
	if len(c.Cookie) <= 8 {
		return "***"
	}
	return c.Cookie[:4] + "..." + c.Cookie[len(c.Cookie)-4:]
}

// Store 按 bindingID 维护多份用户凭证。
// 单用户场景下默认使用 "default" 这个 bindingID。
type Store struct {
	mu    sync.RWMutex
	creds map[string]*Credential
}

// NewStore 创建一个新的凭证存储。
func NewStore() *Store {
	return &Store{creds: make(map[string]*Credential)}
}

// LoadDefault 从配置/环境变量载入初始凭证到 "default" 绑定。
func (s *Store) LoadDefault(cookie string, extra map[string]string) error {
	if cookie == "" {
		return fmt.Errorf("cookie 为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds["default"] = &Credential{Cookie: cookie, ExtraHeaders: extra}
	return nil
}

// Bind 绑定/更新某个 bindingID 的凭证（用户自己上传自己的 Cookie）。
func (s *Store) Bind(id, cookie string, extra map[string]string) error {
	if id == "" {
		id = "default"
	}
	if cookie == "" {
		return fmt.Errorf("cookie 不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[id] = &Credential{Cookie: cookie, ExtraHeaders: extra}
	return nil
}

// Get 取出某个 bindingID 的凭证；id 为空时回退到 "default"。
func (s *Store) Get(id string) (*Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id == "" {
		id = "default"
	}
	c, ok := s.creds[id]
	if !ok {
		return nil, fmt.Errorf("未找到绑定ID=%q 的凭证，请先调用 bind_csdn 或配置 Cookie", id)
	}
	return c, nil
}

// List 返回所有已绑定的 ID（用于调试）。
func (s *Store) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.creds))
	for k := range s.creds {
		ids = append(ids, k)
	}
	return ids
}

// Unbind 撤销并清除指定 bindingID 的凭证（仅从内存移除）。
func (s *Store) Unbind(id string) error {
	if id == "" {
		id = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.creds[id]; !ok {
		return fmt.Errorf("未找到绑定ID=%q 的凭证，无需撤销", id)
	}
	delete(s.creds, id)
	return nil
}

// ---- 可选加密持久化（默认不启用）----
//
// 仅在同时设置环境变量 CSDN_KEY（主密钥，任意字符串）与 CSDN_CREDENTIAL_FILE（输出路径）时，
// bind_csdn 的 persist=true 才会把 default 凭证加密落盘（AES-GCM）。不设置则凭证仅存内存，
// 避免明文 Cookie 落在磁盘上。文件权限固定 0600。

func deriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// Encrypt 用 AES-GCM 加密明文，返回 base64(nonce || ciphertext)。
func Encrypt(plaintext, secret string) (string, error) {
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt 解密 Encrypt 的产出。
func Decrypt(b64, secret string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return "", fmt.Errorf("密文过短")
	}
	nonce, ct := data[:ns], data[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// PersistDefault 将凭证（仅 default 绑定）加密写入 path，文件权限 0600。
func PersistDefault(path string, c *Credential, secret string) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	enc, err := Encrypt(string(b), secret)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(enc), 0600)
}

// RestoreDefault 从 path 读取并解密 default 凭证（需与 PersistDefault 相同的 CSDN_KEY）。
func RestoreDefault(path, secret string) (*Credential, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pt, err := Decrypt(string(b), secret)
	if err != nil {
		return nil, err
	}
	var c Credential
	if err := json.Unmarshal([]byte(pt), &c); err != nil {
		return nil, err
	}
	return &c, nil
}
