package feishu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

var credentials = Credentials{AppID: "app", AppSecret: "private-secret"}

const authPath = "/open-apis/auth/v3/tenant_access_token/internal"
const wikiPath = "/open-apis/wiki/v2/spaces/get_node"
const operationPath = "/open-apis/bitable/v1/apps/base-token/tables/tbl1/records"

type fixture struct {
	mu      sync.Mutex
	calls   []string
	handler func(http.ResponseWriter, *http.Request)
}

func setup(t *testing.T) (*Client, *fixture) {
	t.Helper()
	f := &fixture{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if f.handler != nil {
			f.handler(w, r)
			return
		}
		standard(w, r)
	}))
	t.Cleanup(s.Close)
	c := New()
	c.BaseURL = s.URL
	return c, f
}

func standard(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case authPath:
		fmt.Fprint(w, `{"code":0,"tenant_access_token":"access-token","expire":7200}`)
	case wikiPath:
		fmt.Fprint(w, `{"code":0,"data":{"node":{"obj_token":"base-token"}}}`)
	default:
		fmt.Fprint(w, `{"code":0,"data":{"records":[]}}`)
	}
}

func perform(c *Client, ctx context.Context, creds Credentials) (Result, error) {
	return c.Do(ctx, creds, "POST", "/tables/tbl1/records", url.Values{"page_size": {"20"}}, []byte(`{"fields":{"name":"example"}}`))
}

