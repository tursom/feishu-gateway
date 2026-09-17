package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type APIError struct {
	Status        int
	Code, Message string
}

func (e *APIError) Error() string                 { return e.Message }
func fail(status int, code, message string) error { return &APIError{status, code, message} }

var Scopes = []string{"requirements:read", "requirements:status", "tasks:read", "tasks:write", "bugs:read", "bugs:create", "bugs:status", "bugs:edit"}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func secureEqual(a, b string) bool {
	x := sha256.Sum256([]byte(a))
	y := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func randomID(prefix string) string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:])
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

type App struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	TokenPrefix string   `json:"tokenPrefix"`
	Scopes      []string `json:"scopes"`
	Enabled     bool     `json:"enabled"`
	ExpiresAt   *string  `json:"expiresAt"`
	LastUsedAt  *string  `json:"lastUsedAt"`
	CreatedAt   string   `json:"createdAt"`
}
type AppInput struct {
	Name        *string         `json:"name,omitempty"`
	Description *string         `json:"description,omitempty"`
	Scopes      *[]string       `json:"scopes,omitempty"`
	ExpiresAt   json.RawMessage `json:"expiresAt,omitempty"`
	Enabled     *bool           `json:"enabled,omitempty"`
}
type TokenResult struct {
	App   App    `json:"app"`
	Token string `json:"token"`
}
type Store struct {
	DB          *sql.DB
	auditWrites atomic.Uint64
}

