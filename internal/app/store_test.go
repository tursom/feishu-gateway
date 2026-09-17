package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenPersistenceRotationAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	a := application(t, s, "tasks:read")
	var stored string
	if e = s.db.QueryRow("SELECT token_hash FROM apps WHERE id=?", a.App.ID).Scan(&stored); e != nil || stored != hash(a.Token) || stored == a.Token {
		t.Fatal("token not hashed", e)
	}
	list, _ := s.Apps()
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), a.Token) {
		t.Fatal("token exposed")
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Authenticate(a.Token); e != nil {
		t.Fatal(e)
	}
	rotated, e := s.Rotate(a.App.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(a.Token); e == nil {
		t.Fatal("old token remains valid")
	}
	if _, e = s.Authenticate(rotated.Token); e != nil {
		t.Fatal(e)
	}
	disabled := false
	if _, e = s.Update(a.App.ID, AppInput{Enabled: &disabled}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(rotated.Token); e == nil {
		t.Fatal("disabled token accepted")
	}
	s.db.Exec("UPDATE apps SET enabled=1,expires_at=? WHERE id=?", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), a.App.ID)
	if _, e = s.Authenticate(rotated.Token); e == nil {
		t.Fatal("expired token accepted")
	}
}
func TestSettingsNeverReturnSecretAndPreserveBlank(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SaveCredentials("cli_test", ""); e == nil {
		t.Fatal("first save missing secret")
	}
	if e = s.SaveCredentials("cli_test", "private-value"); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveCredentials("cli_test", ""); e != nil {
		t.Fatal(e)
	}
	c, _ := s.Credentials()
	if c.AppSecret != "private-value" {
		t.Fatal("blank erased secret")
	}
	if e = s.SaveCredentials("cli_different", ""); e == nil {
		t.Fatal("changed ID reused secret")
	}
	info, _ := s.Settings()
	b, _ := json.Marshal(info)
	if strings.Contains(string(b), "private-value") {
		t.Fatal("secret returned in settings")
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	c, e = s.Credentials()
	if e != nil || c.AppSecret != "private-value" {
		t.Fatal("credentials not persisted", e)
	}
}
func TestPreviousCredentialsImportAndLegacyToken(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "feishu-credentials.json")
	original := []byte(`{"app_id":"cli_legacy","app_secret":"legacy-secret"}`)
	if e := os.WriteFile(legacy, original, 0600); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "gateway.sqlite")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	c, e := s.Credentials()
	if e != nil || c.AppID != "cli_legacy" {
		t.Fatal("credential import failed", e)
	}
	// Same hash and schema as the previous release; token length is not reinterpreted.
	token := "fsg_" + strings.Repeat("x", 43)
	_, e = s.db.Exec("INSERT INTO apps VALUES(?,?,?,?,?,?,1,NULL,NULL,?)", "existing-app", "Existing", "", hash(token), "fsg_x…", `["tasks:read"]`, timestamp())
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SaveCredentials("cli_new", "replacement"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	c, _ = s.Credentials()
	if c.AppID != "cli_new" {
		t.Fatal("legacy file overwrote saved credentials")
	}
	if _, e = s.Authenticate(token); e != nil {
		t.Fatal("old token rejected", e)
	}
	unchanged, _ := os.ReadFile(legacy)
	if string(unchanged) != string(original) {
		t.Fatal("legacy credential file changed")
	}
}
func TestOldWorkflowTablesAreUnused(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "gateway.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	_, e = s.db.Exec("CREATE TABLE idempotency(dummy TEXT); INSERT INTO idempotency VALUES('old entry')")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apps(); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveCredentials("cli_test", "secret"); e != nil {
		t.Fatal(e)
	}
	var value string
	s.db.QueryRow("SELECT dummy FROM idempotency").Scan(&value)
	if value != "old entry" {
		t.Fatal("existing workflow data removed")
	}
}
