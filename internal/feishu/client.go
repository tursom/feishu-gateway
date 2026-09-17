package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Credentials struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

type Result struct {
	Status    int
	Body      []byte
	RequestID string
	Stage     string
}

type TransportError struct {
	Stage            string
	ResponseReceived bool
	Status           int
	Message          string
}

func (e *TransportError) Error() string { return e.Stage + ": " + e.Message }

// Configure HTTP and BaseURL before using Client concurrently.
// Cache state is private and scoped to the current credential pair.
type Client struct {
	HTTP     *http.Client
	BaseURL  string
	mu       sync.Mutex
	key      [32]byte
	token    string
	expires  time.Time
	objToken string
}

func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second}, BaseURL: "https://open.feishu.cn"}
}

func failure(r Result, message string) error {
	return &TransportError{Stage: r.Stage, ResponseReceived: r.Status != 0, Status: r.Status, Message: message}
}

func (c *Client) request(ctx context.Context, stage, method, path string, query url.Values, body []byte, token string) (Result, error) {
	r := Result{Stage: stage}
	base := c.BaseURL
	if base == "" {
		base = "https://open.feishu.cn"
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil {
		return r, failure(r, "cannot construct request URL")
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return r, failure(r, "cannot construct request")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.GetBody = nil
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := http.Client{}
	if c.HTTP != nil {
		hc = *c.HTTP
	}
	hc.Timeout = 30 * time.Second
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return r, failure(r, "HTTP request failed")
	}
	defer resp.Body.Close()
	r.Status = resp.StatusCode
	r.RequestID = resp.Header.Get("X-Tt-Logid")
	if r.RequestID == "" {
		r.RequestID = resp.Header.Get("X-Request-Id")
	}
	r.Body, err = io.ReadAll(resp.Body)
	if err != nil {
		r.Body = nil
		return r, failure(r, "cannot read complete response body")
	}
	return r, nil
}

type envelope struct {
	Code   *int   `json:"code"`
	Token  string `json:"tenant_access_token"`
	Expire int64  `json:"expire"`
	Data   struct {
		Node struct {
			ObjToken string `json:"obj_token"`
		} `json:"node"`
	} `json:"data"`
}

func decode(r Result) (envelope, bool) {
	var e envelope
	err := json.Unmarshal(r.Body, &e)
	return e, err == nil && e.Code != nil && *e.Code == 0 && r.Status >= 200 && r.Status < 300
}

func redact(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		encoded, _ := json.Marshal(secret)
		s = strings.ReplaceAll(s, string(encoded[1:len(encoded)-1]), "[REDACTED]")
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	return s
}

func removeTokens(v any) {
	switch v := v.(type) {
	case map[string]any:
		delete(v, "tenant_access_token")
		for _, child := range v {
			removeTokens(child)
		}
	case []any:
		for _, child := range v {
			removeTokens(child)
		}
	}
}

func sanitize(r Result, secret, token string) Result {
	if r.Stage == "auth" {
		var v any
		// RawMessage-free decoding uses Number to preserve numeric values.
		d := json.NewDecoder(bytes.NewReader(r.Body))
		d.UseNumber()
		if d.Decode(&v) == nil && json.Valid(r.Body) {
			removeTokens(v)
			r.Body, _ = json.Marshal(v)
		}
	}
	r.Body = []byte(redact(string(r.Body), secret, token))
	r.RequestID = redact(r.RequestID, secret, token)
	return r
}

// Do performs auth and wiki resolution when needed, followed by one operation.
// path is relative to the Bitable base; the caller owns its allowlist.
func (c *Client) Do(ctx context.Context, creds Credentials, method, path string, query url.Values, body []byte) (Result, error) {
	// Serialize cache setup; release before the potentially long operation.
	c.mu.Lock()
	token, obj, r, err := c.prepare(ctx, creds)
	c.mu.Unlock()
	if err != nil || r.Stage != "" {
		return r, err
	}
	r, err = c.request(ctx, "operation", method, "/open-apis/bitable/v1/apps/"+url.PathEscape(obj)+path, query, body, token)
	var e envelope
	_ = json.Unmarshal(r.Body, &e)
	if err != nil || r.Status < 200 || r.Status >= 300 || (e.Code != nil && *e.Code != 0) {
		r = sanitize(r, creds.AppSecret, token)
	}
	return r, err
}

func (c *Client) prepare(ctx context.Context, creds Credentials) (string, string, Result, error) {
	payload, _ := json.Marshal(creds)
	key := sha256.Sum256(payload)
	if key != c.key {
		c.key, c.token, c.objToken, c.expires = key, "", "", time.Time{}
	}
	if c.token == "" || !time.Now().Before(c.expires) {
		started := time.Now()
		r, err := c.request(ctx, "auth", http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", nil, payload, "")
		e, ok := decode(r)
		if err != nil || !ok {
			return "", "", sanitize(sanitize(r, creds.AppSecret, e.Token), creds.AppSecret, c.token), err
		}
		if e.Token == "" || e.Expire <= 0 {
			return "", "", sanitize(r, creds.AppSecret, e.Token), failure(r, "invalid auth response: missing token or expiry")
		}
		c.token = e.Token
		c.expires = started.Add(time.Duration(e.Expire) * time.Second)
	}
	if c.objToken == "" {
		q := url.Values{"token": {"VXBrwsp2Zii0tnkiVKPcJOvpnmg"}}
		r, err := c.request(ctx, "resolve", http.MethodGet, "/open-apis/wiki/v2/spaces/get_node", q, nil, c.token)
		e, ok := decode(r)
		if err == nil && !ok && e.Code != nil && *e.Code == 0 && r.Status >= 200 && r.Status < 300 {
			err = failure(r, "invalid resolve response: malformed data.node.obj_token")
		}
		if err != nil || !ok {
			return "", "", sanitize(r, creds.AppSecret, c.token), err
		}
		if e.Data.Node.ObjToken == "" {
			return "", "", sanitize(r, creds.AppSecret, c.token), failure(r, "invalid resolve response: missing data.node.obj_token")
		}
		c.objToken = e.Data.Node.ObjToken
	}
	return c.token, c.objToken, Result{}, nil
}