func OpenStore(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return nil, e
		}
		f.Close()
		if e = os.Chmod(path, 0600); e != nil {
			return nil, e
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS apps(id TEXT PRIMARY KEY,name TEXT UNIQUE NOT NULL,description TEXT NOT NULL,token_hash TEXT UNIQUE NOT NULL,token_prefix TEXT NOT NULL,scopes TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,expires_at TEXT,last_used_at TEXT,created_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS audit(id TEXT PRIMARY KEY,time TEXT NOT NULL,app_id TEXT,app_name TEXT NOT NULL,action TEXT NOT NULL,table_name TEXT NOT NULL,record_id TEXT,status INTEGER NOT NULL,result TEXT NOT NULL,field_names TEXT NOT NULL,reason TEXT NOT NULL,duration_ms INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS audit_time ON audit(time);
 CREATE TABLE IF NOT EXISTS idempotency(app_id TEXT NOT NULL,key TEXT NOT NULL,fingerprint TEXT NOT NULL,state TEXT NOT NULL,status INTEGER,response TEXT,created_at TEXT NOT NULL,PRIMARY KEY(app_id,key));
 CREATE TABLE IF NOT EXISTS rate_buckets(app_id TEXT PRIMARY KEY,minute INTEGER NOT NULL,count INTEGER NOT NULL);
 PRAGMA user_version=1;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	store := &Store{DB: db}
	if err = store.PurgeAudit(50000); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}
func (s *Store) Close() error { return s.DB.Close() }

type scanner interface{ Scan(...any) error }

func scanApp(row scanner) (App, error) {
	var a App
	var scopes string
	var expiry, last sql.NullString
	err := row.Scan(&a.ID, &a.Name, &a.Description, &a.TokenPrefix, &scopes, &a.Enabled, &expiry, &last, &a.CreatedAt)
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal([]byte(scopes), &a.Scopes); err != nil {
		return a, err
	}
	if expiry.Valid {
		a.ExpiresAt = &expiry.String
	}
	if last.Valid {
		a.LastUsedAt = &last.String
	}
	return a, nil
}

const appCols = "id,name,description,token_prefix,scopes,enabled,expires_at,last_used_at,created_at"

func (s *Store) GetApp(id string) (App, error) {
	a, e := scanApp(s.DB.QueryRow("SELECT "+appCols+" FROM apps WHERE id=?", id))
	if errors.Is(e, sql.ErrNoRows) {
		return a, fail(404, "APP_NOT_FOUND", "应用不存在")
	}
	return a, e
}
func (s *Store) ListApps() ([]App, error) {
	rows, e := s.DB.Query("SELECT " + appCols + " FROM apps ORDER BY created_at DESC")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []App{}
	for rows.Next() {
		a, e := scanApp(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func mergeInput(a App, in AppInput, create bool) (App, error) {
	if create && in.Enabled != nil {
		return a, fail(400, "INVALID_APP", "创建时不能传 enabled")
	}
	if create && in.Name == nil {
		return a, fail(400, "INVALID_APP", "应用名称必填")
	}
	if in.Name != nil {
		a.Name = strings.TrimSpace(*in.Name)
		if a.Name == "" || len([]rune(a.Name)) > 80 {
			return a, fail(400, "INVALID_APP", "应用名称必填，最多80字符")
		}
	}
	if in.Description != nil {
		if len([]rune(*in.Description)) > 500 {
			return a, fail(400, "INVALID_APP", "用途说明最多500字符")
		}
		a.Description = *in.Description
	}
	if create && in.Scopes == nil {
		return a, fail(400, "INVALID_SCOPE", "请选择有效权限")
	}
	if in.Scopes != nil {
		a.Scopes = []string{}
		for _, scope := range *in.Scopes {
			if !contains(Scopes, scope) {
				return a, fail(400, "INVALID_SCOPE", "包含无效权限")
			}
			if !contains(a.Scopes, scope) {
				a.Scopes = append(a.Scopes, scope)
			}
		}
		if len(a.Scopes) == 0 {
			return a, fail(400, "INVALID_SCOPE", "至少选择一个权限")
		}
	}
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	if len(in.ExpiresAt) > 0 {
		var exp *string
		if e := json.Unmarshal(in.ExpiresAt, &exp); e != nil {
			return a, fail(400, "INVALID_EXPIRY", "expiresAt 必须是 ISO 日期或 null")
		}
		if exp != nil {
			t, e := time.Parse(time.RFC3339, *exp)
			if e != nil || !t.After(time.Now()) {
				return a, fail(400, "INVALID_EXPIRY", "有效期必须晚于当前时间")
			}
			v := t.UTC().Format(time.RFC3339)
			exp = &v
		}
		a.ExpiresAt = exp
	}
	return a, nil
}
func (s *Store) CreateApp(in AppInput) (TokenResult, error) {
	a, e := mergeInput(App{ID: randomID("app_"), Enabled: true, CreatedAt: now()}, in, true)
	if e != nil {
		return TokenResult{}, e
	}
	token := randomID("fsg_")
	a.TokenPrefix = token[:12] + "…"
	sc, _ := json.Marshal(a.Scopes)
	_, e = s.DB.Exec("INSERT INTO apps(id,name,description,token_hash,token_prefix,scopes,enabled,expires_at,created_at) VALUES(?,?,?,?,?,?,1,?,?)", a.ID, a.Name, a.Description, digest(token), a.TokenPrefix, string(sc), a.ExpiresAt, a.CreatedAt)
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE") {
			return TokenResult{}, fail(409, "APP_EXISTS", "应用名称已存在")
		}
		return TokenResult{}, e
	}
	return TokenResult{a, token}, nil
}
func (s *Store) UpdateApp(id string, in AppInput) (App, error) {
	tx, e := s.DB.Begin()
	if e != nil {
		return App{}, e
	}
	defer tx.Rollback()
	a, e := scanApp(tx.QueryRow("SELECT "+appCols+" FROM apps WHERE id=?", id))
	if errors.Is(e, sql.ErrNoRows) {
		return a, fail(404, "APP_NOT_FOUND", "应用不存在")
	}
	if e != nil {
		return a, e
	}
	a, e = mergeInput(a, in, false)
	if e != nil {
		return a, e
	}
	sc, _ := json.Marshal(a.Scopes)
	_, e = tx.Exec("UPDATE apps SET name=?,description=?,scopes=?,enabled=?,expires_at=? WHERE id=?", a.Name, a.Description, string(sc), a.Enabled, a.ExpiresAt, id)
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE") {
			return a, fail(409, "APP_EXISTS", "应用名称已存在")
		}
		return a, e
	}
	return a, tx.Commit()
}
func (s *Store) Rotate(id string) (TokenResult, error) {
	a, e := s.GetApp(id)
	if e != nil {
		return TokenResult{}, e
	}
	token := randomID("fsg_")
	a.TokenPrefix = token[:12] + "…"
	_, e = s.DB.Exec("UPDATE apps SET token_hash=?,token_prefix=? WHERE id=?", digest(token), a.TokenPrefix, id)
	return TokenResult{a, token}, e
}
func (s *Store) ActiveApp(id string) (App, error) {
	a, e := s.GetApp(id)
	if e != nil {
		return a, e
	}
	if !a.Enabled {
		return a, fail(403, "TOKEN_DISABLED", "应用 Token 已禁用")
	}
	if a.ExpiresAt != nil {
		t, e := time.Parse(time.RFC3339, *a.ExpiresAt)
		if e != nil || !t.After(time.Now()) {
			return a, fail(403, "TOKEN_EXPIRED", "应用 Token 已过期")
		}
	}
	return a, nil
}
func (s *Store) Authenticate(token string) (App, error) {
	var empty App
	if !strings.HasPrefix(token, "fsg_") || len(token) != 47 {
		return empty, fail(401, "INVALID_TOKEN", "缺少或无效的 Bearer Token")
	}
	var id string
	e := s.DB.QueryRow("SELECT id FROM apps WHERE token_hash=?", digest(token)).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return empty, fail(401, "INVALID_TOKEN", "缺少或无效的 Bearer Token")
	}
	if e != nil {
		return empty, e
	}
	return s.ActiveApp(id)
}
func (s *Store) ConsumeRate(id string, max int) error {
	minute := time.Now().Unix() / 60
	var count int
	e := s.DB.QueryRow(`INSERT INTO rate_buckets(app_id,minute,count) VALUES(?,?,1) ON CONFLICT(app_id) DO UPDATE SET minute=excluded.minute,count=CASE WHEN minute=excluded.minute THEN count+1 ELSE 1 END RETURNING count`, id, minute).Scan(&count)
	if e != nil {
		return e
	}
	if count > max {
		return fail(429, "RATE_LIMITED", "请求过于频繁，请稍后重试")
	}
	_, e = s.DB.Exec("UPDATE apps SET last_used_at=? WHERE id=?", now(), id)
	return e
}

