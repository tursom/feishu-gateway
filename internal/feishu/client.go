// Package feishu provides a policy-constrained client for the company Bitable.
package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Table struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Policy string `json:"policy"`
}

var Tables = map[string]Table{
	"requirements": {"需求管理", "tblAMCz7qoUVZzfH", "需求状态", "只读；仅允许更新需求状态，不创建需求"},
	"tasks":        {"任务管理", "tblJsfSnouoWxazc", "任务状态", "允许创建和更新所有 API 可写字段"},
	"bugs":         {"BUG管理", "tblyOWQEWoKU8tEd", "BUG状态", "允许创建；通常仅更新BUG状态，其他字段需用户明确授权依据"},
}

// Keep the authorization whitelist independent of the exported metadata map.
var allowedTables = func() map[string]Table {
	m := map[string]Table{}
	for k, v := range Tables {
		m[k] = v
	}
	return m
}()

type Error struct {
	Status       int    `json:"status"`
	Code         string `json:"code"`
	Message      string `json:"message"`
	WriteOutcome string `json:"writeOutcome,omitempty"`
	RecordID     string `json:"recordId,omitempty"`
}

func (e *Error) Error() string { return e.Message }
func fail(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

type Params struct {
	Action           string         `json:"action"`
	Table            string         `json:"table"`
	RecordID         string         `json:"recordId"`
	Fields           map[string]any `json:"fields"`
	ExpectedRevision string         `json:"expectedRevision"`
	ExceptionReason  string         `json:"exceptionReason"`
	Filter           map[string]any `json:"filter"`
	FieldNames       []string       `json:"fieldNames"`
	Limit            int            `json:"limit"`
	PageToken        string         `json:"pageToken"`
}

// Configure HTTPClient and BaseURL before using the client concurrently.
// BaseURL includes /open-apis (the default). Redirects are always disabled.
type Client struct {
	HTTPClient        *http.Client
	BaseURL           string
	mu                sync.Mutex
	cachedToken       string
	tokenUntil        time.Time
	appToken          string
	credentialLoader  func() (map[string]any, error)
	credentialVersion uint64
}

func NewClient() *Client {
	return &Client{HTTPClient: &http.Client{Timeout: 30 * time.Second}, BaseURL: "https://open.feishu.cn/open-apis"}
}

var recordPattern = regexp.MustCompile(`^rec[a-zA-Z0-9]+$`)
var revisionPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validatePolicy(p Params) error {
	switch p.Action {
	case "tables":
		return nil
	case "fields", "search", "get", "create", "update":
	default:
		return fail(400, "INVALID_PARAMS", "Unsupported action")
	}
	t, ok := allowedTables[p.Table]
	if !ok {
		return fail(403, "POLICY_DENIED", "Only requirements, tasks, bugs are allowed")
	}
	if (p.Action == "get" || p.Action == "update") && !recordPattern.MatchString(p.RecordID) {
		return fail(400, "INVALID_PARAMS", "A valid recordId is required")
	}
	if p.Action == "search" && (p.Limit < 0 || p.Limit > 100) {
		return fail(400, "INVALID_PARAMS", "limit must be 1..100")
	}
	if p.Action != "create" && p.Action != "update" {
		return nil
	}
	if len(p.Fields) == 0 {
		return fail(400, "INVALID_PARAMS", "Non-empty fields required")
	}
	if p.Table == "requirements" && (p.Action == "create" || len(p.Fields) != 1 || !has(p.Fields, t.Status)) {
		return fail(403, "POLICY_DENIED", "需求仅允许更新需求状态，禁止创建或修改其他字段")
	}
	if p.Action == "update" {
		if !revisionPattern.MatchString(p.ExpectedRevision) {
			return fail(400, "INVALID_PARAMS", "update requires expectedRevision from get")
		}
		if p.Table == "bugs" && strings.TrimSpace(p.ExceptionReason) == "" {
			for k := range p.Fields {
				if k != t.Status {
					return fail(403, "POLICY_DENIED", "修改 BUG 非状态字段必须提供 exceptionReason 授权依据")
				}
			}
		}
	}
	return nil
}
func has(m map[string]any, k string) bool { _, ok := m[k]; return ok }
func obj(v any) map[string]any            { m, _ := v.(map[string]any); return m }
func text(v any) string                   { s, _ := v.(string); return s }
func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case int:
		return float64(n)
	}
	return 0
}

