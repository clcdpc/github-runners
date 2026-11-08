package secure

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"
)

// Long opaque values are removed as well as known secrets. This protects runner
// diagnostics recovered after a restart, when the controller no longer knows JIT.
var opaque = regexp.MustCompile(`(?:github_pat_[A-Za-z0-9_]+|gh[pousr]_[A-Za-z0-9_]+|Clc[a-f0-9]{48}Aa1!|[A-Za-z0-9_+/=-]{80,}|eyJ[A-Za-z0-9_.-]+)`)
var sensitive = regexp.MustCompile(`(?i)("?(?:authorization|password|MSSQL_SA_PASSWORD|SQLCMDPASSWORD|CLC_SQL_PASSWORD|access_?token|refresh_?token|client_?secret|encodedJITConfig|jitconfig|token)"?\s*[:=]\s*)[^\r\n,}]+`)

type Redactor struct {
	mu        sync.RWMutex
	secrets   []string
	transient map[string]string
}

func (r *Redactor) Set(key, secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transient == nil {
		r.transient = map[string]string{}
	}
	r.transient[key] = secret
}
func (r *Redactor) Forget(key string) { r.mu.Lock(); defer r.mu.Unlock(); delete(r.transient, key) }

func (r *Redactor) Add(secret string) {
	if secret == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = append(r.secrets, secret)
}
func (r *Redactor) Text(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	for _, secret := range r.transient {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	s = opaque.ReplaceAllString(s, "[REDACTED]")
	return sensitive.ReplaceAllString(s, `${1}"[REDACTED]"`)
}

// Writer sanitizes complete slog JSON records and retains valid JSON and integer
// precision. Sensitive key values are removed even if not known to this process.
type Writer struct {
	Out      io.Writer
	Redactor *Redactor
	mu       sync.Mutex
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := json.NewDecoder(bytes.NewReader(p))
	d.UseNumber()
	var value any
	s := w.Redactor.Text(string(p))
	if d.Decode(&value) == nil {
		b, err := json.Marshal(w.sanitize(value))
		if err != nil {
			return 0, err
		}
		s = string(b) + "\n"
	}
	_, err := io.WriteString(w.Out, s)
	return len(p), err
}

var secretKey = regexp.MustCompile(`(?i)^(authorization|password|MSSQL_SA_PASSWORD|SQLCMDPASSWORD|CLC_SQL_PASSWORD|access_?token|refresh_?token|client_?secret|encodedJITConfig|jitconfig|token|personalAccessToken)$`)

func (w *Writer) sanitize(value any) any {
	switch v := value.(type) {
	case string:
		return w.Redactor.Text(v)
	case map[string]any:
		for key, item := range v {
			if secretKey.MatchString(key) {
				v[key] = "[REDACTED]"
			} else {
				v[key] = w.sanitize(item)
			}
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = w.sanitize(item)
		}
		return v
	default:
		return value
	}
}
