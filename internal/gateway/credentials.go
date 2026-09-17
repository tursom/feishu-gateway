package gateway

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"feishu-gateway/internal/feishu"
)

type FeishuCredentialInfo struct {
	AppID            string `json:"appId"`
	SecretConfigured bool   `json:"secretConfigured"`
	Source           string `json:"source"`
}
type FeishuCredentialInput struct {
	AppID     string `json:"appId"`
	AppSecret string `json:"appSecret,omitempty"`
}
type credentialPair struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// Managed credentials are kept outside the public web root, next to SQLite.
// Environment and the legacy read-only file are fallbacks, never overwritten.
type CredentialManager struct {
	mu           sync.Mutex
	path         string
	fallbackPath string
}

func newCredentialManager(databasePath, fallback string) *CredentialManager {
	path := ""
	if databasePath != "" && databasePath != ":memory:" {
		path = filepath.Join(filepath.Dir(databasePath), "feishu-credentials.json")
	}
	return &CredentialManager{path: path, fallbackPath: fallback}
}
func credentialConfigError() error {
	return &feishu.Error{Status: 500, Code: "CONFIG_ERROR", Message: "飞书凭据不可用，请在管理后台的服务设置中配置 App ID 和 App Secret"}
}
func (m *CredentialManager) loadLocked() (credentialPair, string, error) {
	if m.path != "" {
		b, e := os.ReadFile(m.path)
		if e == nil {
			var pair credentialPair
			if json.Unmarshal(b, &pair) != nil || pair.AppID == "" || pair.AppSecret == "" {
				return credentialPair{}, "managed", credentialConfigError()
			}
			return pair, "managed", nil
		}
		if !errors.Is(e, os.ErrNotExist) {
			return credentialPair{}, "managed", credentialConfigError()
		}
	}
	raw, e := feishu.LoadCredentials(m.fallbackPath)
	if e != nil {
		return credentialPair{}, "unconfigured", credentialConfigError()
	}
	pair := credentialPair{}
	pair.AppID, _ = raw["app_id"].(string)
	pair.AppSecret, _ = raw["app_secret"].(string)
	source := "file"
	if os.Getenv("FEISHU_APP_ID") != "" || os.Getenv("FEISHU_APP_SECRET") != "" {
		source = "environment"
	}
	return pair, source, nil
}
func (m *CredentialManager) Load() (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pair, _, e := m.loadLocked()
	if e != nil {
		return nil, e
	}
	return map[string]any{"app_id": pair.AppID, "app_secret": pair.AppSecret}, nil
}
func (m *CredentialManager) Info() FeishuCredentialInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	pair, source, e := m.loadLocked()
	return FeishuCredentialInfo{AppID: pair.AppID, SecretConfigured: e == nil, Source: source}
}

var appIDPattern = regexp.MustCompile(`^cli_[a-zA-Z0-9]+$`)

func (m *CredentialManager) Save(input FeishuCredentialInput) (FeishuCredentialInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := strings.TrimSpace(input.AppID)
	secret := strings.TrimSpace(input.AppSecret)
	if !appIDPattern.MatchString(id) || len(id) > 128 {
		return FeishuCredentialInfo{}, fail(400, "INVALID_APP_ID", "请填写以 cli_ 开头的有效飞书 App ID")
	}
	if len(secret) > 512 || strings.ContainsAny(secret, "\r\n\x00") {
		return FeishuCredentialInfo{}, fail(400, "INVALID_APP_SECRET", "App Secret 格式无效")
	}
	if secret == "" {
		existing, _, err := m.loadLocked()
		if err != nil || existing.AppID != id {
			return FeishuCredentialInfo{}, fail(400, "APP_SECRET_REQUIRED", "首次配置或更换 App ID 时必须填写 App Secret")
		}
		secret = existing.AppSecret
	}
	if m.path == "" {
		return FeishuCredentialInfo{}, fail(500, "CREDENTIAL_SAVE_FAILED", "未配置持久数据目录，无法保存飞书凭据")
	}
	if e := os.MkdirAll(filepath.Dir(m.path), 0700); e != nil {
		return FeishuCredentialInfo{}, fail(500, "CREDENTIAL_SAVE_FAILED", "无法保存飞书凭据，请检查服务数据目录权限")
	}
	// Temp file and atomic rename prevent partial credentials after interruption.
	file, e := os.CreateTemp(filepath.Dir(m.path), ".feishu-credentials-*")
	if e != nil {
		return FeishuCredentialInfo{}, fail(500, "CREDENTIAL_SAVE_FAILED", "无法保存飞书凭据，请检查服务数据目录权限")
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	encoded, _ := json.Marshal(credentialPair{AppID: id, AppSecret: secret})
	if _, e = file.Write(encoded); e == nil {
		e = file.Sync()
	}
	closeErr := file.Close()
	if e == nil {
		e = closeErr
	}
	if e == nil {
		e = os.Rename(tmp, m.path)
	}
	if e != nil {
		return FeishuCredentialInfo{}, fail(500, "CREDENTIAL_SAVE_FAILED", "保存飞书凭据失败，原配置未被替换")
	}
	// Best-effort directory sync strengthens rename durability on local disks.
	if dir, err := os.Open(filepath.Dir(m.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return FeishuCredentialInfo{AppID: id, SecretConfigured: true, Source: "managed"}, nil
}
