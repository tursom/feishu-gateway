package app

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"feishu-gateway/internal/feishu"
	_ "modernc.org/sqlite"
)

type Fault struct {
	Status  int
	Message string
}

func (e *Fault) Error() string               { return e.Message }
func fault(status int, message string) error { return &Fault{status, message} }
func random(prefix string) string {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func timestamp() string    { return time.Now().UTC().Format(time.RFC3339Nano) }
func has(items []string, value string) bool {
	for _, s := range items {
		if s == value {
			return true
		}
	}
	return false
}

var Scopes = []string{"requirements:read", "requirements:status", "tasks:read", "tasks:write", "bugs:read", "bugs:create", "bugs:status", "bugs:edit"}

type Application struct {
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
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	Scopes      *[]string       `json:"scopes"`
	Enabled     *bool           `json:"enabled"`
	ExpiresAt   json.RawMessage `json:"expiresAt"`
}
type Token struct {
	App   Application `json:"app"`
	Token string      `json:"token"`
}
type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return nil, e
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return nil, e
		}
		f.Close()
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	// Preserve the existing apps/audit schema so deployments keep their tokens and history.
	_, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS apps(id TEXT PRIMARY KEY,name TEXT UNIQUE NOT NULL,description TEXT NOT NULL,token_hash TEXT UNIQUE NOT NULL,token_prefix TEXT NOT NULL,scopes TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,expires_at TEXT,last_used_at TEXT,created_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS audit(id TEXT PRIMARY KEY,time TEXT NOT NULL,app_id TEXT,app_name TEXT NOT NULL,action TEXT NOT NULL,table_name TEXT NOT NULL,record_id TEXT,status INTEGER NOT NULL,result TEXT NOT NULL,field_names TEXT NOT NULL,reason TEXT NOT NULL,duration_ms INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS feishu_settings(id INTEGER PRIMARY KEY CHECK(id=1),app_id TEXT NOT NULL,app_secret TEXT NOT NULL);`)
	if e != nil {
		db.Close()
		return nil, e
	}
	s := &Store{db: db}
	// Import credentials saved by the previous release without altering the source file.
	if path != ":memory:" {
		var count int
		if e = db.QueryRow("SELECT count(*) FROM feishu_settings").Scan(&count); e != nil {
			db.Close()
			return nil, e
		}
		if count == 0 {
			var c feishu.Credentials
			b, err := os.ReadFile(filepath.Join(filepath.Dir(path), "feishu-credentials.json"))
			if err == nil && json.Unmarshal(b, &c) == nil && c.AppID != "" && c.AppSecret != "" {
				_, e = db.Exec("INSERT INTO feishu_settings VALUES(1,?,?)", c.AppID, c.AppSecret)
				if e != nil {
					db.Close()
					return nil, e
				}
			}
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

const appColumns = "id,name,description,token_prefix,scopes,enabled,expires_at,last_used_at,created_at"

type scanner interface{ Scan(...any) error }

func readApp(row scanner) (Application, error) {
	var a Application
	var scopes string
	var expiry, last sql.NullString
	e := row.Scan(&a.ID, &a.Name, &a.Description, &a.TokenPrefix, &scopes, &a.Enabled, &expiry, &last, &a.CreatedAt)
	if e != nil {
		return a, e
	}
	e = json.Unmarshal([]byte(scopes), &a.Scopes)
	if expiry.Valid {
		a.ExpiresAt = &expiry.String
	}
	if last.Valid {
		a.LastUsedAt = &last.String
	}
	return a, e
}
func (s *Store) Apps() ([]Application, error) {
	rows, e := s.db.Query("SELECT " + appColumns + " FROM apps ORDER BY created_at DESC")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []Application{}
	for rows.Next() {
		a, e := readApp(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, a)
	}
	return items, rows.Err()
}
func (s *Store) App(id string) (Application, error) {
	a, e := readApp(s.db.QueryRow("SELECT "+appColumns+" FROM apps WHERE id=?", id))
	if errors.Is(e, sql.ErrNoRows) {
		return a, fault(404, "应用不存在")
	}
	return a, e
}
func (s *Store) Active(id string) (Application, error) {
	a, e := s.App(id)
	if e != nil {
		return a, e
	}
	if !a.Enabled {
		return a, fault(403, "应用已禁用")
	}
	if a.ExpiresAt != nil {
		expiry, e := time.Parse(time.RFC3339, *a.ExpiresAt)
		if e != nil || !expiry.After(time.Now()) {
			return a, fault(403, "Token 已过期")
		}
	}
	return a, nil
}
func (s *Store) Authenticate(token string) (Application, error) {
	var id string
	if token == "" {
		return Application{}, fault(401, "需要 Bearer Token")
	}
	e := s.db.QueryRow("SELECT id FROM apps WHERE token_hash=?", hash(token)).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return Application{}, fault(401, "Token 无效")
	}
	if e != nil {
		return Application{}, e
	}
	return s.Active(id)
}
func applyInput(a Application, in AppInput) (Application, error) {
	if in.Name != nil {
		a.Name = strings.TrimSpace(*in.Name)
	}
	if a.Name == "" || len(a.Name) > 240 {
		return a, fault(400, "请填写应用名称（最多80个汉字）")
	}
	if in.Description != nil {
		if len(*in.Description) > 1500 {
			return a, fault(400, "用途说明过长")
		}
		a.Description = *in.Description
	}
	if in.Scopes != nil {
		a.Scopes = []string{}
		for _, scope := range *in.Scopes {
			if !has(Scopes, scope) {
				return a, fault(400, "权限不存在："+scope)
			}
			if !has(a.Scopes, scope) {
				a.Scopes = append(a.Scopes, scope)
			}
		}
	}
	if len(a.Scopes) == 0 {
		return a, fault(400, "至少选择一项权限")
	}
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	if len(in.ExpiresAt) > 0 {
		var expiry *string
		if e := json.Unmarshal(in.ExpiresAt, &expiry); e != nil {
			return a, fault(400, "有效期应为 ISO 日期或 null")
		}
		if expiry != nil {
			t, e := time.Parse(time.RFC3339, *expiry)
			if e != nil || !t.After(time.Now()) {
				return a, fault(400, "有效期必须晚于当前时间")
			}
			v := t.UTC().Format(time.RFC3339)
			expiry = &v
		}
		a.ExpiresAt = expiry
	}
	return a, nil
}
func appError(e error) error {
	if e != nil && strings.Contains(e.Error(), "UNIQUE") {
		return fault(409, "应用名称已存在")
	}
	return e
}
func (s *Store) Create(in AppInput) (Token, error) {
	a, e := applyInput(Application{ID: random("app_"), Enabled: true, CreatedAt: timestamp()}, in)
	if e != nil {
		return Token{}, e
	}
	token := random("fsg_")
	a.TokenPrefix = token[:12] + "…"
	scopes, _ := json.Marshal(a.Scopes)
	_, e = s.db.Exec("INSERT INTO apps(id,name,description,token_hash,token_prefix,scopes,enabled,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)", a.ID, a.Name, a.Description, hash(token), a.TokenPrefix, string(scopes), a.Enabled, a.ExpiresAt, a.CreatedAt)
	return Token{a, token}, appError(e)
}
func (s *Store) Update(id string, in AppInput) (Application, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return Application{}, e
	}
	defer tx.Rollback()
	a, e := readApp(tx.QueryRow("SELECT "+appColumns+" FROM apps WHERE id=?", id))
	if errors.Is(e, sql.ErrNoRows) {
		return a, fault(404, "应用不存在")
	}
	if e != nil {
		return a, e
	}
	a, e = applyInput(a, in)
	if e != nil {
		return a, e
	}
	scopes, _ := json.Marshal(a.Scopes)
	_, e = tx.Exec("UPDATE apps SET name=?,description=?,scopes=?,enabled=?,expires_at=? WHERE id=?", a.Name, a.Description, string(scopes), a.Enabled, a.ExpiresAt, id)
	if e != nil {
		return a, appError(e)
	}
	return a, tx.Commit()
}
func (s *Store) Rotate(id string) (Token, error) {
	a, e := s.App(id)
	if e != nil {
		return Token{}, e
	}
	token := random("fsg_")
	a.TokenPrefix = token[:12] + "…"
	_, e = s.db.Exec("UPDATE apps SET token_hash=?,token_prefix=? WHERE id=?", hash(token), a.TokenPrefix, id)
	return Token{a, token}, e
}
func (s *Store) Credentials() (feishu.Credentials, error) {
	var c feishu.Credentials
	e := s.db.QueryRow("SELECT app_id,app_secret FROM feishu_settings WHERE id=1").Scan(&c.AppID, &c.AppSecret)
	if errors.Is(e, sql.ErrNoRows) {
		return c, fault(400, "请先在服务设置中填写飞书 App ID 和 App Secret")
	}
	return c, e
}
func (s *Store) Settings() (map[string]any, error) {
	c, e := s.Credentials()
	var f *Fault
	if e != nil && !errors.As(e, &f) {
		return nil, e
	}
	return map[string]any{"appId": c.AppID, "secretConfigured": c.AppSecret != ""}, nil
}
func (s *Store) SaveCredentials(id, secret string) error {
	id = strings.TrimSpace(id)
	secret = strings.TrimSpace(secret)
	if id == "" || len(id) > 128 || len(secret) > 512 {
		return fault(400, "App ID 或 App Secret 格式无效")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if secret == "" {
		var existing feishu.Credentials
		e = tx.QueryRow("SELECT app_id,app_secret FROM feishu_settings WHERE id=1").Scan(&existing.AppID, &existing.AppSecret)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if existing.AppID != id || existing.AppSecret == "" {
			return fault(400, "首次配置或更换 App ID 时必须填写 App Secret")
		}
		secret = existing.AppSecret
	}
	_, e = tx.Exec("INSERT INTO feishu_settings VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET app_id=excluded.app_id,app_secret=excluded.app_secret", id, secret)
	if e != nil {
		return e
	}
	return tx.Commit()
}

type Log struct {
	ID        string `json:"id"`
	Time      string `json:"time"`
	AppName   string `json:"appName"`
	Action    string `json:"action"`
	Table     string `json:"table"`
	Status    int    `json:"status"`
	Result    string `json:"result"`
	RequestID string `json:"requestId"`
}

func (s *Store) Log(a Application, action, table string, status int, result, requestID string) error {
	if len(result) > 1000 {
		result = result[:1000]
	}
	if requestID != "" {
		requestID = "upstream:" + requestID
	}
	_, e := s.db.Exec("INSERT INTO audit(id,time,app_id,app_name,action,table_name,record_id,status,result,field_names,reason,duration_ms) VALUES(?,?,?,?,?,?,?, ?,?,'[]',?,0)", random("log_"), timestamp(), a.ID, a.Name, action, table, "", status, result, requestID)
	if e == nil && a.ID != "" {
		_, e = s.db.Exec("UPDATE apps SET last_used_at=? WHERE id=?", timestamp(), a.ID)
	}
	return e
}
func (s *Store) Logs() ([]Log, error) {
	rows, e := s.db.Query("SELECT id,time,app_name,action,table_name,status,result,reason FROM audit ORDER BY rowid DESC LIMIT 100")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []Log{}
	for rows.Next() {
		var l Log
		if e = rows.Scan(&l.ID, &l.Time, &l.AppName, &l.Action, &l.Table, &l.Status, &l.Result, &l.RequestID); e != nil {
			return nil, e
		}
		if strings.HasPrefix(l.RequestID, "upstream:") {
			l.RequestID = strings.TrimPrefix(l.RequestID, "upstream:")
		} else {
			l.RequestID = ""
		}
		items = append(items, l)
	}
	return items, rows.Err()
}
func (s *Store) Overview() (map[string]any, error) {
	var apps, requests int
	if e := s.db.QueryRow("SELECT count(*) FROM apps").Scan(&apps); e != nil {
		return nil, e
	}
	if e := s.db.QueryRow("SELECT count(*) FROM audit").Scan(&requests); e != nil {
		return nil, e
	}
	recent, e := s.Logs()
	if len(recent) > 5 {
		recent = recent[:5]
	}
	return map[string]any{"apps": apps, "requests": requests, "recentLogs": recent}, e
}
