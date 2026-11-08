package secure

import (
	"bytes"
	"io"
	"regexp"
	"strings"
)

// DiagnosticWriter sanitizes untrusted guest diagnostics before they reach an
// archive or bounded tail. It is single-stream and must not be shared between
// concurrent producers. Close flushes a final record, but does not close Out.
// Structured payloads are deliberately omitted: runner job messages can contain
// secret values before their isSecret metadata, or credentials under arbitrary
// variable names. This is a heuristic diagnostic filter, not a general secret
// detector: arbitrary unlabelled or encoded secrets may remain. Keep archives
// private and review them before sharing.
type DiagnosticWriter struct {
	out                                   io.Writer
	redactor                              *Redactor
	limit                                 int
	line                                  []byte
	depth                                 int
	quoted, escaped, pem, stopped, closed bool
	err                                   error
	credentialContinuation                bool
}

const diagnosticOmitted = "[REDACTED diagnostic record]\n"

func NewDiagnosticWriter(out io.Writer, redactor *Redactor, maxLineBytes int) *DiagnosticWriter {
	if maxLineBytes <= 0 || maxLineBytes > 64*1024 {
		maxLineBytes = 64 * 1024
	}
	if redactor == nil {
		redactor = &Redactor{}
	}
	return &DiagnosticWriter{out: out, redactor: redactor, limit: maxLineBytes}
}

func (w *DiagnosticWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	if w.err != nil {
		return 0, w.err
	}
	for i, c := range p {
		if w.stopped {
			continue
		}
		if c == '\n' {
			w.record()
			w.line = w.line[:0]
		} else if len(w.line) == w.limit {
			// An oversized line may hide a multiline payload opener beyond the cap.
			// Fail closed for the remainder of this stream rather than leak its tail.
			clear(w.line)
			w.line = w.line[:0]
			w.stopped = true
			w.emit("[REDACTED oversized diagnostic stream]\n")
		} else {
			w.line = append(w.line, c)
		}
		if w.err != nil {
			return i + 1, w.err
		}
	}
	return len(p), nil
}

func (w *DiagnosticWriter) Close() error {
	if !w.closed {
		w.closed = true
		if !w.stopped && len(w.line) != 0 && w.err == nil {
			w.record()
		}
		clear(w.line)
		w.line = nil
	}
	return w.err
}

// Abort discards an incomplete final record after cancellation or transport
// failure. It preserves already-emitted sanitized records and closes only this
// filter, never the destination. Call this instead of Close on interrupted input.
func (w *DiagnosticWriter) Abort() error {
	w.closed = true
	clear(w.line)
	w.line = nil
	return w.err
}
func (w *DiagnosticWriter) emit(s string) {
	if w.err != nil {
		return
	}
	n, err := io.WriteString(w.out, s)
	if err == nil && n != len(s) {
		err = io.ErrShortWrite
	}
	w.err = err
}

// Suppress entire credential-bearing lines, including quoted values containing
// commas, spaces, shell escapes, or additional assignment delimiters.
var diagnosticCredential = regexp.MustCompile(`(?i)(authorization|proxy-authorization|\bbearer\b|\bbasic\b|password|passwd|passphrase|secret|token|api[-_]?key|access[-_]?key|private[-_ ]?key|cookie|credential|jitconfig|signature|\bsig\s*[=:]|\bsas\s*[=:])`)
var diagnosticLogHeader = regexp.MustCompile(`^\s*\[[0-9]{4}-[0-9]{2}-[0-9]{2}[^\]]*\]`)

