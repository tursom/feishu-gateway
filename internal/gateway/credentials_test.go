package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"feishu-gateway/internal/feishu"
)

func noFallback(t *testing.T) {
	t.Helper()
	t.Setenv("FEISHU_APP_ID", "")
	t.Setenv("FEISHU_APP_SECRET", "")
	t.Setenv("FEISHU_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing.json"))
}
func TestCredentialsSaveKeepReplaceAndRestart(t *testing.T) {
	noFallback(t)
	dir := t.TempDir()
	m := newCredentialManager(filepath.Join(dir, "db.sqlite"), "")
	if m.Info().SecretConfigured {
		t.Fatal("unexpected configured credentials")
	}
	if _, e := m.Save(FeishuCredentialInput{AppID: "cli_first"}); e == nil {
		t.Fatal("first save without secret accepted")
	}
	info, e := m.Save(FeishuCredentialInput{AppID: "cli_first", AppSecret: "first-secret-never-echo"})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(info)
	if strings.Contains(string(b), "first-secret-never-echo") || !info.SecretConfigured || info.Source != "managed" {
		t.Fatal("unsafe info")
	}
	stat, e := os.Stat(filepath.Join(dir, "feishu-credentials.json"))
	if e != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("credential file is not private", e)
	}
	if _, e = m.Save(FeishuCredentialInput{AppID: "cli_first"}); e != nil {
		t.Fatal(e)
	}
	loaded, _ := m.Load()
	if loaded["app_secret"] != "first-secret-never-echo" {
		t.Fatal("empty secret erased previous secret")
	}
	if _, e = m.Save(FeishuCredentialInput{AppID: "cli_second"}); e == nil {
		t.Fatal("changed id reused secret")
	}
	loaded, _ = m.Load()
	if loaded["app_id"] != "cli_first" {
		t.Fatal("failed save changed settings")
	}
	if _, e = m.Save(FeishuCredentialInput{AppID: "cli_second", AppSecret: "replacement-secret"}); e != nil {
		t.Fatal(e)
	}
	restarted := newCredentialManager(filepath.Join(dir, "db.sqlite"), "")
	loaded, e = restarted.Load()
	if e != nil || loaded["app_id"] != "cli_second" || loaded["app_secret"] != "replacement-secret" {
		t.Fatal("save not persistent", e)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary credential files left behind")
	}
}
func TestManagedCredentialsOverrideLegacyWithoutModifyingIt(t *testing.T) {
	noFallback(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.json")
	original := []byte(`{"app_id":"cli_legacy","app_secret":"legacy-secret"}`)
	if e := os.WriteFile(legacy, original, 0400); e != nil {
		t.Fatal(e)
	}
	m := newCredentialManager(filepath.Join(dir, "data", "db.sqlite"), legacy)
	if info := m.Info(); info.Source != "file" || info.AppID != "cli_legacy" {
		t.Fatal("fallback file missing")
	}
	if _, e := m.Save(FeishuCredentialInput{AppID: "cli_legacy"}); e != nil {
		t.Fatal(e)
	}
	t.Setenv("FEISHU_APP_ID", "cli_env")
	t.Setenv("FEISHU_APP_SECRET", "env-secret")
	loaded, e := m.Load()
	if e != nil || loaded["app_id"] != "cli_legacy" {
		t.Fatal("managed settings not preferred")
	}
	unchanged, _ := os.ReadFile(legacy)
	if string(unchanged) != string(original) {
		t.Fatal("legacy secret overwritten")
	}
	if e = os.WriteFile(m.path, []byte("invalid"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Load(); e == nil {
		t.Fatal("corrupt managed credentials silently fell back")
	}
	if _, e = m.Save(FeishuCredentialInput{AppID: "cli_new", AppSecret: "new-secret"}); e != nil {
		t.Fatal("cannot replace corrupt managed credentials", e)
	}
}
func TestCredentialManagementRequiresAdminAndDoesNotLeak(t *testing.T) {
	noFallback(t)
	s, _ := newTestServer(t)
	secret := "sensitive-password-dont-log"
	body := map[string]any{"appId": "cli_console", "appSecret": secret}
	w := request(s, "POST", "/admin-api/feishu-credentials", "", body, false)
	if w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	w = request(s, "POST", "/admin-api/feishu-credentials", "", body, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) {
		t.Fatal("save failed or leaked")
	}
	w = request(s, "GET", "/admin-api/settings", "", nil, false)
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), "cli_console") {
		t.Fatal("settings exposes secret or lacks id")
	}
	logs, e := s.Store.Logs(50, 0, "all", "")
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(logs)
	if len(logs.Items) != 1 || strings.Contains(string(b), secret) {
		t.Fatal("credential audit leaks")
	}
	s.Config.Mode = "pangolin"
	s.Config.ProxySecret = strings.Repeat("p", 40)
	s.Config.TrustedProxyIPs = []string{"127.0.0.1"}
	token := app(t, s.Store, "tasks:write")
	w = request(s, "POST", "/admin-api/feishu-credentials", token.Token, body, true)
	if w.Code != 403 {
		t.Fatal("API token could save admin credentials")
	}
}
func TestSaveCredentialsRefreshesRealClientCache(t *testing.T) {
	noFallback(t)
	var authCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			authCalls.Add(1)
			var payload map[string]string
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Error("bad auth body")
			}
			if payload["app_secret"] != "secret-"+payload["app_id"] {
				t.Error("wrong saved secret used")
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "access-" + payload["app_id"], "expire": 7200})
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-cli_") {
			t.Error("missing auth")
		}
		if strings.Contains(r.URL.Path, "get_node") {
			io.WriteString(w, `{"code":0,"data":{"node":{"obj_type":"bitable","obj_token":"base"}}}`)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"items":[],"has_more":false}}`)
	}))
	defer upstream.Close()
	s, _ := newTestServer(t)
	client := feishu.NewClient()
	client.BaseURL = upstream.URL
	s = NewServer(s.Config, s.Store, client, testWeb)
	for _, id := range []string{"cli_first", "cli_second"} {
		w := request(s, "POST", "/admin-api/feishu-credentials", "", map[string]any{"appId": id, "appSecret": "secret-" + id}, true)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		for i := 0; i < 2; i++ {
			if _, e := client.Run(context.Background(), feishu.Params{Action: "fields", Table: "tasks"}); e != nil {
				t.Fatal(e)
			}
		}
	}
	if authCalls.Load() != 2 {
		t.Fatalf("expected one auth per saved configuration, got %d", authCalls.Load())
	}
}
