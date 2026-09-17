package gateway

import (
	"context"
	"encoding/json"
	"feishu-gateway/internal/feishu"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

type fakeClient struct {
	calls atomic.Int32
	run   func(context.Context, feishu.Params) (any, error)
}

func (f *fakeClient) Run(ctx context.Context, p feishu.Params) (any, error) {
	f.calls.Add(1)
	if f.run != nil {
		return f.run(ctx, p)
	}
	return map[string]any{"record_id": "rec123", "fields": p.Fields, "verified": true, "revision": strings.Repeat("a", 64), "items": []any{}, "has_more": false}, nil
}

var testWeb fs.FS = fstest.MapFS{"index.html": {Data: []byte("<html>console</html>")}, "app.js": {Data: []byte("console.log('ok')")}, "style.css": {Data: []byte("body{}")}}

func newTestServer(t *testing.T) (*Server, *fakeClient) {
	t.Helper()
	s, e := OpenStore(filepath.Join(t.TempDir(), "gateway.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	f := &fakeClient{}
	cfg := Config{Mode: "local", Host: "127.0.0.1", Port: 8787, PublicOrigin: "http://127.0.0.1:8787", RateLimit: 60}
	return NewServer(cfg, s, f, testWeb), f
}
func request(s *Server, method, path, token string, body any, admin bool) *httptest.ResponseRecorder {
	var raw string
	if body != nil {
		b, _ := json.Marshal(body)
		raw = string(b)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(raw))
	r.Host = "127.0.0.1:8787"
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if admin {
		r.Header.Set("Origin", s.Config.PublicOrigin)
		r.Header.Set("X-CSRF-Token", s.csrf)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func app(t *testing.T, s *Store, scopes ...string) TokenResult {
	t.Helper()
	name := randomID("test-")
	v, e := s.CreateApp(AppInput{Name: &name, Scopes: &scopes})
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func decodeResponse(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var r map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &r); e != nil {
		t.Fatalf("decode: %v: %s", e, w.Body.String())
	}
	return r
}
func TestPangolinBoundaryAndCSRF(t *testing.T) {
	s, _ := newTestServer(t)
	s.Config.Mode = "pangolin"
	s.Config.PublicOrigin = "https://gateway.example.com"
	s.Config.ProxySecret = strings.Repeat("k", 40)
	s.Config.TrustedProxyIPs = []string{"127.0.0.1"}
	a := app(t, s.Store, "tasks:read")
	for _, path := range []string{"/", "/assets/app.js", "/admin-api/session", "/admin-api/apps"} {
		w := request(s, "GET", path, a.Token, nil, false)
		if w.Code != 403 {
			t.Fatalf("API token reached admin %s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/admin-api/session", nil)
	r.Host = "gateway.example.com"
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Gateway-Secret", s.Config.ProxySecret)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("trusted proxy rejected: %s", w.Body.String())
	}
	r.RemoteAddr = "192.0.2.1:1"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("forged remote allowed")
	}
	r = httptest.NewRequest("GET", "/api/v1/tables", nil)
	r.Host = "gateway.example.com"
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Gateway-Secret", s.Config.ProxySecret)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("proxy auth bypassed API token")
	}
	s.Config.Mode = "local"
	s.Config.PublicOrigin = "http://127.0.0.1:8787"
	w = request(s, "POST", "/admin-api/apps", "", map[string]any{"name": "csrf-test", "scopes": []string{"tasks:read"}}, false)
	if w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	w = request(s, "POST", "/admin-api/apps", "", map[string]any{"name": "csrf-test", "scopes": []string{"tasks:read"}}, true)
	if w.Code != 201 {
		t.Fatalf("valid csrf failed: %s", w.Body.String())
	}
	r = httptest.NewRequest("GET", "/admin-api/session", nil)
	r.Host = "evil.test"
	r.RemoteAddr = "127.0.0.1:1"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("host rebinding accepted")
	}
}
func TestTokenHashRotationExpiryAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	v := app(t, s, "tasks:read")
	var stored string
	s.DB.QueryRow("SELECT token_hash FROM apps WHERE id=?", v.App.ID).Scan(&stored)
	if stored == v.Token || stored != digest(v.Token) {
		t.Fatal("token not hashed")
	}
	list, _ := s.ListApps()
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), v.Token) {
		t.Fatal("token exposed in list")
	}
	s.Close()
	s, e = OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Authenticate(v.Token); e != nil {
		t.Fatal(e)
	}
	next, e := s.Rotate(v.App.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(v.Token); e == nil {
		t.Fatal("old token still active")
	}
	if _, e = s.Authenticate(next.Token); e != nil {
		t.Fatal(e)
	}
	disabled := false
	s.UpdateApp(v.App.ID, AppInput{Enabled: &disabled})
	if _, e = s.Authenticate(next.Token); e == nil {
		t.Fatal("disabled token allowed")
	}
	enabled := true
	s.UpdateApp(v.App.ID, AppInput{Enabled: &enabled})
	s.DB.Exec("UPDATE apps SET expires_at=? WHERE id=?", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), v.App.ID)
	if _, e = s.Authenticate(next.Token); e == nil {
		t.Fatal("expired token allowed")
	}
}
func TestPermissionsAndPlatformLimits(t *testing.T) {
	s, f := newTestServer(t)
	ro := app(t, s.Store, "tasks:read")
	w := request(s, "PATCH", "/api/v1/tables/tasks/records/rec123", ro.Token, map[string]any{"fields": map[string]any{"备注": "new"}, "expectedRevision": strings.Repeat("a", 64)}, false)
	if w.Code != 403 || f.calls.Load() != 0 {
		t.Fatal("read only write reached client")
	}
	rw := app(t, s.Store, "tasks:write", "requirements:status", "bugs:status", "bugs:edit")
	for _, table := range []string{"tasks", "requirements"} {
		field := "任务状态"
		if table == "requirements" {
			field = "需求状态"
		}
		w = request(s, "PATCH", "/api/v1/tables/"+table+"/records/rec123", rw.Token, map[string]any{"fields": map[string]any{field: "开发中"}, "expectedRevision": strings.Repeat("a", 64)}, false)
		if w.Code != 422 {
			t.Fatalf("flow field %s accepted: %s", field, w.Body.String())
		}
	}
	w = request(s, "PATCH", "/api/v1/tables/bugs/records/rec123", rw.Token, map[string]any{"fields": map[string]any{"备注": "new"}, "expectedRevision": strings.Repeat("a", 64)}, false)
	if w.Code != 400 {
		t.Fatal("bug special edit without reason accepted")
	}
	w = request(s, "DELETE", "/api/v1/tables/tasks/records/rec123", rw.Token, nil, false)
	if w.Code != 405 {
		t.Fatal("delete allowed")
	}
	w = request(s, "GET", "/api/v1/tables", ro.Token, nil, false)
	data := decodeResponse(t, w)["data"].(map[string]any)
	tables := data["tables"].(map[string]any)
	if len(tables) != 1 || tables["tasks"] == nil {
		t.Fatal("tables not filtered")
	}
}
func createRequest(s *Server, token, key, title string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"fields":{"任务名称":%q}}`, title)
	r := httptest.NewRequest("POST", "/api/v1/tables/tasks/records", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", key)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestIdempotencyReplayConflictAndRevocation(t *testing.T) {
	s, f := newTestServer(t)
	v := app(t, s.Store, "tasks:write")
	w := createRequest(s, v.Token, "unique-key-01", "new")
	if w.Code != 201 {
		t.Fatalf("create failed: %s", w.Body.String())
	}
	w = createRequest(s, v.Token, "unique-key-01", "new")
	if w.Code != 201 || f.calls.Load() != 1 {
		t.Fatal("retry duplicated create")
	}
	w = createRequest(s, v.Token, "unique-key-01", "changed")
	if w.Code != 409 {
		t.Fatal("key reused for different request")
	}
	disabled := false
	s.Store.UpdateApp(v.App.ID, AppInput{Enabled: &disabled})
	w = createRequest(s, v.Token, "unique-key-01", "new")
	if w.Code != 403 {
		t.Fatal("replay bypassed revocation")
	}
}
func TestConcurrentIdempotencyAndUnknownResult(t *testing.T) {
	s, f := newTestServer(t)
	v := app(t, s.Store, "tasks:write")
	entered, release := make(chan struct{}), make(chan struct{})
	f.run = func(ctx context.Context, p feishu.Params) (any, error) {
		close(entered)
		<-release
		return nil, &feishu.Error{Status: 502, Code: "WRITE_UNCERTAIN", Message: "写入结果待核实", WriteOutcome: "unknown"}
	}
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- createRequest(s, v.Token, "concurrent-key", "x") }()
	<-entered
	second := createRequest(s, v.Token, "concurrent-key", "x")
	if second.Code != 409 {
		t.Fatal("in-flight request not blocked")
	}
	close(release)
	first := <-done
	if first.Code != 502 {
		t.Fatal("unknown not surfaced")
	}
	again := createRequest(s, v.Token, "concurrent-key", "x")
	if again.Code != 502 || f.calls.Load() != 1 {
		t.Fatal("unknown create retried")
	}
}
func TestRateLimitAndAuditNoSecrets(t *testing.T) {
	s, _ := newTestServer(t)
	s.Config.RateLimit = 2
	v := app(t, s.Store, "tasks:read")
	for i := 0; i < 3; i++ {
		w := request(s, "GET", "/api/v1/tables", v.Token, nil, false)
		want := 200
		if i == 2 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("rate got %d want %d", w.Code, want)
		}
	}
	logs, e := s.Store.Logs(50, 0, "all", "")
	if e != nil || logs.Total != 3 {
		t.Fatal(e, logs.Total)
	}
	b, _ := json.Marshal(logs)
	if strings.Contains(string(b), v.Token) {
		t.Fatal("audit leaked token")
	}
	overview, e := s.Store.Overview()
	if e != nil || overview["requestsToday"] != 3 {
		t.Fatal("overview counts incorrect", overview, e)
	}
}
func TestDebugUsesLiveAppPermissionsAndAudits(t *testing.T) {
	s, f := newTestServer(t)
	v := app(t, s.Store, "tasks:read")
	body := map[string]any{"appId": v.App.ID, "action": "create", "table": "tasks", "fields": map[string]any{"任务名称": "x"}, "idempotencyKey": "debug-key"}
	w := request(s, "POST", "/admin-api/debug", "", body, true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	r := decodeResponse(t, w)
	if r["data"].(map[string]any)["status"] != float64(403) || f.calls.Load() != 0 {
		t.Fatal("debug bypassed scopes")
	}
	logs, _ := s.Store.Logs(10, 0, "failure", "")
	if logs.Total != 1 {
		t.Fatal("debug not audited")
	}
}
func TestBodyLimitsAndUnknownFields(t *testing.T) {
	s, _ := newTestServer(t)
	w := request(s, "POST", "/admin-api/apps", "", map[string]any{"name": "x", "scopes": []string{"tasks:read"}, "token": "injected"}, true)
	if w.Code != 400 {
		t.Fatal("unknown fields accepted")
	}
	w = request(s, "POST", "/admin-api/apps", "", map[string]any{"name": "x", "description": strings.Repeat("a", 300000), "scopes": []string{"tasks:read"}}, true)
	if w.Code != 413 {
		t.Fatalf("oversized body got %d", w.Code)
	}
}
func TestStoreConcurrentCreation(t *testing.T) {
	s, _ := newTestServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprint("client-", i)
			scopes := []string{"bugs:read"}
			if _, e := s.Store.CreateApp(AppInput{Name: &name, Scopes: &scopes}); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	apps, e := s.Store.ListApps()
	if e != nil || len(apps) != 8 {
		t.Fatal(e, len(apps))
	}
}
func TestConfigFailsClosed(t *testing.T) {
	t.Setenv("AUTH_MODE", "pangolin")
	t.Setenv("PUBLIC_ORIGIN", "https://gateway.example.com")
	t.Setenv("PANGOLIN_PROXY_SECRET", "")
	if _, e := LoadConfig(); e == nil {
		t.Fatal("missing proxy secret allowed")
	}
	t.Setenv("AUTH_MODE", "local")
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("PUBLIC_ORIGIN", "http://127.0.0.1:8787")
	if _, e := LoadConfig(); e == nil {
		t.Fatal("local mode exposed network")
	}
}

func TestWriteResponseAndHistoricalReplayDoNotGrantRead(t *testing.T) {
	s, f := newTestServer(t)
	v := app(t, s.Store, "tasks:read", "tasks:write")
	f.run = func(context.Context, feishu.Params) (any, error) {
		return map[string]any{"record": map[string]any{"record_id": "rec123", "revision": strings.Repeat("a", 64), "fields": map[string]any{"备注": "PRIVATE-UNSUBMITTED-CONTENT"}}, "verified": true}, nil
	}
	w := createRequest(s, v.Token, "first-key-01", "x")
	if w.Code != 201 || strings.Contains(w.Body.String(), "PRIVATE-UNSUBMITTED") {
		t.Fatal("write response exposed record", w.Code)
	}
	p := feishu.Params{Action: "create", Table: "tasks", Fields: map[string]any{"任务名称": "legacy"}}
	s.Store.BeginIdempotency(v.App.ID, "legacy-key-01", digest(string(jsonBytes(p))))
	s.Store.FinishIdempotency(v.App.ID, "legacy-key-01", 201, jsonBytes(map[string]any{"data": map[string]any{"record": map[string]any{"record_id": "rec123", "revision": strings.Repeat("a", 64), "fields": map[string]any{"备注": "PRIVATE-LEGACY-CONTENT"}}, "verified": true}}))
	reduced := []string{"tasks:write"}
	s.Store.UpdateApp(v.App.ID, AppInput{Scopes: &reduced})
	w = createRequest(s, v.Token, "legacy-key-01", "legacy")
	if w.Code != 201 || strings.Contains(w.Body.String(), "PRIVATE-LEGACY") {
		t.Fatal("replay exposed revoked read data", w.Code)
	}
	if f.calls.Load() != 1 {
		t.Fatal("replay repeated upstream")
	}
}
func TestRejectedRequestsAreBudgetedAndAuditIsBounded(t *testing.T) {
	s, _ := newTestServer(t)
	s.Config.RateLimit = 2
	v := app(t, s.Store, "tasks:read")
	for i := 0; i < 3; i++ {
		w := request(s, "POST", "/api/v1/tables/tasks/records/search", v.Token, map[string]any{"reason": strings.Repeat("x", 240000)}, false)
		expected := 400
		if i == 2 {
			expected = 429
		}
		if w.Code != expected {
			t.Fatalf("rejection bypassed rate budget: %d", w.Code)
		}
	}
	logs, e := s.Store.Logs(20, 0, "all", "")
	if e != nil || logs.Total != 3 {
		t.Fatal(e, logs.Total)
	}
	for _, l := range logs.Items {
		if len(l.Reason) > 1024 {
			t.Fatal("unbounded reason in audit")
		}
	}
	s.entryMu.Lock()
	s.entryMinute = time.Now().Unix() / 60
	s.entryCount = 600
	s.entryMu.Unlock()
	w := request(s, "GET", "/api/v1/tables", "invalid", nil, false)
	if w.Code != 429 {
		t.Fatal("entry rejection not limited")
	}
	after, _ := s.Store.Logs(20, 0, "all", "")
	if after.Total != logs.Total {
		t.Fatal("over-budget request still grew audit")
	}
	s.Store.BeginIdempotency(v.App.ID, "pending-key-01", "fp")
	if e = s.Store.PurgeAudit(2); e != nil {
		t.Fatal(e)
	}
	after, _ = s.Store.Logs(20, 0, "all", "")
	if after.Total != 2 {
		t.Fatal("audit retention not applied")
	}
	if _, e = s.Store.BeginIdempotency(v.App.ID, "pending-key-01", "fp"); e == nil {
		t.Fatal("retention removed pending key")
	}
}