type Replay struct {
	Fresh  bool
	Status int
	Body   json.RawMessage
}

func (s *Store) BeginIdempotency(app, key, fingerprint string) (Replay, error) {
	r, e := s.DB.Exec("INSERT OR IGNORE INTO idempotency(app_id,key,fingerprint,state,created_at) VALUES(?,?,?,'pending',?)", app, key, fingerprint, now())
	if e != nil {
		return Replay{}, e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return Replay{}, e
	}
	if n > 0 {
		return Replay{Fresh: true}, nil
	}
	var fp, state string
	var status sql.NullInt64
	var body sql.NullString
	e = s.DB.QueryRow("SELECT fingerprint,state,status,response FROM idempotency WHERE app_id=? AND key=?", app, key).Scan(&fp, &state, &status, &body)
	if e != nil {
		return Replay{}, e
	}
	if fp != fingerprint {
		return Replay{}, fail(409, "IDEMPOTENCY_CONFLICT", "同一 Idempotency-Key 已用于不同请求")
	}
	if state != "done" {
		return Replay{}, fail(409, "IDEMPOTENCY_PENDING", "原请求仍在执行或结果待核实；不要更换 Key 重复创建，请先查询记录和日志")
	}
	return Replay{Status: int(status.Int64), Body: json.RawMessage(body.String)}, nil
}
func (s *Store) FinishIdempotency(app, key string, status int, body []byte) error {
	_, e := s.DB.Exec("UPDATE idempotency SET state='done',status=?,response=? WHERE app_id=? AND key=?", status, string(body), app, key)
	return e
}

type Audit struct {
	ID         string   `json:"id"`
	Time       string   `json:"time"`
	AppID      string   `json:"-"`
	AppName    string   `json:"appName"`
	Action     string   `json:"action"`
	Table      string   `json:"table"`
	RecordID   string   `json:"recordId"`
	Status     int      `json:"status"`
	Result     string   `json:"result"`
	FieldNames []string `json:"fieldNames"`
	Reason     string   `json:"reason"`
	DurationMS int64    `json:"durationMs"`
}

func clipped(v string, max int) string {
	if len(v) <= max {
		return v
	}
	v = v[:max]
	for !utf8.ValidString(v) {
		v = v[:len(v)-1]
	}
	return v
}

