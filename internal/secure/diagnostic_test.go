package secure

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDiagnosticWriterAdversarialRecords(t *testing.T) {
	cases := []struct {
		name, input string
		secrets     []string
	}{
		{"array", "before\n[\"tinyArraySecret\", \"second\"]\nafter", []string{"tinyArraySecret"}},
		{"multiline quoted", "before\n--password \"first\ntinyMultilineSecret\nlast\"", []string{"tinyMultilineSecret"}},
		{"YAML block", "before\npassword: |\n  tinyMultilineSecret", []string{"tinyMultilineSecret"}},
		{"PEM", "before\n-----BEGIN RSA PRIVATE KEY-----\nshortPrivateKeyMaterial\n-----END RSA PRIVATE KEY-----\nafter", []string{"shortPrivateKeyMaterial"}},
		{"unterminated PEM", "before\n-----BEGIN OPENSSH PRIVATE KEY-----\nshortPrivateKeyMaterial", []string{"shortPrivateKeyMaterial"}},
		{"URL credentials", "before\nfetch https://user:tinyPass@host/path\nfetch https://host/path?oddName=tinyQuery\nfetch https://host/#tinyFragment\nafter", []string{"tinyPass", "tinyQuery", "tinyFragment"}},
		{"headers", "before\nAuthorization: Basic dTpw\nrequest bearer tinyBearer\nCookie: session=tinyCookie; other=x\nX-API-Key: tinyKey\nafter", []string{"dTpw", "tinyBearer", "tinyCookie", "tinyKey"}},
		{"assignments", "before\nPASSWORD=\"tiny, quoted secret\"\ncmd --client-secret 'tiny cli secret'\nAWS_SECRET_ACCESS_KEY=tinyKey\nafter", []string{"tiny, quoted secret", "tiny cli secret", "tinyKey"}},
		{"job value before flag", "before\n[2026-11-07 INFO Worker] Job message: {\n  \"variables\": {\n    \"innocentName\": {\n      \"value\": \"tinyWorkerSecret\",\n      \"isSecret\": true\n    }\n  }\n}\nafter", []string{"tinyWorkerSecret"}},
		{"job flag before value", "before\n{\n\"isSecret\": true,\n\"value\": \"tinyWorkerSecret\"\n}\nafter", []string{"tinyWorkerSecret"}},
		{"escaped braces", "before\n{\n\"value\": \"brace } \\\" [\",\n\"second\": \"tinyWorkerSecret\"\n}\nafter", []string{"tinyWorkerSecret"}},
		{"orphan job field", "before\n  \"value\": \"tinyWorkerSecret\",\nafter", []string{"tinyWorkerSecret"}},
		{"truncated object", "before\n{\n\"value\": \"tinyWorkerSecret\"", []string{"tinyWorkerSecret"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			for chunk := 1; chunk <= len(tt.input); chunk++ {
				var out bytes.Buffer
				w := NewDiagnosticWriter(&out, nil, 0)
				for pos := 0; pos < len(tt.input); pos += chunk {
					end := min(pos+chunk, len(tt.input))
					n, err := w.Write([]byte(tt.input[pos:end]))
					if err != nil || n != end-pos {
						t.Fatal(n, err)
					}
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				for _, secret := range tt.secrets {
					if strings.Contains(out.String(), secret) {
						t.Fatalf("chunk %d leaked %q: %s", chunk, secret, out.String())
					}
				}
				if !strings.HasPrefix(out.String(), "before\n") {
					t.Fatal("lost preceding diagnostic", out.String())
				}
				if strings.HasSuffix(tt.input, "after") && !strings.HasSuffix(out.String(), "after\n") {
					t.Fatal("lost following diagnostic", out.String())
				}
			}
		})
	}
}

func TestDiagnosticWriterBoundedAndKnownSecrets(t *testing.T) {
	var out bytes.Buffer
	r := &Redactor{}
	r.Add("tinyKnown")
	r.Set("multi", "firstPart\nsecondPart")
	w := NewDiagnosticWriter(&out, r, 32)
	_, _ = w.Write([]byte("tinyKnown\nfirstPart\nsecondPart\nordinary exit code 1\r\n"))
	// This input includes a credential prefix past the capacity and a following
	// value which must not escape as a separate record.
	_, _ = w.Write([]byte(strings.Repeat("x", 10000) + "-----BEGIN PRIVATE KEY-----\nveryShortHiddenValue\n"))
	if len(w.line) > 32 {
		t.Fatal("unbounded pending line")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"tinyKnown", "firstPart", "secondPart", "veryShortHiddenValue", strings.Repeat("x", 32)} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
	if !strings.Contains(out.String(), "ordinary exit code 1\n") {
		t.Fatal(out.String())
	}
	if !strings.Contains(out.String(), "oversized diagnostic stream") {
		t.Fatal(out.String())
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatal("incomplete record")
	}
	if _, err := w.Write([]byte("closed")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

type diagnosticShortWriter struct{}

func (diagnosticShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestDiagnosticWriterErrors(t *testing.T) {
	w := NewDiagnosticWriter(diagnosticShortWriter{}, nil, 0)
	if _, err := w.Write([]byte("normal\n")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

func TestDiagnosticWriterAbortDiscardsPending(t *testing.T) {
	var out bytes.Buffer
	r := &Redactor{}
	r.Add("knownSecretWhosePrefixMustNotLeak")
	w := NewDiagnosticWriter(&out, r, 0)
	_, err := w.Write([]byte("complete diagnostic\nknownSecretWhosePrefix"))
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Abort(); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "complete diagnostic\n" {
		t.Fatal("incomplete record escaped", out.String())
	}
	if _, err = w.Write([]byte("more")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

func TestDiagnosticWriterPrefixedMultilineCredentials(t *testing.T) {
	prefixes := []string{
		"",
		"2026-11-07T18:03:24.123456+0000 runner-host runner.service[123]: ",
		"[2026-11-07 18:03:24Z INFO Worker] ",
	}
	openers := []string{"password: |", "password: >-", "password: |2+ # literal", "PASSWORD=", "Authorization:", "cmd --client-secret", "password=\"first", "password=first"}
	for _, prefix := range prefixes {
		for _, opener := range openers {
			input := "before\n" + prefix + opener + "\n" + prefix + "  tinyContinuationValue\n" + prefix + "\tsecondContinuationValue\n"
			for chunk := 1; chunk <= len(input); chunk++ {
				var out bytes.Buffer
				w := NewDiagnosticWriter(&out, nil, 0)
				for pos := 0; pos < len(input); pos += chunk {
					if _, err := w.Write([]byte(input[pos:min(pos+chunk, len(input))])); err != nil {
						t.Fatal(err)
					}
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(out.String(), "before\n") {
					t.Fatal("lost preceding record")
				}
				for _, secret := range []string{"tinyContinuationValue", "secondContinuationValue"} {
					if strings.Contains(out.String(), secret) {
						t.Fatalf("prefix %q opener %q chunk %d leaked %s", prefix, opener, chunk, out.String())
					}
				}
			}
		}
	}
}

func TestDiagnosticWriterCommandAndAuditArguments(t *testing.T) {
	prefix := "2026-11-07T18:03:24.123456+0000 runner-host sudo[123]: "
	command := prefix + "runner : TTY=pts/0 ; PWD=/work ; USER=root ; COMMAND=/usr/bin/sshpass -p tinyvalue /usr/bin/ssh host"
	cases := []string{
		"type=EXECVE msg=audit(123.123:4): argc=3 a0=\"tool\" a1=\"-p\" a2=74696e7976616c7565",
		"type=PROCTITLE msg=audit(123.123:4): proctitle=74696e7976616c7565",
		"type=USER_CMD msg=audit(123.123:4): pid=123 uid=1000 cmd=74696e7976616c7565",
		"type=1309 audit(123.123:4): argc=2 a0=tool a1=74696e7976616c7565",
		"type=1327 audit(123.123:4): proctitle=74696e7976616c7565",
		"type=1123 audit(123.123:4): cmd=74696e7976616c7565",
		"EXECVE argc=2 a0=tool a1=74696e7976616c7565",
	}
	input := "before\n" + command + "\n"
	for _, record := range cases {
		input += prefix + record + "\n"
	}
	input += "after\n"
	for chunk := 1; chunk <= len(input); chunk++ {
		var out bytes.Buffer
		w := NewDiagnosticWriter(&out, nil, 0)
		for pos := 0; pos < len(input); pos += chunk {
			if _, err := w.Write([]byte(input[pos:min(pos+chunk, len(input))])); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		for _, unwanted := range []string{"tinyvalue", "74696e7976616c7565", "sshpass", "argc=", "a0=", "cmd="} {
			if strings.Contains(got, unwanted) {
				t.Fatalf("chunk %d leaked %q: %s", chunk, unwanted, got)
			}
		}
		for _, wanted := range []string{"before\n", "after\n", prefix + "runner : TTY=pts/0 ; PWD=/work ; USER=root ; COMMAND=[REDACTED]\n"} {
			if !strings.Contains(got, wanted) {
				t.Fatalf("chunk %d lost %q: %s", chunk, wanted, got)
			}
		}
	}
}

func TestDiagnosticWriterPrefixedArrays(t *testing.T) {
	prefixes := []string{
		"",
		"2026-11-07T20:00:00.000000+00:00 host worker[123]: ",
		"[2026-11-07 20:00:00Z INFO Worker] ",
	}
	arrays := [][]string{
		{`[123, "tinyValue"]`},
		{`[true, "tinyValue"]`},
		{`[false, "tinyValue"]`},
		{`[null, "tinyValue"]`},
		{`[123,`, `  "tinyValue"`, `]`},
		{`[true,`, `  "tinyValue"`, `]`},
	}
	for _, prefix := range prefixes {
		for _, lines := range arrays {
			input := prefix + "before\n"
			for _, line := range lines {
				input += prefix + line + "\n"
			}
			input += prefix + "after\n"
			for chunk := 1; chunk <= len(input); chunk++ {
				var out bytes.Buffer
				w := NewDiagnosticWriter(&out, nil, 0)
				for pos := 0; pos < len(input); pos += chunk {
					if _, err := w.Write([]byte(input[pos:min(pos+chunk, len(input))])); err != nil {
						t.Fatal(err)
					}
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				want := prefix + "before\n" + diagnosticOmitted + prefix + "after\n"
				if out.String() != want {
					t.Fatalf("prefix %q lines %q chunk %d: got %q, want %q", prefix, lines, chunk, out.String(), want)
				}
			}
		}
	}
}
