package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"

	"feishu-gateway/internal/feishu"
)

type Table struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	ID             string `json:"id"`
	StatusField    string `json:"statusField"`
	Policy         string `json:"policy"`
	StatusWritable bool   `json:"statusWritable"`
}

var Tables = []Table{
	{"requirements", "需求管理", "tblAMCz7qoUVZzfH", "需求状态", "只读；最多允许更新需求状态（流程字段目前不支持 API 写入）", false},
	{"tasks", "任务管理", "tblJsfSnouoWxazc", "任务状态", "可创建和更新；任务状态为流程字段，飞书 API 目前不支持写入", false},
	{"bugs", "BUG管理", "tblyOWQEWoKU8tEd", "BUG状态", "可创建、更新状态；其他字段需 bugs:edit 权限", true},
}

type Upstream interface {
	Do(context.Context, feishu.Credentials, string, string, url.Values, []byte) (feishu.Result, error)
}
type Server struct {
	store  *Store
	client Upstream
	web    fs.FS
	mux    *http.ServeMux
}

func New(store *Store, client Upstream, web fs.FS) *Server {
	s := &Server{store: store, client: client, web: web, mux: http.NewServeMux()}
	s.routes()
	return s
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func failResponse(w http.ResponseWriter, e error) {
	status, message := 500, "服务内部错误"
	var f *Fault
	body := map[string]any{}
	if errors.As(e, &f) {
		status, message = f.Status, f.Message
	} else {
		var transport *feishu.TransportError
		if errors.As(e, &transport) {
			status, message = 502, transport.Message
			body["stage"] = transport.Stage
			body["responseReceived"] = transport.ResponseReceived
			if transport.Status != 0 {
				body["upstreamHttpStatus"] = transport.Status
			}
		} else {
			log.Printf("internal error type=%T", e)
		}
	}
	body["message"] = message
	jsonResponse(w, status, map[string]any{"error": body})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return fault(415, "请使用 application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return fault(400, "JSON 请求格式错误或内容过大")
	}
	if d.Decode(new(any)) != io.EOF {
		return fault(400, "请求必须是单个 JSON 值")
	}
	return nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Pangolin + the existing firewall are the sole administrator admission layer.
	// A custom header blocks cross-site form/fetch writes without a second login protocol.
	if strings.HasPrefix(r.URL.Path, "/admin-api/") && r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Requested-With") != "FeishuGateway" {
		failResponse(w, fault(403, "管理请求缺少 X-Requested-With"))
		return
	}
	s.mux.ServeHTTP(w, r)
}
func (s *Server) admin(pattern string, fn func(http.ResponseWriter, *http.Request) (any, error)) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		data, e := fn(w, r)
		if e != nil {
			failResponse(w, e)
			return
		}
		jsonResponse(w, 200, map[string]any{"data": data})
	})
}
func (s *Server) routes() {
	s.admin("GET /admin-api/overview", func(http.ResponseWriter, *http.Request) (any, error) { return s.store.Overview() })
	s.admin("GET /admin-api/settings", func(http.ResponseWriter, *http.Request) (any, error) { return s.store.Settings() })
	s.admin("POST /admin-api/settings", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			AppID     string `json:"appId"`
			AppSecret string `json:"appSecret"`
		}
		if e := decode(w, r, &in); e != nil {
			return nil, e
		}
		if e := s.store.SaveCredentials(in.AppID, in.AppSecret); e != nil {
			return nil, e
		}
		s.audit(Application{Name: "管理员"}, "settings.save", "", 200, "凭据已保存", "")
		return s.store.Settings()
	})
	s.admin("GET /admin-api/apps", func(http.ResponseWriter, *http.Request) (any, error) { return s.store.Apps() })
	s.admin("POST /admin-api/apps", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in AppInput
		if e := decode(w, r, &in); e != nil {
			return nil, e
		}
		v, e := s.store.Create(in)
		if e == nil {
			s.audit(Application{Name: "管理员"}, "app.create", "", 200, "应用已创建", "")
		}
		return v, e
	})
	s.admin("PATCH /admin-api/apps/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in AppInput
		if e := decode(w, r, &in); e != nil {
			return nil, e
		}
		v, e := s.store.Update(r.PathValue("id"), in)
		if e == nil {
			s.audit(Application{Name: "管理员"}, "app.update", "", 200, "应用配置已更新", "")
		}
		return v, e
	})
	s.admin("POST /admin-api/apps/{id}/rotate", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct{}
		if e := decode(w, r, &in); e != nil {
			return nil, e
		}
		v, e := s.store.Rotate(r.PathValue("id"))
		if e == nil {
			s.audit(Application{Name: "管理员"}, "app.rotate", "", 200, "Token 已轮换", "")
		}
		return v, e
	})
	s.admin("GET /admin-api/tables", func(http.ResponseWriter, *http.Request) (any, error) { return Tables, nil })
	s.admin("GET /admin-api/logs", func(http.ResponseWriter, *http.Request) (any, error) { return s.store.Logs() })
	s.admin("POST /admin-api/debug", s.debug)
	s.mux.HandleFunc("/api/v1/", s.publicAPI)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, map[string]bool{"ok": true}) })
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := "index.html"
		if r.URL.Path != "/" {
			switch r.URL.Path {
			case "/assets/app.js":
				name = "app.js"
			case "/assets/style.css":
				name = "style.css"
			default:
				http.NotFound(w, r)
				return
			}
		}
		b, e := fs.ReadFile(s.web, name)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		content := "text/html; charset=utf-8"
		if name == "app.js" {
			content = "text/javascript; charset=utf-8"
		}
		if name == "style.css" {
			content = "text/css; charset=utf-8"
		}
		w.Header().Set("Content-Type", content)
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
}