// Retention never removes idempotency keys; uncertain writes must not be retried.
func (s *Store) PurgeAudit(maxRows int) error {
	if _, e := s.DB.Exec("DELETE FROM audit WHERE time<?", time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)); e != nil {
		return e
	}
	_, e := s.DB.Exec("DELETE FROM audit WHERE rowid IN (SELECT rowid FROM audit ORDER BY rowid DESC LIMIT -1 OFFSET ?)", maxRows)
	return e
}
func (s *Store) Audit(a Audit) error {
	a.AppID = clipped(a.AppID, 128)
	a.AppName = clipped(a.AppName, 200)
	a.Action = clipped(a.Action, 64)
	a.Table = clipped(a.Table, 128)
	a.RecordID = clipped(a.RecordID, 128)
	a.Result = clipped(a.Result, 256)
	a.Reason = clipped(a.Reason, 1024)
	fieldsOut := []string{}
	size := 0
	for _, field := range a.FieldNames {
		field = clipped(field, 128)
		if len(fieldsOut) >= 32 || size+len(field) > 2048 {
			break
		}
		fieldsOut = append(fieldsOut, field)
		size += len(field)
	}
	a.FieldNames = fieldsOut
	if a.Time == "" {
		a.Time = now()
	}
	if a.AppName == "" {
		a.AppName = "未认证调用方"
	}
	if a.FieldNames == nil {
		a.FieldNames = []string{}
	}
	fields, _ := json.Marshal(a.FieldNames)
	_, e := s.DB.Exec("INSERT INTO audit VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", a.ID, a.Time, a.AppID, a.AppName, a.Action, a.Table, a.RecordID, a.Status, a.Result, string(fields), a.Reason, a.DurationMS)
	if e == nil && s.auditWrites.Add(1)%128 == 0 {
		return s.PurgeAudit(50000)
	}
	return e
}

type LogPage struct {
	Items []Audit `json:"items"`
	Total int     `json:"total"`
}

func (s *Store) Logs(limit, offset int, result, q string) (LogPage, error) {
	where := []string{}
	args := []any{}
	if result == "success" {
		where = append(where, "status<400")
	} else if result == "failure" {
		where = append(where, "status>=400")
	}
	if q != "" {
		where = append(where, "(instr(lower(app_name),lower(?))>0 OR instr(lower(table_name),lower(?))>0 OR instr(lower(id),lower(?))>0 OR instr(lower(action),lower(?))>0)")
		args = append(args, q, q, q, q)
	}
	suffix := ""
	if len(where) > 0 {
		suffix = " WHERE " + strings.Join(where, " AND ")
	}
	out := LogPage{Items: []Audit{}}
	if e := s.DB.QueryRow("SELECT count(*) FROM audit"+suffix, args...).Scan(&out.Total); e != nil {
		return out, e
	}
	args = append(args, limit, offset)
	rows, e := s.DB.Query("SELECT * FROM audit"+suffix+" ORDER BY time DESC,rowid DESC LIMIT ? OFFSET ?", args...)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var a Audit
		var fields string
		var app, record sql.NullString
		e = rows.Scan(&a.ID, &a.Time, &app, &a.AppName, &a.Action, &a.Table, &record, &a.Status, &a.Result, &fields, &a.Reason, &a.DurationMS)
		if e != nil {
			return out, e
		}
		a.AppID = app.String
		a.RecordID = record.String
		if e = json.Unmarshal([]byte(fields), &a.FieldNames); e != nil {
			return out, e
		}
		out.Items = append(out.Items, a)
	}
	return out, rows.Err()
}
func (s *Store) Overview() (map[string]any, error) {
	today := time.Now().UTC().Format("2006-01-02")
	var count, success int
	e := s.DB.QueryRow("SELECT count(*),coalesce(sum(CASE WHEN status<400 THEN 1 ELSE 0 END),0) FROM audit WHERE time>=? AND action IN ('request','search','get','fields','tables','create','update')", today).Scan(&count, &success)
	if e != nil {
		return nil, e
	}
	trend := []map[string]any{}
	for ago := 6; ago >= 0; ago-- {
		day := time.Now().UTC().AddDate(0, 0, -ago).Format("2006-01-02")
		var read, write int
		e = s.DB.QueryRow("SELECT coalesce(sum(CASE WHEN action IN ('search','get','fields','tables') THEN 1 ELSE 0 END),0),coalesce(sum(CASE WHEN action IN ('create','update') THEN 1 ELSE 0 END),0) FROM audit WHERE substr(time,1,10)=?", day).Scan(&read, &write)
		if e != nil {
			return nil, e
		}
		trend = append(trend, map[string]any{"day": day, "read": read, "write": write})
	}
	apps, e := s.ListApps()
	if e != nil {
		return nil, e
	}
	active := 0
	for _, a := range apps {
		if a.Enabled {
			if a.ExpiresAt != nil {
				t, e := time.Parse(time.RFC3339, *a.ExpiresAt)
				if e != nil || !t.After(time.Now()) {
					continue
				}
			}
			active++
		}
	}
	var rate any
	if count > 0 {
		rate = float64(success) * 100 / float64(count)
	}
	logs, e := s.Logs(5, 0, "all", "")
	if e != nil {
		return nil, e
	}
	return map[string]any{"requestsToday": count, "successRate": rate, "activeApps": active, "totalApps": len(apps), "tables": 3, "trend": trend, "recentLogs": logs.Items}, nil
}