// journalctl short-iso-precise prefixes every physical message line. Strip only
// framing for parsing, retaining message indentation and the original output.
var diagnosticJournalHeader = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T\S+\s+\S+\s+[^:]+: ?`)
var diagnosticMultilineCredential = regexp.MustCompile(`(?i)(?:[:=]\s*(?:[|>][0-9+-]*(?:\s+#.*)?)?$|(?:password|passwd|passphrase|secret|token|api[-_]?key|authorization|cookie)\s*$)`)
var diagnosticArray = regexp.MustCompile(`\[\s*(?:["{\[]|$)`)
var diagnosticJSONField = regexp.MustCompile(`^\s*"[^"\r\n]+"\s*:`)

// Audit argument records can encode even short credentials as hex. Do not
// attempt to decode or retain them; numeric types are the Linux audit IDs.
var diagnosticAuditArguments = regexp.MustCompile(`(?i)\b(?:EXECVE|PROCTITLE|USER_CMD)\b|\btype\s*=\s*(?:1309|1327|1123)\b`)
var diagnosticCommand = regexp.MustCompile(`(?i)\bCOMMAND\s*=`)
var diagnosticURL = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s<>"']+`)

func (w *DiagnosticWriter) record() {
	raw := strings.TrimSuffix(string(w.line), "\r")
	message := diagnosticJournalHeader.ReplaceAllString(raw, "")
	if loc := diagnosticLogHeader.FindStringIndex(message); loc != nil {
		message = strings.TrimPrefix(message[loc[1]:], " ")
	}
	upper := strings.ToUpper(message)
	suppressContinuation := w.credentialContinuation && (strings.TrimSpace(message) == "" || strings.HasPrefix(message, " ") || strings.HasPrefix(message, "\t"))
	if !suppressContinuation {
		w.credentialContinuation = false
	}
	if w.pem || (strings.Contains(upper, "-----BEGIN ") && strings.Contains(upper, "PRIVATE KEY-----")) {
		if !w.pem {
			w.emit(diagnosticOmitted)
		}
		w.pem = !strings.Contains(upper, "-----END ")
		return
	}
	// sudo command logs contain arbitrary arguments, often without recognizable
	// credential labels. Retain caller/TTY/target metadata before COMMAND only.
	if w.depth == 0 {
		if diagnosticAuditArguments.MatchString(message) {
			w.credentialContinuation = true
			w.emit(diagnosticOmitted)
			return
		}
		if loc := diagnosticCommand.FindStringIndex(raw); loc != nil {
			raw = raw[:loc[1]] + "[REDACTED]"
			if loc := diagnosticCommand.FindStringIndex(message); loc != nil {
				message = message[:loc[1]] + "[REDACTED]"
			}
			w.credentialContinuation = true
		}
	}
	// Count JSON containers before redaction, which may remove structural bytes.
	// A '[' in a normal log header is not a payload opener. Objects anywhere and
	// arrays at the start of otherwise unprefixed records are conservative cues.
	wasPayload := w.depth > 0
	start := -1
	if wasPayload {
		start = 0
	} else {
		start = strings.IndexByte(message, '{')
		if loc := diagnosticArray.FindStringIndex(message); loc != nil && (start < 0 || loc[0] < start) {
			start = loc[0]
		}
		trimmed := strings.TrimSpace(message)
		if start < 0 && strings.HasPrefix(trimmed, "[") {
			start = strings.IndexByte(message, '[')
		}
	}
	if start >= 0 {
		for i := start; i < len(message); i++ {
			c := message[i]
			if w.quoted {
				if w.escaped {
					w.escaped = false
				} else if c == '\\' {
					w.escaped = true
				} else if c == '"' {
					w.quoted = false
				}
				continue
			}
			switch c {
			case '"':
				w.quoted = true
			case '{', '[':
				w.depth++
			case '}', ']':
				if w.depth > 0 {
					w.depth--
				}
			}
		}
		if w.depth == 0 {
			w.quoted, w.escaped = false, false
		}
		if !wasPayload {
			w.emit(diagnosticOmitted)
		}
		return
	}
	if diagnosticCredential.MatchString(raw) || diagnosticJSONField.MatchString(message) {
		w.credentialContinuation = true
		// Malformed or multiline shell values cannot safely be reconstructed from
		// independent log records. Stop instead of emitting a later value fragment.
		var quote byte
		escape := false
		for i := 0; i < len(raw); i++ {
			c := raw[i]
			if escape {
				escape = false
				continue
			}
			if c == 92 {
				escape = true
				continue
			}
			if quote != 0 {
				if c == quote {
					quote = 0
				}
			} else if c == '"' || c == 39 {
				quote = c
			}
		}
		if quote != 0 || diagnosticMultilineCredential.MatchString(message) || strings.Contains(raw, "<<") || strings.Contains(raw, "$(") || (len(raw) > 0 && raw[len(raw)-1] == 92) {
			w.stopped = true
		}
		w.emit(diagnosticOmitted)
		return
	}
	if suppressContinuation {
		return
	}
	// URL userinfo and query values may use arbitrary names. Remove the URL
	// entirely rather than trying to enumerate all credential parameter names.
	raw = diagnosticURL.ReplaceAllStringFunc(raw, func(s string) string {
		if strings.ContainsAny(s, "@?#") {
			return "[REDACTED URL]"
		}
		return s
	})
	// A registered multiline secret must not leak one line at a time.
	w.redactor.mu.RLock()
	replaceParts := func(secret string) {
		for _, part := range bytes.Split([]byte(secret), []byte{'\n'}) {
			s := strings.TrimSuffix(string(part), "\r")
			if s != "" {
				raw = strings.ReplaceAll(raw, s, "[REDACTED]")
			}
		}
	}
	for _, secret := range w.redactor.secrets {
		replaceParts(secret)
	}
	for _, secret := range w.redactor.transient {
		replaceParts(secret)
	}
	w.redactor.mu.RUnlock()
	w.emit(w.redactor.Text(raw) + "\n")
}
