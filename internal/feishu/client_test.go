package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setup(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	t.Setenv("FEISHU_APP_ID", "test-app")
	t.Setenv("FEISHU_APP_SECRET", "secret-never-return")
	t.Setenv("FEISHU_LOCK_DIR", t.TempDir())
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := NewClient()
	c.BaseURL = server.URL
	return c
}
func respond(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
}
func prelude(w http.ResponseWriter, r *http.Request) bool {
	if strings.Contains(r.URL.Path, "tenant_access_token") {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "token-never-return", "expire": 7200})
		return true
	}
	if strings.Contains(r.URL.Path, "get_node") {
		respond(w, map[string]any{"node": map[string]any{"obj_type": "bitable", "obj_token": "app-token"}})
		return true
	}
	return false
}
func errorCode(t *testing.T, err error, status int, code string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected Error, got %v", err)
	}
	if e.Status != status || e.Code != code {
		t.Fatalf("unexpected error: %+v", e)
	}
	return e
}
func schemaField(name string, typ int) any { return map[string]any{"field_name": name, "type": typ} }
func TestPolicyBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	c := setup(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); t.Error("policy made network request") })
	rev := strings.Repeat("0", 64)
	cases := []Params{
		{Action: "get", Table: "outside", RecordID: "recA"},
		{Action: "create", Table: "requirements", Fields: map[string]any{"需求状态": "x"}},
		{Action: "update", Table: "requirements", RecordID: "recA", ExpectedRevision: rev, Fields: map[string]any{"名称": "x"}},
		{Action: "update", Table: "bugs", RecordID: "recA", ExpectedRevision: rev, Fields: map[string]any{"名称": "x"}},
	}
	for _, p := range cases {
		_, err := c.Run(context.Background(), p)
		errorCode(t, err, 403, "POLICY_DENIED")
	}
	if _, err := c.Run(context.Background(), Params{Action: "tables"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("network used")
	}
}
func TestReadOnlyAndFormats(t *testing.T) {
	for _, typ := range []int{19, 20, 24, 1001, 1002, 1003, 1004, 1005, 3001} {
		for _, v := range []any{nil, "x"} {
			errorCode(t, validateFields(map[string]any{"field": v}, []any{schemaField("field", typ)}), 422, "FIELD_READ_ONLY")
		}
	}
	selectField := map[string]any{"field_name": "status", "type": 3, "property": map[string]any{"options": []any{map[string]any{"name": "Open"}}}}
	if err := validateFields(map[string]any{"status": "Open"}, []any{selectField}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{"Invented", []string{"Open"}, 12} {
		errorCode(t, validateFields(map[string]any{"status": v}, []any{selectField}), 422, "INVALID_FIELD_VALUE")
	}
	for _, tc := range []struct {
		typ   int
		value any
	}{{1, 17}, {2, "12"}, {5, 1.5}, {7, "true"}, {11, []any{map[string]any{"id": "user-name"}}}, {18, []string{"unknown"}}, {17, []any{map[string]any{"name": "file"}}}, {15, "https://example.com"}} {
		errorCode(t, validateFields(map[string]any{"f": tc.value}, []any{schemaField("f", tc.typ)}), 422, "INVALID_FIELD_VALUE")
	}
	errorCode(t, validateFields(map[string]any{"missing": "x"}, nil), 422, "UNKNOWN_FIELD")
}
func TestUpdateAndVerification(t *testing.T) {
	for _, mode := range []string{"conflict", "verified", "mismatch", "verification-failed", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			writes, gets, pages := 0, 0, 0
			before := map[string]any{"title": "old", "untouched": "keep"}
			c := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if prelude(w, r) {
					return
				}
				if r.Header.Get("Authorization") != "Bearer token-never-return" {
					t.Error("missing auth")
				}
				if strings.HasSuffix(r.URL.Path, "/fields") {
					pages++
					if r.URL.Query().Get("page_token") == "" {
						respond(w, map[string]any{"items": []any{schemaField("untouched", 1)}, "has_more": true, "page_token": "next"})
					} else {
						respond(w, map[string]any{"items": []any{schemaField("title", 1)}, "has_more": false})
					}
					return
				}
				if r.Method == "PUT" {
					writes++
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if string(canonical(body)) != `{"fields":{"title":"new"}}` {
						t.Errorf("unexpected mutation body %s", canonical(body))
					}
					if mode == "uncertain" {
						w.WriteHeader(502)
						_, _ = io.WriteString(w, "secret-never-return token-never-return")
						return
					}
					respond(w, map[string]any{"record": map[string]any{"record_id": "recA"}})
					return
				}
				gets++
				fields := before
				if gets > 1 {
					if mode == "verification-failed" {
						w.WriteHeader(500)
						_, _ = io.WriteString(w, "token-never-return")
						return
					}
					fields = map[string]any{"title": []any{map[string]any{"text": "new"}}, "untouched": "keep"}
					if mode == "mismatch" {
						fields["title"] = "other"
					}
				}
				respond(w, map[string]any{"record": map[string]any{"record_id": "recA", "fields": fields}})
			})
			rev := revision(before)
			if mode == "conflict" {
				rev = strings.Repeat("0", 64)
			}
			result, err := c.Run(context.Background(), Params{Action: "update", Table: "tasks", RecordID: "recA", ExpectedRevision: rev, Fields: map[string]any{"title": "new"}})
			switch mode {
			case "conflict":
				errorCode(t, err, 409, "REVISION_CONFLICT")
				if writes != 0 {
					t.Fatal("conflict wrote")
				}
			case "verification-failed":
				e := errorCode(t, err, 502, "VERIFICATION_FAILED")
				if e.WriteOutcome != "committed" || e.RecordID != "recA" {
					t.Fatalf("%+v", e)
				}
			case "uncertain":
				e := errorCode(t, err, 502, "WRITE_UNCERTAIN")
				if e.WriteOutcome != "unknown" || gets != 1 {
					t.Fatalf("%+v gets=%d", e, gets)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				m := obj(result)
				if m["verified"] != (mode == "verified") {
					t.Fatalf("%v", m)
				}
				if mode == "mismatch" && len(m["mismatchedFields"].([]string)) != 1 {
					t.Fatal(m)
				}
			}
			if mode != "conflict" && writes != 1 {
				t.Fatalf("writes=%d", writes)
			}
			if pages != 2 {
				t.Fatalf("pages=%d", pages)
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("FEISHU_LOCK_DIR"), allowedTables["tasks"].ID+".lock")); !os.IsNotExist(err) {
				t.Fatalf("lock not released: %v", err)
			}
		})
	}
}
func TestTokenSafetyAndRedirect(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"redirect", "body"} {
		t.Run(mode, func(t *testing.T) {
			c := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if prelude(w, r) {
					return
				}
				if mode == "redirect" {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					return
				}
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"code":123,"msg":"secret-never-return token-never-return /private/credentials.json"}`)
			})
			_, err := c.Run(context.Background(), Params{Action: "get", Table: "bugs", RecordID: "recA"})
			if err == nil {
				t.Fatal("expected failure")
			}
			b, _ := json.Marshal(err)
			for _, s := range []string{"secret-never-return", "token-never-return", "/private/credentials.json"} {
				if strings.Contains(string(b), s) {
					t.Fatal("sensitive error leaked")
				}
			}
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("followed redirect")
	}
}
func TestCredentialsErrorSafe(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "")
	t.Setenv("FEISHU_APP_SECRET", "")
	path := filepath.Join(t.TempDir(), "private-credential-file")
	t.Setenv("FEISHU_CREDENTIALS_FILE", path)
	_, err := NewClient().Run(context.Background(), Params{Action: "fields", Table: "tasks"})
	errorCode(t, err, 500, "CONFIG_ERROR")
	if strings.Contains(err.Error(), path) {
		t.Fatal("path leaked")
	}
}
func TestConcurrentTokenCache(t *testing.T) {
	var auth atomic.Int32
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			auth.Add(1)
		}
		if prelude(w, r) {
			return
		}
		respond(w, map[string]any{"items": []any{}, "has_more": false})
	})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Run(context.Background(), Params{Action: "fields", Table: "tasks"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if auth.Load() != 1 {
		t.Fatalf("auth requests=%d", auth.Load())
	}
}
func TestLockContextAndPreservation(t *testing.T) {
	t.Setenv("FEISHU_LOCK_DIR", t.TempDir())
	lock := filepath.Join(os.Getenv("FEISHU_LOCK_DIR"), allowedTables["tasks"].ID+".lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := NewClient().Run(ctx, Params{Action: "create", Table: "tasks", Fields: map[string]any{"title": "x"}})
	errorCode(t, err, 408, "REQUEST_CANCELED")
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("removed someone else's lock")
	}
}
func TestCanonicalAndFieldEquality(t *testing.T) {
	if revision(map[string]any{"n": json.Number("1e0")}) != revision(map[string]any{"n": 1}) {
		t.Fatal("revision depends on number spelling")
	}
	if revision(map[string]any{"b": 2, "a": map[string]any{"z": 1, "x": 2}}) != revision(map[string]any{"a": map[string]any{"x": 2, "z": 1}, "b": 2}) {
		t.Fatal("revision depends on order")
	}
	for _, tc := range []struct {
		sent, got any
		typ       int
	}{{nil, []any{}, 1}, {"hello", []any{map[string]any{"text": "hello"}}, 1}, {[]string{"B", "A"}, []any{map[string]any{"name": "A"}, map[string]any{"name": "B"}}, 4}, {[]any{map[string]any{"id": "ou_a"}}, []any{map[string]any{"id": "ou_a", "name": "Person"}}, 11}} {
		if !fieldEqual(tc.sent, tc.got, tc.typ) {
			t.Fatalf("not equal: %+v", tc)
		}
	}
}
func TestCreateAndSearch(t *testing.T) {
	writes := 0
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if prelude(w, r) {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/search") {
			if r.URL.Query().Get("page_token") != "next" || r.URL.Query().Get("page_size") != "7" {
				t.Error("search pagination missing")
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !has(body, "filter") || !has(body, "field_names") {
				t.Error("missing search body")
			}
			respond(w, map[string]any{"items": []any{}, "has_more": true, "page_token": "third"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/fields") {
			respond(w, map[string]any{"items": []any{schemaField("title", 1)}})
			return
		}
		if r.Method == "POST" {
			writes++
			respond(w, map[string]any{"record": map[string]any{"record_id": "recNew"}})
			return
		}
		respond(w, map[string]any{"record": map[string]any{"record_id": "recNew", "fields": map[string]any{"title": "new"}}})
	})
	result, err := c.Run(context.Background(), Params{Action: "create", Table: "bugs", Fields: map[string]any{"title": "new"}})
	if err != nil || obj(result)["verified"] != true || writes != 1 {
		t.Fatalf("%v %v writes=%d", result, err, writes)
	}
	result, err = c.Run(context.Background(), Params{Action: "search", Table: "tasks", Limit: 7, PageToken: "next", Filter: map[string]any{"conjunction": "and", "conditions": []any{}}, FieldNames: []string{"title"}})
	if err != nil || obj(result)["page_token"] != "third" || writes != 1 {
		t.Fatalf("%v %v", result, err)
	}
}