type Operation struct {
	Table    string
	Action   string
	RecordID string
	Query    url.Values
	Body     []byte
}

func table(key string) (Table, error) {
	for _, t := range Tables {
		if t.Key == key {
			return t, nil
		}
	}
	return Table{}, fault(404, "数据表不存在")
}
func authorize(a Application, op Operation) (Table, error) {
	t, e := table(op.Table)
	if e != nil {
		return t, e
	}
	scope := op.Table + ":read"
	switch op.Action {
	case "fields", "search", "get":
	case "create", "update":
		var body struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		if json.Unmarshal(op.Body, &body) != nil || len(body.Fields) == 0 {
			return t, fault(400, "写入需要非空 fields 对象")
		}
		switch op.Table {
		case "requirements":
			if op.Action == "create" {
				return t, fault(403, "需求管理不能创建记录")
			}
			for key := range body.Fields {
				if key != "需求状态" {
					return t, fault(403, "需求管理只能更新需求状态")
				}
			}
			scope = "requirements:status"
		case "tasks":
			scope = "tasks:write"
		case "bugs":
			scope = "bugs:create"
			if op.Action == "update" {
				scope = "bugs:status"
				for key := range body.Fields {
					if key != "BUG状态" {
						scope = "bugs:edit"
					}
				}
			}
		}
	default:
		return t, fault(400, "操作不存在")
	}
	if !has(a.Scopes, scope) && !(scope == "bugs:status" && has(a.Scopes, "bugs:edit")) {
		return t, fault(403, "缺少权限："+scope)
	}
	return t, nil
}
func (s *Server) audit(a Application, action, table string, status int, result, requestID string) {
	if e := s.store.Log(a, action, table, status, result, requestID); e != nil {
		log.Printf("operation log could not be stored")
	}
}
func (s *Server) execute(ctx context.Context, a Application, op Operation) (feishu.Result, error) {
	t, e := authorize(a, op)
	if e != nil {
		s.audit(a, op.Action, op.Table, errorStatus(e), e.Error(), "")
		return feishu.Result{}, e
	}
	c, e := s.store.Credentials()
	if e != nil {
		return feishu.Result{}, e
	}
	method, path := "GET", "/tables/"+t.ID
	switch op.Action {
	case "fields":
		path += "/fields"
	case "search":
		method = "POST"
		path += "/records/search"
	case "create":
		method = "POST"
		path += "/records"
	case "get", "update":
		if op.RecordID == "" {
			return feishu.Result{}, fault(400, "需要记录 ID")
		}
		path += "/records/" + url.PathEscape(op.RecordID)
		if op.Action == "update" {
			method = "PUT"
		}
	}
	result, e := s.client.Do(ctx, c, method, path, op.Query, op.Body)
	if e != nil {
		s.audit(a, op.Action, t.Name, errorStatus(e), "未取得完整上游响应", "")
		return result, e
	}
	// Preserve actual upstream status and business code, including HTTP 200 rejections.
	var message struct {
		Code any `json:"code"`
	}
	_ = json.Unmarshal(result.Body, &message)
	s.audit(a, op.Action, t.Name, result.Status, fmt.Sprintf("%s · code=%v", result.Stage, message.Code), result.RequestID)
	if (op.Action == "create" || op.Action == "update") && !has(a.Scopes, op.Table+":read") && result.Stage == "operation" {
		result.Body = writeOnlyFields(result.Body, op.Body)
	}
	return result, nil
}
func errorStatus(e error) int {
	var f *Fault
	if errors.As(e, &f) {
		return f.Status
	}
	return 502
}

