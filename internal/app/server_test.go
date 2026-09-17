package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"feishu-gateway/internal/feishu"
)

type mockUpstream struct {
	calls atomic.Int32
	fn    func(feishu.Credentials, string, string, url.Values, []byte) (feishu.Result, error)
}

func (m *mockUpstream) Do(_ context.Context, c feishu.Credentials, method, path string, q url.Values, b []byte) (feishu.Result, error) {
	m.calls.Add(1)
	if m.fn != nil {
		return m.fn(c, method, path, q, b)
	}
	return feishu.Result{Status: 200, Stage: "operation", RequestID: "upstream-id", Body: []byte(`{"code":0,"msg":"success","data":{"record":{"record_id":"rec1","fields":{"任务名称":"new","备注":"unrelated-value"}}}}`)}, nil
}
func setup(t *testing.T) (*Server, *mockUpstream) {
	t.Helper()
	store, e := Open(filepath.Join(t.TempDir(), "gateway.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	if e = store.SaveCredentials("cli_test", "secret-not-logged"); e != nil {
		t.Fatal(e)
	}
	up := &mockUpstream{}
	return New(store, up, fstest.MapFS{"index.html": {Data: []byte("console")}, "app.js": {Data: []byte("/* app */")}, "style.css": {Data: []byte("body{}")}}), up
}
func application(t *testing.T, s *Store, scopes ...string) Token {
	t.Helper()
	name := random("test")
	out, e := s.Create(AppInput{Name: &name, Scopes: &scopes})
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func req(s *Server, method, path, token string, body any, admin bool) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	r.RemoteAddr = "192.0.2.12:1234"
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if admin {
		r.Header.Set("X-Requested-With", "FeishuGateway")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestAdminReliesOnPangolinAndAPITokenIsSeparate(t *testing.T) {
	s, _ := setup(t)
	// The source has no second login or IP allowlist. Pangolin admission is external.
	if w := req(s, "GET", "/admin-api/settings", "", nil, false); w.Code != 200 {
		t.Fatal(w.Code)
	}
	body := map[string]any{"appId": "cli_new", "appSecret": "new-secret"}
	if w := req(s, "POST", "/admin-api/settings", "", body, false); w.Code != 403 {
		t.Fatal("cross-site write accepted")
	}
	if w := req(s, "POST", "/admin-api/settings", "", body, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := req(s, "GET", "/api/v1/tables", "", nil, true); w.Code != 401 {
		t.Fatal("admin header bypassed API auth")
	}
	a := application(t, s.store, "tasks:read")
	w := req(s, "GET", "/api/v1/tables", a.Token, nil, false)
	if w.Code != 200 || strings.Contains(w.Body.String(), "requirements") {
		t.Fatal("table scope listing")
	}
	w = req(s, "GET", "/admin-api/settings", "", nil, false)
	if strings.Contains(w.Body.String(), "new-secret") {
		t.Fatal("secret echoed")
	}
}
func TestPoliciesBeforeUpstream(t *testing.T) {
	s, up := setup(t)
	read := application(t, s.store, "tasks:read")
	write := application(t, s.store, "tasks:write", "requirements:status", "bugs:status")
	cases := []struct {
		table, method, token string
		fields               map[string]any
	}{{"tasks", "POST", read.Token, map[string]any{"任务名称": "x"}}, {"requirements", "POST", write.Token, map[string]any{"需求名称": "x"}}, {"requirements", "PATCH", write.Token, map[string]any{"需求名称": "x"}}, {"bugs", "PATCH", write.Token, map[string]any{"备注": "x"}}}
	for _, c := range cases {
		path := "/api/v1/tables/" + c.table + "/records"
		if c.method == "PATCH" {
			path += "/rec1"
		}
		w := req(s, c.method, path, c.token, map[string]any{"fields": c.fields}, false)
		if w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if up.calls.Load() != 0 {
		t.Fatal("rejected policy called upstream")
	}
	if w := req(s, "DELETE", "/api/v1/tables/tasks/records/rec1", write.Token, nil, false); w.Code != 405 {
		t.Fatal("delete allowed")
	}
}
func TestCreatesAreNotDeduplicatedAndUpdatesNeedNoRevision(t *testing.T) {
	s, up := setup(t)
	a := application(t, s.store, "tasks:write", "tasks:read")
	body := `{"fields":{"任务名称":"new"}}`
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/api/v1/tables/tasks/records", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+a.Token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "same-key")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		if strings.Contains(w.Body.String(), "verified") || strings.Contains(w.Body.String(), "revision") {
			t.Fatal("synthetic workflow result")
		}
	}
	if up.calls.Load() != 2 {
		t.Fatal("requests deduplicated or retried")
	}
	w := req(s, "PATCH", "/api/v1/tables/tasks/records/rec1", a.Token, map[string]any{"fields": map[string]any{"任务名称": "new"}}, false)
	if w.Code != 200 || up.calls.Load() != 3 {
		t.Fatal("update required revision or extra request")
	}
	var count int
	if e := s.store.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='idempotency'").Scan(&count); e != nil || count != 0 {
		t.Fatal("idempotency table created", e)
	}
}
func TestUpstreamResponseAndTransportErrorAreTransparent(t *testing.T) {
	s, up := setup(t)
	a := application(t, s.store, "tasks:write", "tasks:read")
	payload := map[string]any{"fields": map[string]any{"任务名称": "x"}}
	rejected := `{"code":1254030,"msg":"Permission denied","data":{}}`
	up.fn = func(feishu.Credentials, string, string, url.Values, []byte) (feishu.Result, error) {
		return feishu.Result{Status: 200, Stage: "operation", Body: []byte(rejected), RequestID: "feishu-trace"}, nil
	}
	w := req(s, "POST", "/api/v1/tables/tasks/records", a.Token, payload, false)
	if w.Code != 200 || w.Body.String() != rejected || w.Header().Get("X-Feishu-Request-ID") != "feishu-trace" {
		t.Fatal("business response replaced")
	}
	up.fn = func(feishu.Credentials, string, string, url.Values, []byte) (feishu.Result, error) {
		return feishu.Result{}, &feishu.TransportError{Stage: "operation", Message: "未收到飞书响应", ResponseReceived: false}
	}
	w = req(s, "POST", "/api/v1/tables/tasks/records", a.Token, payload, false)
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"responseReceived":false`) {
		t.Fatal(w.Body.String())
	}
	if up.calls.Load() != 2 {
		t.Fatal("unexpected retries")
	}
	up.fn = func(feishu.Credentials, string, string, url.Values, []byte) (feishu.Result, error) {
		return feishu.Result{Status: 503, Stage: "operation", Body: []byte("upstream maintenance")}, nil
	}
	w = req(s, "POST", "/api/v1/tables/tasks/records", a.Token, payload, false)
	if w.Code != 503 || w.Body.String() != "upstream maintenance" {
		t.Fatal("raw HTTP result masked")
	}
}
func TestWriteOnlyResponsePreservesStatusAndSubmittedFields(t *testing.T) {
	s, _ := setup(t)
	a := application(t, s.store, "tasks:write")
	w := req(s, "POST", "/api/v1/tables/tasks/records", a.Token, map[string]any{"fields": map[string]any{"任务名称": "new"}}, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":0`) || !strings.Contains(w.Body.String(), `"任务名称":"new"`) || strings.Contains(w.Body.String(), "unrelated-value") {
		t.Fatal("write scope leaked data or hid status")
	}
	if w.Header().Get("X-Response-Fields") != "submitted-only" {
		t.Fatal("filtering not disclosed")
	}
}
func TestNativeSearchAndDebug(t *testing.T) {
	s, up := setup(t)
	a := application(t, s.store, "bugs:read")
	up.fn = func(c feishu.Credentials, method, path string, q url.Values, b []byte) (feishu.Result, error) {
		if c.AppID != "cli_test" || method != "POST" || !strings.HasSuffix(path, "/records/search") || q.Get("page_size") != "7" || q.Get("page_token") != "next" {
			t.Error("native params lost")
		}
		if !strings.Contains(string(b), "filter") {
			t.Error("filter lost")
		}
		return feishu.Result{Status: 200, Stage: "operation", Body: []byte(`{"code":0,"data":{"items":[],"has_more":false}}`)}, nil
	}
	w := req(s, "POST", "/admin-api/debug", "", map[string]any{"appId": a.App.ID, "table": "bugs", "action": "search", "pageSize": 7, "pageToken": "next", "payload": map[string]any{"filter": map[string]any{"conjunction": "and", "conditions": []any{}}}}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"httpStatus":200`) {
		t.Fatal(w.Body.String())
	}
	logs, e := s.store.Logs()
	if e != nil || len(logs) != 1 || logs[0].Status != 200 {
		t.Fatal(e, logs)
	}
	b, _ := json.Marshal(logs)
	if strings.Contains(string(b), "secret-not-logged") {
		t.Fatal("secret logged")
	}
}
