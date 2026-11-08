package secure

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestCredentialRedaction(t *testing.T) {
	r := &Redactor{}
	r.Add("short-private-value")
	for _, secret := range []string{"short-private-value", "github_pat_notarealtoken123", "ghp_notarealtoken123", "Clc" + strings.Repeat("a", 48) + "Aa1!", strings.Repeat("a", 120), "eyJabcdefgh.abcdefgh.abcdefgh"} {
		got := r.Text("error: " + secret)
		if strings.Contains(got, secret) {
			t.Fatalf("credential was retained: %q", got)
		}
	}
	if got := r.Text("password=shortvalue"); strings.Contains(got, "shortvalue") {
		t.Fatal(got)
	}
	for _, key := range []string{"MSSQL_SA_PASSWORD", "SQLCMDPASSWORD", "CLC_SQL_PASSWORD"} {
		if got := r.Text(key + "=short-sql-credential"); strings.Contains(got, "short-sql-credential") {
			t.Fatal("SQL environment credential leaked", got)
		}
		var out bytes.Buffer
		w := &Writer{Out: &out, Redactor: r}
		if _, err := w.Write([]byte(`{"` + key + `":"short-sql-credential"}`)); err != nil || strings.Contains(out.String(), "short-sql-credential") {
			t.Fatal("structured SQL credential leaked", err, out.String())
		}
	}
	if got := r.Text("runner clc-linux completed with exit code 1"); got != "runner clc-linux completed with exit code 1" {
		t.Fatal("redaction removed useful diagnostics")
	}
}
func TestStructuredLogsRemainJSON(t *testing.T) {
	var out bytes.Buffer
	r := &Redactor{}
	r.Add("a-secret")
	logger := slog.New(slog.NewJSONHandler(&Writer{Out: &out, Redactor: r}, nil))
	logger.Error("failed with a-secret", "requestId", int64(9223372036854775806), "authorization", "Basic small", "nested", map[string]any{"token": "short", "detail": "retry"})
	var data map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err, out.String())
	}
	if string(data["requestId"]) != "9223372036854775806" {
		t.Fatal("integer precision was lost")
	}
	for _, secret := range []string{"a-secret", "Basic small", `"short"`} {
		if strings.Contains(out.String(), secret) {
			t.Fatal("secret retained in structured log")
		}
	}
}