// request never exposes response bodies, transport errors, URLs or credentials.
func (c *Client) request(ctx context.Context, method, path, token string, body any, mutation bool) (map[string]any, error) {
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fail(422, "INVALID_FIELD_VALUE", "Request contains an invalid JSON value")
		}
		input = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	base := c.BaseURL
	if base == "" {
		base = "https://open.feishu.cn/open-apis"
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/"+path, input)
	if err != nil {
		return nil, fail(500, "CONFIG_ERROR", "Invalid Feishu endpoint configuration")
	}
	// Do not give net/http a replayable body for mutations.
	if mutation {
		req.GetBody = nil
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := http.Client{}
	if c.HTTPClient != nil {
		hc = *c.HTTPClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	uncertain := func() error {
		if mutation {
			return &Error{Status: 502, Code: "WRITE_UNCERTAIN", Message: "写入结果未确认；请先查询核实，不要盲目重试", WriteOutcome: "unknown"}
		}
		return fail(502, "UPSTREAM_ERROR", "Feishu request failed")
	}
	resp, err := hc.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, uncertain()
	}
	defer resp.Body.Close()
	var result map[string]any
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	dec.UseNumber()
	if err = dec.Decode(&result); err != nil {
		return nil, uncertain()
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, uncertain()
	}
	code, ok := result["code"].(json.Number)
	if resp.StatusCode >= 500 || !ok {
		return nil, uncertain()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || code.String() != "0" {
		return nil, fail(502, "UPSTREAM_ERROR", "Feishu rejected the request")
	}
	return result, nil
}

// SetCredentialLoader installs a server-managed source and invalidates cached auth.
func (c *Client) SetCredentialLoader(loader func() (map[string]any, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credentialLoader = loader
	c.invalidateCredentialsLocked()
}
func (c *Client) invalidateCredentialsLocked() {
	c.cachedToken = ""
	c.tokenUntil = time.Time{}
	c.appToken = ""
	c.credentialVersion++
}
func (c *Client) InvalidateCredentials() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateCredentialsLocked()
}
func credentials() (map[string]any, error) { return LoadCredentials("") }

// LoadCredentials retains environment and legacy file compatibility.
func LoadCredentials(fallbackPath string) (map[string]any, error) {
	id, secret := os.Getenv("FEISHU_APP_ID"), os.Getenv("FEISHU_APP_SECRET")
	if id == "" || secret == "" {
		path := fallbackPath
		if path == "" {
			path = os.Getenv("FEISHU_CREDENTIALS_FILE")
		}
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fail(500, "CONFIG_ERROR", "Feishu credentials unavailable")
			}
			path = filepath.Join(home, ".config/feishu/credentials.json")
		}
		b, err := os.ReadFile(path)
		var stored struct {
			ID     string `json:"app_id"`
			Secret string `json:"app_secret"`
		}
		if err != nil || json.Unmarshal(b, &stored) != nil {
			return nil, fail(500, "CONFIG_ERROR", "Feishu credentials unavailable")
		}
		if id == "" {
			id = stored.ID
		}
		if secret == "" {
			secret = stored.Secret
		}
	}
	if id == "" || secret == "" {
		return nil, fail(500, "CONFIG_ERROR", "Feishu credentials unavailable")
	}
	return map[string]any{"app_id": id, "app_secret": secret}, nil
}
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedToken != "" && time.Now().Before(c.tokenUntil) {
		return c.cachedToken, nil
	}
	loader := c.credentialLoader
	if loader == nil {
		loader = credentials
	}
	cred, err := loader()
	if err != nil {
		return "", err
	}
	r, err := c.request(ctx, "POST", "auth/v3/tenant_access_token/internal", "", cred, false)
	if err != nil {
		return "", err
	}
	token := text(r["tenant_access_token"])
	if token == "" {
		return "", fail(502, "UPSTREAM_ERROR", "Feishu authentication failed")
	}
	expire := number(r["expire"])
	if !has(r, "expire") {
		expire = 7200
	}
	c.cachedToken = token
	c.tokenUntil = time.Now().Add(time.Duration(math.Max(0, math.Min(expire, 86400)-120)) * time.Second)
	return token, nil
}
func (c *Client) api(ctx context.Context, method, path string, body any, mutation bool) (map[string]any, error) {
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	r, err := c.request(ctx, method, path, token, body, mutation)
	if err != nil {
		return nil, err
	}
	data := obj(r["data"])
	if data == nil {
		if mutation {
			return nil, &Error{Status: 502, Code: "VERIFICATION_FAILED", Message: "写入成功，但返回数据不完整；请查询核实", WriteOutcome: "committed"}
		}
		return nil, fail(502, "UPSTREAM_ERROR", "Feishu returned invalid data")
	}
	return data, nil
}
func (c *Client) base(ctx context.Context) (string, error) {
	c.mu.Lock()
	app := c.appToken
	version := c.credentialVersion
	c.mu.Unlock()
	if app == "" {
		r, err := c.api(ctx, "GET", "wiki/v2/spaces/get_node?token=VXBrwsp2Zii0tnkiVKPcJOvpnmg", nil, false)
		if err != nil {
			return "", err
		}
		node := obj(r["node"])
		app = text(node["obj_token"])
		if text(node["obj_type"]) != "bitable" || app == "" {
			return "", fail(502, "UPSTREAM_ERROR", "Configured wiki node is not a Bitable")
		}
		c.mu.Lock()
		if c.credentialVersion == version {
			c.appToken = app
		}
		c.mu.Unlock()
	}
	return "bitable/v1/apps/" + url.PathEscape(app), nil
}
func (c *Client) fields(ctx context.Context, path string) ([]any, error) {
	items := []any{}
	cursor := ""
	seen := map[string]bool{}
	for {
		q := url.Values{"page_size": {"100"}}
		if cursor != "" {
			q.Set("page_token", cursor)
		}
		data, err := c.api(ctx, "GET", path+"/fields?"+q.Encode(), nil, false)
		if err != nil {
			return nil, err
		}
		page, ok := data["items"].([]any)
		if !ok {
			return nil, fail(502, "UPSTREAM_ERROR", "Invalid field schema")
		}
		items = append(items, page...)
		if data["has_more"] != true {
			return items, nil
		}
		cursor = text(data["page_token"])
		if cursor == "" || seen[cursor] {
			return nil, fail(502, "UPSTREAM_ERROR", "Invalid schema pagination cursor")
		}
		seen[cursor] = true
	}
}
func canonical(v any) []byte {
	// Normalize json.Number and Go numeric types before hashing, so equivalent
	// JSON numbers (such as 1, 1.0 and 1e0) produce the same revision.
	if raw, err := json.Marshal(v); err == nil {
		var normalized any
		if json.Unmarshal(raw, &normalized) == nil {
			v = normalized
		}
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
func revision(fields any) string {
	sum := sha256.Sum256(canonical(fields))
	return hex.EncodeToString(sum[:])
}
func (c *Client) get(ctx context.Context, path, id string) (map[string]any, error) {
	data, err := c.api(ctx, "GET", path+"/records/"+url.PathEscape(id)+"?user_id_type=open_id", nil, false)
	if err != nil {
		return nil, err
	}
	r := obj(data["record"])
	if r == nil || obj(r["fields"]) == nil || text(r["record_id"]) != id {
		return nil, fail(502, "UPSTREAM_ERROR", "Invalid record returned by Feishu")
	}
	r["revision"] = revision(r["fields"])
	return r, nil
}
func acquire(ctx context.Context, tableID string) (func(), error) {
	dir := os.Getenv("FEISHU_LOCK_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fail(500, "LOCK_ERROR", "Cannot initialize write lock")
		}
		dir = filepath.Join(home, ".cache/pi-feishu-locks")
	}
	if os.MkdirAll(dir, 0700) != nil {
		return nil, fail(500, "LOCK_ERROR", "Cannot initialize write lock")
	}
	lock := filepath.Join(dir, tableID+".lock")
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		if ctx.Err() != nil {
			return nil, fail(408, "REQUEST_CANCELED", "Request canceled before writing")
		}
		err := os.Mkdir(lock, 0700)
		if err == nil {
			return func() { _ = os.Remove(lock) }, nil
		}
		if !os.IsExist(err) {
			return nil, fail(500, "LOCK_ERROR", "Cannot acquire write lock")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fail(408, "REQUEST_CANCELED", "Request canceled before writing")
		case <-deadline.C:
			timer.Stop()
			return nil, fail(409, "LOCK_BUSY", "其他会话正在更新此表；未发起写入")
		case <-timer.C:
		}
	}
}
func (c *Client) Run(ctx context.Context, p Params) (any, error) {
	if err := validatePolicy(p); err != nil {
		return nil, err
	}
	if p.Action == "tables" {
		tables := map[string]Table{}
		for k, v := range allowedTables {
			tables[k] = v
		}
		return map[string]any{"tables": tables, "deletesSupported": false, "platformLimit": "需求状态和任务状态为流程字段(type=24)，API仅支持读取；BUG状态可写。"}, nil
	}
	if p.Action == "create" || p.Action == "update" {
		unlock, err := acquire(ctx, allowedTables[p.Table].ID)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	base, err := c.base(ctx)
	if err != nil {
		return nil, err
	}
	path := base + "/tables/" + allowedTables[p.Table].ID
	switch p.Action {
	case "fields":
		fields, err := c.fields(ctx, path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"table": allowedTables[p.Table], "fields": fields}, nil
	case "get":
		return c.get(ctx, path, p.RecordID)
	case "search":
		limit := p.Limit
		if limit == 0 {
			limit = 20
		}
		q := url.Values{"page_size": {strconv.Itoa(limit)}, "user_id_type": {"open_id"}}
		if p.PageToken != "" {
			q.Set("page_token", p.PageToken)
		}
		body := map[string]any{}
		if p.Filter != nil {
			body["filter"] = p.Filter
		}
		if p.FieldNames != nil {
			body["field_names"] = p.FieldNames
		}
		data, err := c.api(ctx, "POST", path+"/records/search?"+q.Encode(), body, false)
		if err != nil {
			return nil, err
		}
		data["scope"] = "Entire table; no view filter. Use page_token while has_more is true. Call get before update."
		return data, nil
	}
	schema, err := c.fields(ctx, path)
	if err != nil {
		return nil, err
	}
	if err = validateFields(p.Fields, schema); err != nil {
		return nil, err
	}
	if p.Action == "update" {
		before, err := c.get(ctx, path, p.RecordID)
		if err != nil {
			return nil, err
		}
		if before["revision"] != p.ExpectedRevision {
			return nil, fail(409, "REVISION_CONFLICT", "记录已变化，请重新读取；未写入")
		}
	}
	endpoint := path + "/records"
	method := "POST"
	if p.Action == "update" {
		endpoint += "/" + url.PathEscape(p.RecordID)
		method = "PUT"
	}
	written, err := c.api(ctx, method, endpoint+"?user_id_type=open_id", map[string]any{"fields": p.Fields}, true)
	if err != nil {
		if e, ok := err.(*Error); ok && e.WriteOutcome != "" {
			e.RecordID = p.RecordID
		}
		return nil, err
	}
	id := text(obj(written["record"])["record_id"])
	if id == "" {
		id = p.RecordID
	}
	verificationError := func() error {
		return &Error{Status: 502, Code: "VERIFICATION_FAILED", Message: "写入已成功，但回读失败；请查询核验，不要重复创建", WriteOutcome: "committed", RecordID: id}
	}
	if !recordPattern.MatchString(id) {
		return nil, verificationError()
	}
	after, err := c.get(ctx, path, id)
	if err != nil {
		return nil, verificationError()
	}
	mismatches := []string{}
	for k, v := range p.Fields {
		if !fieldEqual(v, obj(after["fields"])[k], fieldType(schema, k)) {
			mismatches = append(mismatches, k)
		}
	}
	sort.Strings(mismatches)
	result := map[string]any{"action": p.Action, "record": after, "submittedFields": p.Fields, "verified": len(mismatches) == 0, "mismatchedFields": mismatches}
	if p.ExceptionReason != "" {
		result["exceptionReason"] = p.ExceptionReason
	}
	if len(mismatches) > 0 {
		result["note"] = "API写入成功，但回读值存在差异，请核验。"
	} else {
		result["note"] = "提交字段与回读结果一致。"
	}
	return result, nil
}
func fieldType(schema []any, key string) int {
	for _, v := range schema {
		f := obj(v)
		if text(f["field_name"]) == key {
			return int(number(f["type"]))
		}
	}
	return 0
}
func validateFields(fields map[string]any, schema []any) error {
	for k, v := range fields {
		var f map[string]any
		for _, s := range schema {
			if text(obj(s)["field_name"]) == k {
				f = obj(s)
				break
			}
		}
		if f == nil {
			return fail(422, "UNKNOWN_FIELD", "Unknown field; read fields first")
		}
		typ := int(number(f["type"]))
		switch typ {
		case 19, 20, 24, 1001, 1002, 1003, 1004, 1005, 3001:
			return fail(422, "FIELD_READ_ONLY", "流程、按钮及系统计算字段仅支持读取")
		}
		if f["is_read_only"] == true {
			return fail(422, "FIELD_READ_ONLY", "Field is read-only")
		}
		if v == nil {
			continue
		}
		// Normalize typed Go slices/numbers to the same values as JSON API input.
		b, err := json.Marshal(v)
		if err != nil {
			return fail(422, "INVALID_FIELD_VALUE", "Invalid field value")
		}
		var value any
		_ = json.Unmarshal(b, &value)
		valid := true
		switch typ {
		case 1:
			_, valid = value.(string)
		case 2, 5:
			n, ok := value.(float64)
			valid = ok && !math.IsNaN(n) && !math.IsInf(n, 0)
			if typ == 5 {
				valid = valid && math.Trunc(n) == n
			}
		case 3, 4:
			values := []any{value}
			if typ == 4 {
				values, valid = value.([]any)
			}
			options, _ := obj(f["property"])["options"].([]any)
			if len(options) == 0 {
				valid = false
			}
			for _, item := range values {
				s, ok := item.(string)
				found := false
				for _, option := range options {
					if text(obj(option)["name"]) == s {
						found = true
					}
				}
				valid = valid && ok && found
			}
		case 7:
			_, valid = value.(bool)
		case 11, 17, 18, 21:
			values, ok := value.([]any)
			valid = ok
			for _, item := range values {
				switch typ {
				case 11:
					valid = valid && strings.HasPrefix(text(obj(item)["id"]), "ou_")
				case 17:
					valid = valid && text(obj(item)["file_token"]) != ""
				case 18, 21:
					valid = valid && recordPattern.MatchString(text(item))
				}
			}
		case 13:
			_, valid = value.(string)
		case 15:
			m := obj(value)
			valid = m != nil && text(m["link"]) != ""
			if has(m, "text") {
				_, ok := m["text"].(string)
				valid = valid && ok
			}
		case 22:
			m := obj(value)
			valid = m != nil && text(m["location"]) != ""
		case 23:
			m := obj(value)
			valid = m != nil && text(m["id"]) != ""
		default:
			return fail(422, "FIELD_READ_ONLY", "Unsupported field type; cannot safely write")
		}
		if !valid {
			return fail(422, "INVALID_FIELD_VALUE", "Invalid field format or option; read field configuration first")
		}
	}
	return nil
}
func fieldEqual(sent, got any, typ int) bool {
	if sent == nil {
		if got == nil || got == "" {
			return true
		}
		v := reflect.ValueOf(got)
		return v.Kind() == reflect.Slice && v.Len() == 0
	}
	normalize := func(value any) any {
		b, err := json.Marshal(value)
		if err != nil {
			return value
		}
		_ = json.Unmarshal(b, &value)
		if typ == 1 {
			if a, ok := value.([]any); ok {
				var out strings.Builder
				for _, v := range a {
					s, ok := obj(v)["text"].(string)
					if !ok {
						return value
					}
					out.WriteString(s)
				}
				return out.String()
			}
		}
		if typ == 3 || typ == 24 {
			if m := obj(value); m != nil {
				return m["name"]
			}
		}
		if typ == 4 || typ == 11 || typ == 17 || typ == 18 || typ == 21 {
			if a, ok := value.([]any); ok {
				ids := []string{}
				for _, v := range a {
					s, ok := v.(string)
					if !ok {
						m := obj(v)
						keys := []string{"id", "record_id", "file_token"}
						if typ == 4 {
							keys = []string{"name"}
						}
						for _, k := range keys {
							if s = text(m[k]); s != "" {
								break
							}
						}
						if s == "" {
							return value
						}
					}
					ids = append(ids, s)
				}
				sort.Strings(ids)
				return ids
			}
		}
		return value
	}
	return bytes.Equal(canonical(normalize(sent)), canonical(normalize(got)))
}
