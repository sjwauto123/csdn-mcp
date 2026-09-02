package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnbind(t *testing.T) {
	s := NewStore()
	if err := s.Bind("default", "cookie-a", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind("work", "cookie-b", nil); err != nil {
		t.Fatal(err)
	}
	// 撤销存在的
	if err := s.Unbind("work"); err != nil {
		t.Fatalf("Unbind 应成功: %v", err)
	}
	if _, err := s.Get("work"); err == nil {
		t.Fatal("Unbind 后该 binding 应不存在")
	}
	// 撤销不存在的应报错（而非静默）
	if err := s.Unbind("nope"); err == nil {
		t.Fatal("Unbind 不存在的 binding 应报错")
	}
	// default 仍可用
	if _, err := s.Get(""); err != nil {
		t.Fatalf("default 应仍在: %v", err)
	}
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	plain := `{"cookie":"sessionid=abc123; token=xyz","extra_headers":null}`
	secret := "a-strong-master-key"
	enc, err := Encrypt(plain, secret)
	if err != nil {
		t.Fatalf("Encrypt 失败: %v", err)
	}
	if enc == plain {
		t.Fatal("密文不应等于明文")
	}
	dec, err := Decrypt(enc, secret)
	if err != nil {
		t.Fatalf("Decrypt 失败: %v", err)
	}
	if dec != plain {
		t.Fatalf("解密往返不一致:\n got=%q\nwant=%q", dec, plain)
	}
	// 错误密钥必须解密失败
	if _, err := Decrypt(enc, "wrong-key"); err == nil {
		t.Fatal("错误密钥应解密失败")
	}
}

func TestPersistRestoreDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cred.enc")
	secret := "master-key"
	cred := &Credential{Cookie: "sessionid=secret", ExtraHeaders: map[string]string{"X-Custom": "1"}}
	if err := PersistDefault(path, cred, secret); err != nil {
		t.Fatalf("PersistDefault 失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("加密文件未生成: %v", err)
	}
	got, err := RestoreDefault(path, secret)
	if err != nil {
		t.Fatalf("RestoreDefault 失败: %v", err)
	}
	if got.Cookie != cred.Cookie || got.ExtraHeaders["X-Custom"] != "1" {
		t.Fatalf("恢复出的凭证不一致: %+v", got)
	}
	// 错误密钥恢复失败
	if _, err := RestoreDefault(path, "bad"); err == nil {
		t.Fatal("错误密钥应恢复失败")
	}
}