// Status/code/message stay unchanged. Write-only tokens do not gain unrelated fields.
func writeOnlyFields(body, submitted []byte) []byte {
	var response map[string]json.RawMessage
	var input struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if json.Unmarshal(body, &response) != nil || string(response["code"]) != "0" || json.Unmarshal(submitted, &input) != nil {
		return body
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(response["data"], &data) != nil {
		return body
	}
	var record map[string]json.RawMessage
	if json.Unmarshal(data["record"], &record) != nil {
		return body
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(record["fields"], &fields)
	kept := map[string]json.RawMessage{}
	for key := range input.Fields {
		if v, ok := fields[key]; ok {
			kept[key] = v
		}
	}
	filtered := map[string]any{"fields": kept}
	if id, ok := record["record_id"]; ok {
		filtered["record_id"] = id
	}
	b, e := json.Marshal(filtered)
	if e != nil {
		return body
	}
	data["record"] = b
	response["data"], _ = json.Marshal(data)
	b, e = json.Marshal(response)
	if e != nil {
		return body
	}
	return b
}
func (s *Server) publicAPI(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		failResponse(w, fault(401, "需要 Bearer Token"))
		return
	}
	a, e := s.store.Authenticate(strings.TrimPrefix(auth, "Bearer "))
	if e != nil {
		failResponse(w, e)
		return
	}
	if r.URL.Path == "/api/v1/tables" && r.Method == "GET" {
		tables := []Table{}
		for _, t := range Tables {
			for _, scope := range a.Scopes {
				if strings.HasPrefix(scope, t.Key+":") {
					tables = append(tables, t)
					break
				}
			}
		}
		jsonResponse(w, 200, map[string]any{"data": tables})
		return
	}
	op, e := parseOperation(w, r)
	if e != nil {
		failResponse(w, e)
		return
	}
	result, e := s.execute(r.Context(), a, op)
	if e != nil {
		failResponse(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Upstream-Stage", result.Stage)
	if result.RequestID != "" {
		w.Header().Set("X-Feishu-Request-ID", result.RequestID)
	}
	if (op.Action == "create" || op.Action == "update") && !has(a.Scopes, op.Table+":read") {
		w.Header().Set("X-Response-Fields", "submitted-only")
	}
	w.WriteHeader(result.Status)
	w.Write(result.Body)
}
func parseOperation(w http.ResponseWriter, r *http.Request) (Operation, error) {
	op := Operation{Query: r.URL.Query()}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	if len(p) < 3 || p[0] != "tables" {
		return op, fault(404, "接口不存在")
	}
	op.Table = p[1]
	switch {
	case len(p) == 3 && p[2] == "fields" && r.Method == "GET":
		op.Action = "fields"
	case len(p) == 3 && p[2] == "records" && r.Method == "POST":
		op.Action = "create"
	case len(p) == 4 && p[2] == "records" && p[3] == "search" && r.Method == "POST":
		op.Action = "search"
	case len(p) == 4 && p[2] == "records" && p[3] != "search" && r.Method == "GET":
		op.Action = "get"
		op.RecordID = p[3]
	case len(p) == 4 && p[2] == "records" && p[3] != "search" && (r.Method == "PATCH" || r.Method == "PUT"):
		op.Action = "update"
		op.RecordID = p[3]
	default:
		return op, fault(405, "接口或请求方法不支持")
	}
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
		var body map[string]json.RawMessage
		if e := decode(w, r, &body); e != nil {
			return op, e
		}
		op.Body, _ = json.Marshal(body)
	}
	return op, nil
}
func (s *Server) debug(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		AppID     string                     `json:"appId"`
		Table     string                     `json:"table"`
		Action    string                     `json:"action"`
		RecordID  string                     `json:"recordId"`
		Payload   map[string]json.RawMessage `json:"payload"`
		PageSize  int                        `json:"pageSize"`
		PageToken string                     `json:"pageToken"`
	}
	if e := decode(w, r, &in); e != nil {
		return nil, e
	}
	a, e := s.store.Active(in.AppID)
	if e != nil {
		return nil, e
	}
	q := url.Values{}
	if in.PageSize > 0 {
		q.Set("page_size", fmt.Sprint(in.PageSize))
	}
	if in.PageToken != "" {
		q.Set("page_token", in.PageToken)
	}
	var body []byte
	if in.Payload != nil {
		body, _ = json.Marshal(in.Payload)
	} else if in.Action == "search" {
		body = []byte("{}")
	}
	result, e := s.execute(r.Context(), a, Operation{Table: in.Table, Action: in.Action, RecordID: in.RecordID, Query: q, Body: body})
	if e != nil {
		return nil, e
	}
	var resultBody any
	if json.Unmarshal(result.Body, &resultBody) != nil {
		resultBody = string(result.Body)
	}
	return map[string]any{"httpStatus": result.Status, "stage": result.Stage, "requestId": result.RequestID, "body": resultBody}, nil
}
