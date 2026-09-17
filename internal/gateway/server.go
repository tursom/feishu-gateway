package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"feishu-gateway/internal/feishu"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Feishu interface {
	Run(context.Context, feishu.Params) (any, error)
}
type Server struct {
	Config           Config
	Store            *Store
	Client           Feishu
	Web              fs.FS
	csrf             string
	slots            chan struct{}
	entryMu          sync.Mutex
	entryMinute      int64
	entryCount       int
	credentials      *CredentialManager
	credentialSaveMu sync.Mutex
	entrySlots       chan struct{}
}

func NewServer(c Config, s *Store, f Feishu, web fs.FS) *Server {
	credentials := newCredentialManager(s.Path, c.CredentialsFile)
	if configurable, ok := f.(interface {
		SetCredentialLoader(func() (map[string]any, error))
	}); ok {
		configurable.SetCredentialLoader(credentials.Load)
	}
	return &Server{Config: c, Store: s, Client: f, Web: web, credentials: credentials, csrf: randomID("csrf_"), slots: make(chan struct{}, 16), entrySlots: make(chan struct{}, 64)}
}

type response struct {
	Status int
	Body   any
}

func ok(data any) response { return response{200, map[string]any{"data": data}} }
func errorResponse(err error, id string) response {
	status, code, message := 500, "INTERNAL_ERROR", "服务内部错误，请根据 requestId 查看服务日志"
	extra := map[string]any{}
	var a *APIError
	var f *feishu.Error
	if errors.As(err, &a) {
		status, code, message = a.Status, a.Code, a.Message
	} else if errors.As(err, &f) {
		status, code, message = f.Status, f.Code, f.Message
		if f.WriteOutcome != "" {
			extra["writeOutcome"] = f.WriteOutcome
		}
		if f.RecordID != "" {
			extra["recordId"] = f.RecordID
		}
	} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		status, code, message = 504, "REQUEST_INTERRUPTED", "请求超时或中断；若执行写入，请先核实记录，不要盲目重复创建"
	}
	extra["code"] = code
	extra["message"] = message
	return response{status, map[string]any{"requestId": id, "error": extra}}
}
func jsonBytes(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		return []byte(`{"error":{"code":"ENCODING_ERROR","message":"响应编码失败"}}`)
	}
	return b
}
func writeResponse(w http.ResponseWriter, r response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(r.Status)
	w.Write(jsonBytes(r.Body))
}
func (s *Server) admin(r *http.Request) (string, error) {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fail(403, "ADMIN_ENTRY_DENIED", "管理入口不可直接访问")
	}
	if s.Config.Mode == "local" {
		if !ip.IsLoopback() {
			return "", fail(403, "ADMIN_ENTRY_DENIED", "本地模式仅允许回环访问")
		}
	} else {
		trusted := false
		for _, allowed := range s.Config.TrustedProxyIPs {
			if ip.Equal(net.ParseIP(allowed)) {
				trusted = true
				break
			}
		}
		if !trusted || len(r.Header.Values("X-Gateway-Secret")) != 1 || !secureEqual(r.Header.Get("X-Gateway-Secret"), s.Config.ProxySecret) {
			return "", fail(403, "ADMIN_ENTRY_DENIED", "请通过 Pangolin 管理入口访问")
		}
	}
	origin, _ := url.Parse(s.Config.PublicOrigin)
	if !strings.EqualFold(r.Host, origin.Host) {
		return "", fail(403, "INVALID_HOST", "管理入口 Host 不匹配")
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		if r.Header.Get("Origin") != s.Config.PublicOrigin || !secureEqual(r.Header.Get("X-CSRF-Token"), s.csrf) {
			return "", fail(403, "CSRF_DENIED", "管理操作来源或 CSRF 校验失败，请刷新页面")
		}
	}
	if s.Config.Mode == "local" {
		return "本地管理员", nil
	}
	if s.Config.IdentityHeader != "" {
		name := strings.TrimSpace(r.Header.Get(s.Config.IdentityHeader))
		if len(name) > 0 && len(name) <= 200 {
			return name, nil
		}
	}
	return "Pangolin 管理员", nil
}
func decode(w http.ResponseWriter, r *http.Request, dest any) error {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return fail(415, "JSON_REQUIRED", "请求必须使用 application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(dest); e != nil {
		var large *http.MaxBytesError
		if errors.As(e, &large) {
			return fail(413, "BODY_TOO_LARGE", "请求体超过256KB")
		}
		return fail(400, "INVALID_JSON", "请求格式无效或包含未知字段")
	}
	if e := decoder.Decode(new(any)); e != io.EOF {
		return fail(400, "INVALID_JSON", "请求必须仅包含一个 JSON 对象")
	}
	return nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := randomID("req_")
	w.Header().Set("X-Request-ID", id)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if s.Config.Mode == "pangolin" {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	defer func() {
		if p := recover(); p != nil {
			log.Printf("request panic id=%s type=%T", id, p)
			writeResponse(w, errorResponse(errors.New("panic"), id))
		}
	}()
	if strings.Contains(r.URL.EscapedPath(), "%") || strings.Contains(r.URL.Path, "//") || strings.Contains(r.URL.Path, "..") {
		writeResponse(w, errorResponse(fail(400, "INVALID_PATH", "路径格式无效"), id))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/") || r.URL.Path == "/api/v1" {
		s.external(w, r, id)
		return
	}
	admin, err := s.admin(r)
	if err != nil {
		writeResponse(w, errorResponse(err, id))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin-api/") {
		out, e := s.adminRoute(w, r, id, admin)
		if e != nil {
			out = errorResponse(e, id)
			if out.Status >= 500 {
				log.Printf("admin failure id=%s code=%d", id, out.Status)
			}
		}
		writeResponse(w, out)
		return
	}
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		writeResponse(w, ok(map[string]any{"status": "ok"}))
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		writeResponse(w, errorResponse(fail(405, "METHOD_NOT_ALLOWED", "不支持此方法"), id))
		return
	}
	path := "index.html"
	if r.URL.Path != "/" {
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			writeResponse(w, errorResponse(fail(404, "NOT_FOUND", "路径不存在"), id))
			return
		}
		path = strings.TrimPrefix(r.URL.Path, "/assets/")
	}
	if path != "index.html" && path != "app.js" && path != "style.css" {
		writeResponse(w, errorResponse(fail(404, "NOT_FOUND", "资源不存在"), id))
		return
	}
	data, e := fs.ReadFile(s.Web, path)
	if e != nil {
		writeResponse(w, errorResponse(fail(404, "NOT_FOUND", "资源不存在"), id))
		return
	}
	contentType := "text/html; charset=utf-8"
	if strings.HasSuffix(path, ".js") {
		contentType = "text/javascript; charset=utf-8"
	}
	if strings.HasSuffix(path, ".css") {
		contentType = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(200)
	if r.Method != "HEAD" {
		w.Write(data)
	}
}
func (s *Server) allowEntry() bool {
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	minute := time.Now().Unix() / 60
	if s.entryMinute != minute {
		s.entryMinute = minute
		s.entryCount = 0
	}
	s.entryCount++
	return s.entryCount <= 600
}
func (s *Server) external(w http.ResponseWriter, r *http.Request, id string) {
	if !s.allowEntry() {
		w.Header().Set("Retry-After", "60")
		writeResponse(w, errorResponse(fail(429, "ENTRY_RATE_LIMITED", "入口请求过多，请稍后重试"), id))
		return
	}
	select {
	case s.entrySlots <- struct{}{}:
		defer func() { <-s.entrySlots }()
	default:
		writeResponse(w, errorResponse(fail(503, "ENTRY_BUSY", "入口繁忙，请稍后重试"), id))
		return
	}
	start := time.Now()
	audit := Audit{ID: id, Action: "request"}
	var out response
	defer func() {
		audit.Status = out.Status
		audit.DurationMS = time.Since(start).Milliseconds()
		if audit.RecordID == "" {
			audit.RecordID = resultRecordID(out)
		}
		if audit.Result == "" {
			audit.Result = resultLabel(out)
		}
		if e := s.Store.Audit(audit); e != nil {
			log.Printf("audit failure request_id=%s", id)
		}
		if out.Status == 401 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="feishu-api"`)
		}
		if out.Status == 429 {
			w.Header().Set("Retry-After", "60")
		}
		writeResponse(w, out)
	}()
	auth := r.Header.Get("Authorization")
	if len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(auth, "Bearer ") {
		out = errorResponse(fail(401, "INVALID_TOKEN", "缺少或无效的 Bearer Token"), id)
		return
	}
	app, e := s.Store.Authenticate(strings.TrimPrefix(auth, "Bearer "))
	if app.ID != "" {
		audit.AppID = app.ID
		audit.AppName = app.Name
	}
	if e != nil {
		out = errorResponse(e, id)
		return
	}
	if e = s.Store.ConsumeRate(app.ID, s.Config.RateLimit); e != nil {
		out = errorResponse(e, id)
		return
	}
	p, key, e := parseExternal(w, r)
	if e != nil {
		out = errorResponse(e, id)
		return
	}
	audit.Action = p.Action
	audit.Table = tableName(p.Table)
	audit.RecordID = p.RecordID
	audit.FieldNames = fieldNames(p.Fields)
	audit.Reason = p.ExceptionReason
	out = s.execute(r.Context(), app, p, key, id, true)
	if !strings.HasPrefix(outcomeCode(out), "IDEMPOTENCY_") {
		audit.Result = resultLabel(out)
	}
}
func tableName(key string) string {
	if t, ok := feishu.Tables[key]; ok {
		return t.Name
	}
	return key
}
func fieldNames(fields map[string]any) []string {
	out := []string{}
	for k := range fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func outcomeCode(out response) string {
	if m, ok := out.Body.(map[string]any); ok {
		if e, ok := m["error"].(map[string]any); ok {
			if c, ok := e["code"].(string); ok {
				return c
			}
		}
	}
	return ""
}
func resultLabel(out response) string {
	if out.Status >= 400 {
		if code := outcomeCode(out); code != "" {
			return code
		}
		return "请求失败"
	}
	var envelope struct {
		Data struct {
			Verified *bool `json:"verified"`
		} `json:"data"`
	}
	_ = json.Unmarshal(jsonBytes(out.Body), &envelope)
	if envelope.Data.Verified != nil {
		if *envelope.Data.Verified {
			return "回读已核验"
		}
		return "写入已返回，回读不一致"
	}
	return "成功"
}
func resultRecordID(out response) string {
	var envelope struct {
		Data struct {
			RecordID string `json:"record_id"`
			Record   struct {
				RecordID string `json:"record_id"`
			} `json:"record"`
		} `json:"data"`
		Error struct {
			RecordID string `json:"recordId"`
		} `json:"error"`
	}
	_ = json.Unmarshal(jsonBytes(out.Body), &envelope)
	if envelope.Data.Record.RecordID != "" {
		return envelope.Data.Record.RecordID
	}
	if envelope.Data.RecordID != "" {
		return envelope.Data.RecordID
	}
	return envelope.Error.RecordID
}

type operationBody struct {
	Fields           map[string]any `json:"fields,omitempty"`
	ExpectedRevision string         `json:"expectedRevision,omitempty"`
	Filter           map[string]any `json:"filter,omitempty"`
	FieldNames       []string       `json:"fieldNames,omitempty"`
	Limit            int            `json:"limit,omitempty"`
	PageToken        string         `json:"pageToken,omitempty"`
	Reason           string         `json:"reason,omitempty"`
}

func parseExternal(w http.ResponseWriter, r *http.Request) (feishu.Params, string, error) {
	p := feishu.Params{}
	segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	if len(segments) == 1 && segments[0] == "tables" && r.Method == "GET" {
		p.Action = "tables"
		return p, "", nil
	}
	if len(segments) < 3 || segments[0] != "tables" {
		return p, "", fail(404, "NOT_FOUND", "接口不存在")
	}
	p.Table = segments[1]
	if _, ok := feishu.Tables[p.Table]; !ok {
		return p, "", fail(404, "TABLE_NOT_FOUND", "仅支持需求、任务和 BUG 三张表")
	}
	switch {
	case len(segments) == 3 && segments[2] == "fields" && r.Method == "GET":
		p.Action = "fields"
	case len(segments) == 3 && segments[2] == "records" && r.Method == "POST":
		p.Action = "create"
	case len(segments) == 4 && segments[2] == "records" && segments[3] == "search" && r.Method == "POST":
		p.Action = "search"
	case len(segments) == 4 && segments[2] == "records" && segments[3] != "search" && r.Method == "GET":
		p.Action = "get"
		p.RecordID = segments[3]
	case len(segments) == 4 && segments[2] == "records" && segments[3] != "search" && r.Method == "PATCH":
		p.Action = "update"
		p.RecordID = segments[3]
	default:
		return p, "", fail(405, "METHOD_NOT_ALLOWED", "接口或请求方法不支持")
	}
	if r.Method == "POST" || r.Method == "PATCH" {
		var body operationBody
		if e := decode(w, r, &body); e != nil {
			return p, "", e
		}
		p.Fields = body.Fields
		p.ExpectedRevision = body.ExpectedRevision
		p.Filter = body.Filter
		p.FieldNames = body.FieldNames
		p.Limit = body.Limit
		p.PageToken = body.PageToken
		p.ExceptionReason = body.Reason
	}
	return p, r.Header.Get("Idempotency-Key"), nil
}
func validateOperation(p feishu.Params) error {
	if !contains([]string{"tables", "fields", "search", "get", "create", "update"}, p.Action) {
		return fail(400, "INVALID_ACTION", "操作不存在")
	}
	if p.Action == "tables" {
		return nil
	}
	if _, ok := feishu.Tables[p.Table]; !ok {
		return fail(404, "TABLE_NOT_FOUND", "数据表不存在")
	}
	if len(p.ExceptionReason) > 2000 {
		return fail(400, "INVALID_REASON", "修改原因过长")
	}
	if p.Action == "search" {
		if p.Limit < 0 || p.Limit > 100 {
			return fail(400, "INVALID_LIMIT", "limit 必须为1至100，或省略")
		}
		if len(p.PageToken) > 2048 || len(p.FieldNames) > 100 {
			return fail(400, "INVALID_QUERY", "查询参数过长")
		}
	}
	if p.Action == "get" || p.Action == "update" {
		if !strings.HasPrefix(p.RecordID, "rec") || len(p.RecordID) < 4 || len(p.RecordID) > 100 {
			return fail(400, "INVALID_RECORD_ID", "需要有效的 rec 记录 ID")
		}
		for _, v := range p.RecordID {
			if !(v >= 'a' && v <= 'z' || v >= 'A' && v <= 'Z' || v >= '0' && v <= '9') {
				return fail(400, "INVALID_RECORD_ID", "记录 ID 格式错误")
			}
		}
	}
	if p.Action == "create" || p.Action == "update" {
		if len(p.Fields) == 0 || len(p.Fields) > 100 {
			return fail(400, "INVALID_FIELDS", "需要非空 fields 对象，最多100个字段")
		}
		if p.Table == "requirements" {
			if p.Action == "create" {
				return fail(403, "POLICY_DENIED", "需求管理不允许创建")
			}
			for key := range p.Fields {
				if key != "需求状态" {
					return fail(403, "POLICY_DENIED", "需求管理最多只能更新需求状态")
				}
			}
		}
		for key := range p.Fields {
			if (p.Table == "tasks" && key == "任务状态") || (p.Table == "requirements" && key == "需求状态") {
				return fail(422, "FIELD_READ_ONLY", "需求状态和任务状态是流程字段，飞书 API 仅支持读取")
			}
		}
	}
	if p.Action == "update" {
		if len(p.ExpectedRevision) != 64 {
			return fail(400, "REVISION_REQUIRED", "必须传入 get 返回的 expectedRevision")
		}
		for _, v := range p.ExpectedRevision {
			if !(v >= 'a' && v <= 'f' || v >= '0' && v <= '9') {
				return fail(400, "REVISION_REQUIRED", "expectedRevision 格式错误")
			}
		}
		if p.Table == "bugs" {
			for key := range p.Fields {
				if key != "BUG状态" && strings.TrimSpace(p.ExceptionReason) == "" {
					return fail(400, "REASON_REQUIRED", "更新 BUG 非状态字段必须填写 reason")
				}
			}
		}
	}
	return nil
}
func scopeFor(p feishu.Params) string {
	if p.Action == "tables" {
		return ""
	}
	if p.Action != "create" && p.Action != "update" {
		return p.Table + ":read"
	}
	switch p.Table {
	case "tasks":
		return "tasks:write"
	case "requirements":
		return "requirements:status"
	case "bugs":
		if p.Action == "create" {
			return "bugs:create"
		}
		for k := range p.Fields {
			if k != "BUG状态" {
				return "bugs:edit"
			}
		}
		return "bugs:status"
	}
	return "invalid"
}

// Write permission never grants read permission. Full upstream readback remains
// internal; even historical idempotency responses are projected before delivery.
func redactWriteResponse(out response) response {
	if out.Status >= 400 {
		return out
	}
	var envelope map[string]any
	if json.Unmarshal(jsonBytes(out.Body), &envelope) != nil {
		return errorResponse(fail(500, "RESPONSE_ERROR", "写入响应解析失败，请先核实记录"), "")
	}
	data, _ := envelope["data"].(map[string]any)
	projected := map[string]any{}
	for _, key := range []string{"action", "submittedFields", "verified", "mismatchedFields", "note", "exceptionReason"} {
		if v, ok := data[key]; ok {
			projected[key] = v
		}
	}
	rec, _ := data["record"].(map[string]any)
	if rec == nil {
		rec = data
	}
	metadata := map[string]any{}
	for _, key := range []string{"record_id", "revision"} {
		if v, ok := rec[key]; ok {
			metadata[key] = v
		}
	}
	projected["record"] = metadata
	envelope["data"] = projected
	out.Body = envelope
	return out
}
func (s *Server) execute(ctx context.Context, app App, p feishu.Params, key, id string, budgeted bool) response {
	// Current permissions apply even when replaying an idempotent request.
	current, e := s.Store.ActiveApp(app.ID)
	if e != nil {
		return errorResponse(e, id)
	}
	if !budgeted {
		if e = s.Store.ConsumeRate(app.ID, s.Config.RateLimit); e != nil {
			return errorResponse(e, id)
		}
	}
	required := scopeFor(p)
	if required != "" && !contains(current.Scopes, required) && !(required == "bugs:status" && contains(current.Scopes, "bugs:edit")) {
		return errorResponse(fail(403, "SCOPE_DENIED", "当前 Token 缺少权限："+required), id)
	}
	if e = validateOperation(p); e != nil {
		return errorResponse(e, id)
	}
	if p.Action == "tables" {
		tables := map[string]feishu.Table{}
		for k, t := range feishu.Tables {
			for _, scope := range current.Scopes {
				if strings.HasPrefix(scope, k+":") {
					tables[k] = t
				}
			}
		}
		return ok(map[string]any{"tables": tables, "platformLimit": "需求状态、任务状态为流程字段，仅支持读取。"})
	}
	isWrite := p.Action == "create" || p.Action == "update"
	if p.Action == "create" && key == "" {
		return errorResponse(fail(400, "IDEMPOTENCY_KEY_REQUIRED", "创建记录必须提供 Idempotency-Key"), id)
	}
	if key != "" {
		if !isWrite || len(key) > 128 || len(key) < 8 {
			return errorResponse(fail(400, "INVALID_IDEMPOTENCY_KEY", "写入幂等键长度须为8至128个字符"), id)
		}
		for _, c := range key {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return errorResponse(fail(400, "INVALID_IDEMPOTENCY_KEY", "幂等键只能包含字母、数字、点、横线和下划线"), id)
			}
		}
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return errorResponse(fail(503, "BUSY", "服务繁忙，请稍后重试"), id)
	}
	if key != "" {
		replay, err := s.Store.BeginIdempotency(app.ID, key, digest(string(jsonBytes(p))))
		if err != nil {
			return errorResponse(err, id)
		}
		if !replay.Fresh {
			var body any
			if json.Unmarshal(replay.Body, &body) != nil {
				return errorResponse(errors.New("invalid replay"), id)
			}
			return redactWriteResponse(response{replay.Status, body})
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	result, err := s.Client.Run(ctx, p)
	out := ok(result)
	if p.Action == "create" {
		out.Status = 201
	}
	if err != nil {
		out = errorResponse(err, id)
	}
	if isWrite {
		out = redactWriteResponse(out)
	}
	if key != "" {
		if e = s.Store.FinishIdempotency(app.ID, key, out.Status, jsonBytes(out.Body)); e != nil {
			log.Printf("idempotency persistence failure request_id=%s; do not retry writes with new keys", id)
			return errorResponse(fail(503, "IDEMPOTENCY_SAVE_FAILED", "请求可能已执行，但结果保存失败。请先核实记录，不要更换 Key 重试"), id)
		}
	}
	return out
}
func parseIntQuery(r *http.Request, name string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, e := strconv.Atoi(v)
	if e != nil || n < min || n > max {
		return 0, fail(400, "INVALID_QUERY", "分页参数无效")
	}
	return n, nil
}
func (s *Server) adminRoute(w http.ResponseWriter, r *http.Request, id, admin string) (response, error) {
	path := strings.TrimPrefix(r.URL.Path, "/admin-api/")
	if r.Method == "GET" {
		switch path {
		case "session":
			return ok(map[string]any{"csrfToken": s.csrf, "authMode": s.Config.Mode, "adminName": admin, "publicOrigin": s.Config.PublicOrigin}), nil
		case "overview":
			v, e := s.Store.Overview()
			return ok(v), e
		case "apps":
			v, e := s.Store.ListApps()
			return ok(v), e
		case "resources":
			return ok(map[string]any{"tables": feishu.Tables, "platformLimit": "需求状态和任务状态是流程字段(type=24)，飞书 API 仅支持读取；BUG状态可写。"}), nil
		case "logs":
			limit, e := parseIntQuery(r, "limit", 50, 1, 500)
			if e != nil {
				return response{}, e
			}
			offset, e := parseIntQuery(r, "offset", 0, 0, 10000000)
			if e != nil {
				return response{}, e
			}
			result := r.URL.Query().Get("result")
			if result != "" && !contains([]string{"all", "success", "failure"}, result) {
				return response{}, fail(400, "INVALID_QUERY", "结果筛选无效")
			}
			q := r.URL.Query().Get("q")
			if len(q) > 300 {
				return response{}, fail(400, "INVALID_QUERY", "搜索内容过长")
			}
			v, e := s.Store.Logs(limit, offset, result, q)
			return ok(v), e
		case "settings":
			info := s.credentials.Info()
			return ok(map[string]any{"authMode": s.Config.Mode, "publicOrigin": s.Config.PublicOrigin, "rateLimitPerMinute": s.Config.RateLimit, "credentialConfigured": info.SecretConfigured, "feishu": info, "version": "0.1.0"}), nil
		}
	}
	if path == "feishu-credentials" && r.Method == "POST" {
		var input FeishuCredentialInput
		if e := decode(w, r, &input); e != nil {
			return response{}, e
		}
		s.credentialSaveMu.Lock()
		defer s.credentialSaveMu.Unlock()
		info, e := s.credentials.Save(input)
		if e != nil {
			return response{}, e
		}
		if invalidator, ok := s.Client.(interface{ InvalidateCredentials() }); ok {
			invalidator.InvalidateCredentials()
		}
		s.adminAudit(id, admin, "settings.feishu.save", "", []string{"app_id", "app_secret"})
		return ok(info), nil
	}
	if path == "apps" && r.Method == "POST" {
		var input AppInput
		if e := decode(w, r, &input); e != nil {
			return response{}, e
		}
		v, e := s.Store.CreateApp(input)
		if e == nil {
			s.adminAudit(id, admin, "app.create", v.App.ID, []string{"name", "scopes", "expiresAt"})
		}
		return response{201, map[string]any{"data": v}}, e
	}
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && parts[0] == "apps" {
		if len(parts) == 2 && r.Method == "PATCH" {
			var input AppInput
			if e := decode(w, r, &input); e != nil {
				return response{}, e
			}
			v, e := s.Store.UpdateApp(parts[1], input)
			if e == nil {
				s.adminAudit(id, admin, "app.update", v.ID, []string{"application settings"})
			}
			return ok(v), e
		}
		if len(parts) == 3 && parts[2] == "rotate" && r.Method == "POST" {
			var empty struct{}
			if e := decode(w, r, &empty); e != nil {
				return response{}, e
			}
			v, e := s.Store.Rotate(parts[1])
			if e == nil {
				s.adminAudit(id, admin, "app.rotate", v.App.ID, []string{"token"})
			}
			return ok(v), e
		}
	}
	if path == "connection-test" && r.Method == "POST" {
		var empty struct{}
		if e := decode(w, r, &empty); e != nil {
			return response{}, e
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			return response{}, fail(503, "BUSY", "服务繁忙")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
		defer cancel()
		items := []map[string]any{}
		for _, name := range []string{"requirements", "tasks", "bugs"} {
			v, e := s.Client.Run(ctx, feishu.Params{Action: "fields", Table: name})
			if e != nil {
				return response{}, e
			}
			raw := jsonBytes(v)
			var obj struct {
				Fields []any `json:"fields"`
			}
			if e = json.Unmarshal(raw, &obj); e != nil {
				return response{}, e
			}
			items = append(items, map[string]any{"name": tableName(name), "fieldCount": len(obj.Fields)})
		}
		return ok(map[string]any{"ok": true, "tables": items}), nil
	}
	if path == "debug" && r.Method == "POST" {
		var body struct {
			AppID            string         `json:"appId"`
			Action           string         `json:"action"`
			Table            string         `json:"table"`
			RecordID         string         `json:"recordId"`
			Fields           map[string]any `json:"fields"`
			ExpectedRevision string         `json:"expectedRevision"`
			Filter           map[string]any `json:"filter"`
			FieldNames       []string       `json:"fieldNames"`
			Limit            int            `json:"limit"`
			PageToken        string         `json:"pageToken"`
			Reason           string         `json:"reason"`
			IdempotencyKey   string         `json:"idempotencyKey"`
		}
		if e := decode(w, r, &body); e != nil {
			return response{}, e
		}
		app, e := s.Store.GetApp(body.AppID)
		if e != nil {
			return response{}, e
		}
		p := feishu.Params{Action: body.Action, Table: body.Table, RecordID: body.RecordID, Fields: body.Fields, ExpectedRevision: body.ExpectedRevision, Filter: body.Filter, FieldNames: body.FieldNames, Limit: body.Limit, PageToken: body.PageToken, ExceptionReason: body.Reason}
		start := time.Now()
		out := s.execute(r.Context(), app, p, body.IdempotencyKey, id, false)
		a := Audit{ID: id, AppID: app.ID, AppName: app.Name, Action: p.Action, Table: tableName(p.Table), RecordID: p.RecordID, Status: out.Status, Result: resultLabel(out), FieldNames: fieldNames(p.Fields), Reason: p.ExceptionReason, DurationMS: time.Since(start).Milliseconds()}
		if a.RecordID == "" {
			a.RecordID = resultRecordID(out)
		}
		if e := s.Store.Audit(a); e != nil {
			log.Printf("audit failure id=%s", id)
		}
		return ok(map[string]any{"status": out.Status, "body": out.Body}), nil
	}
	return response{}, fail(404, "NOT_FOUND", "管理接口不存在或方法不支持")
}
func (s *Server) adminAudit(id, admin, action, record string, fields []string) {
	if e := s.Store.Audit(Audit{ID: id, AppName: admin, Action: action, RecordID: record, Status: 200, Result: "成功", FieldNames: fields}); e != nil {
		log.Printf("admin audit failure id=%s", id)
	}
}
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{Addr: net.JoinHostPort(s.Config.Host, strconv.Itoa(s.Config.Port)), Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 110 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
}