func TestCacheSwitchExpiryAndRequestContract(t *testing.T) {
	c, f := setup(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case authPath:
			if r.Method != "POST" || r.Header.Get("Authorization") != "" {
				t.Error("invalid auth request")
			}
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"app_secret":`) {
				t.Error("missing credentials")
			}
		case wikiPath:
			if r.Method != "GET" || r.URL.Query().Get("token") != "VXBrwsp2Zii0tnkiVKPcJOvpnmg" {
				t.Error("invalid wiki request")
			}
		case operationPath:
			b, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || r.URL.Query().Get("page_size") != "20" || string(b) != `{"fields":{"name":"example"}}` {
				t.Error("operation changed")
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Path != authPath && r.Header.Get("Authorization") != "Bearer access-token" {
			t.Error("missing bearer")
		}
		standard(w, r)
	}
	for _, creds := range []Credentials{credentials, credentials, {AppID: "app", AppSecret: "other"}, credentials} {
		r, err := perform(c, context.Background(), creds)
		if err != nil || r.Stage != "operation" {
			t.Fatalf("%+v %v", r, err)
		}
	}
	c.mu.Lock()
	c.expires = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if _, err := perform(c, context.Background(), credentials); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	counts := map[string]int{}
	for _, call := range f.calls {
		counts[call]++
	}
	if counts["POST "+authPath] != 4 || counts["GET "+wikiPath] != 3 || counts["POST "+operationPath] != 5 || len(f.calls) != 12 {
		t.Fatalf("unexpected requests: %v", f.calls)
	}
}

func TestOperationPassthrough(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{200, "not JSON\n"}, {200, ` {"code":125,"msg":"denied"} `},
		{403, " forbidden\n"}, {201, `{"code":0,"data":{"tenant_access_token":"business-value","secret":"private-secret"}}`},
	} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			c, f := setup(t)
			f.handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != operationPath {
					standard(w, r)
					return
				}
				w.Header().Set("X-Tt-Logid", "log-id")
				w.Header().Set("X-Request-Id", "fallback")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}
			r, err := perform(c, context.Background(), credentials)
			if err != nil || r.Status != tc.status || string(r.Body) != tc.body || r.RequestID != "log-id" || r.Stage != "operation" {
				t.Fatalf("%+v %v", r, err)
			}
			if len(f.calls) != 3 {
				t.Fatal(f.calls)
			}
		})
	}
}

func TestPrerequisiteFailuresAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		stage     string
		status    int
		body      string
		wantError bool
	}{
		{"auth", 401, `{"code":1,"tenant_access_token":"access-token","msg":"private-secret access-token"}`, false},
		{"auth", 200, `{"code":1,"tenant_access_token":"access-token","msg":"private-secret access-token"}`, false},
		{"auth", 503, `private-secret`, false},
		{"resolve", 200, `{"code":1,"msg":"private-secret access-token"}`, false},
		{"resolve", 502, `private-secret access-token`, false},
		{"resolve", 200, `{"code":0,"data":{}}`, true},
		{"resolve", 200, `{"code":0,"data":{"node":{"obj_token":123}}}`, true},
		{"operation", 400, `{"msg":"private-secret access-token"}`, false},
	} {
		t.Run(fmt.Sprint(tc.stage, tc.status, tc.body), func(t *testing.T) {
			c, f := setup(t)
			target := map[string]string{"auth": authPath, "resolve": wikiPath, "operation": operationPath}[tc.stage]
			f.handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != target {
					standard(w, r)
					return
				}
				w.Header().Set("X-Request-Id", "id-private-secret")
				if tc.stage != "auth" || strings.Contains(tc.body, "tenant_access_token") {
					w.Header().Set("X-Request-Id", "id-private-secret-access-token")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}
			r, err := perform(c, context.Background(), credentials)
			if (err != nil) != tc.wantError || r.Stage != tc.stage || r.Status != tc.status {
				t.Fatalf("%+v %v", r, err)
			}
			text := fmt.Sprintf("%+v %s %v", r, r.Body, err)
			if strings.Contains(text, "private-secret") || strings.Contains(text, "access-token") || (tc.stage == "auth" && strings.Contains(string(r.Body), "tenant_access_token")) {
				t.Fatalf("leaked: %s", text)
			}
			want := map[string]int{"auth": 1, "resolve": 2, "operation": 3}[tc.stage]
			if len(f.calls) != want {
				t.Fatalf("extra request: %v", f.calls)
			}
			if tc.wantError {
				var te *TransportError
				if !errors.As(err, &te) || !te.ResponseReceived || te.Status != 200 || !strings.Contains(te.Message, "resolve response") {
					t.Fatal(err)
				}
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) {
	return 0, errors.New("private-secret access-token URL")
}
func (brokenReader) Close() error { return nil }

func TestTransportStagesAndNoRetry(t *testing.T) {
	for _, stage := range []string{"auth", "resolve", "operation"} {
		for _, received := range []bool{false, true} {
			t.Run(fmt.Sprint(stage, received), func(t *testing.T) {
				c := New()
				calls := 0
				target := map[string]int{"auth": 1, "resolve": 2, "operation": 3}[stage]
				c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == target {
						if received {
							return &http.Response{StatusCode: 502, Header: http.Header{}, Body: brokenReader{}, Request: r}, nil
						}
						return nil, errors.New("https://private-secret/access-token")
					}
					w := httptest.NewRecorder()
					standard(w, r)
					return w.Result(), nil
				})
				r, err := perform(c, context.Background(), credentials)
				var te *TransportError
				if !errors.As(err, &te) || te.Stage != stage || te.ResponseReceived != received || r.Stage != stage || calls != target {
					t.Fatalf("%+v %v calls=%d", r, err, calls)
				}
				if received && (r.Status != 502 || te.Status != 502) {
					t.Fatal(r, te)
				}
				if strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), "access-token") || strings.Contains(err.Error(), "https://") {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	for _, stage := range []string{"auth", "resolve", "operation"} {
		t.Run(stage, func(t *testing.T) {
			c, f := setup(t)
			target := map[string]string{"auth": authPath, "resolve": wikiPath, "operation": operationPath}[stage]
			f.handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect-target" {
					t.Error("redirect followed")
				}
				if r.URL.Path != target {
					standard(w, r)
					return
				}
				http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
			}
			c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { t.Error("injected redirect policy used"); return nil }
			r, err := perform(c, context.Background(), credentials)
			if err != nil || r.Status != 307 || r.Stage != stage {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	for _, stage := range []string{"auth", "resolve", "operation"} {
		t.Run(stage, func(t *testing.T) {
			c, f := setup(t)
			release := make(chan struct{})
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			target := map[string]string{"auth": authPath, "resolve": wikiPath, "operation": operationPath}[stage]
			f.handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != target {
					standard(w, r)
					return
				}
				cancel()
				<-release
			}
			r, err := perform(c, ctx, credentials)
			var te *TransportError
			if !errors.As(err, &te) || te.Stage != stage || te.ResponseReceived || r.Stage != stage {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestConcurrentCache(t *testing.T) {
	c, f := setup(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := perform(c, context.Background(), credentials); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 22 {
		t.Fatalf("cache missed: %v", f.calls)
	}
}
