package hyperv

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clcdpc/github-runners/internal/secure"
)

func TestProgressRedactsSplitCredentialsAndBoundsLines(t *testing.T) {
	var out bytes.Buffer
	r := &secure.Redactor{}
	r.Add("split-secret-value")
	w := &redactedStream{out: &out, redactor: r}
	for _, chunk := range []string{"SQL startup split-", "secret-", "value\nSQLCMDPASS", "WORD=another-secret\nexitCode=137 OOMKilled=true\n", strings.Repeat("x", 64<<10), "overflow\nlast diagnostic"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, value := range []string{"split-secret-value", "another-secret", strings.Repeat("x", 100)} {
		if strings.Contains(s, value) {
			t.Fatal("unsafe progress output", s)
		}
	}
	if !strings.Contains(s, "exitCode=137 OOMKilled=true") || !strings.Contains(s, "line exceeded 64 KiB") || !strings.HasSuffix(s, "last diagnostic\n") {
		t.Fatal("useful diagnostics lost", s)
	}
}

type notificationWriter struct{ lines chan string }

func (w notificationWriter) Write(p []byte) (int, error) {
	w.lines <- string(p)
	return len(p), nil
}

func TestNativeGuestProgressStreamsBeforeProcessExit(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh.exe")
	if err != nil {
		t.Skip("native PowerShell unavailable")
	}
	c, _, _ := fixture(t)
	c.Command = nil
	c.Config.SSH = pwsh
	c.Redactor.Add("stream-private-value")
	lines := make(chan string, 20)
	c.Progress = notificationWriter{lines}
	marker := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.command(ctx, nil, pwsh, "-NoProfile", "-NonInteractive", "-Command", "[Console]::WriteLine('starting stream-private-value'); while (-not (Test-Path -LiteralPath '"+strings.ReplaceAll(marker, "'", "''")+"')) { Start-Sleep -Milliseconds 25 }; [Console]::Error.WriteLine('SQLCMDPASSWORD=short-secret'); exit 1")
		done <- err
	}()
	select {
	case line := <-lines:
		if !strings.Contains(line, "starting [REDACTED]") || strings.Contains(line, "stream-private-value") {
			t.Fatal("unsafe initial progress", line)
		}
	case <-ctx.Done():
		t.Fatal("progress buffered until process exit")
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || strings.Contains(err.Error(), "short-secret") {
		t.Fatal("stderr failure was lost or leaked credentials", err)
	}
	select {
	case line := <-lines:
		if strings.Contains(line, "short-secret") || !strings.Contains(line, "REDACTED") {
			t.Fatal("unsafe stderr progress", line)
		}
	default:
		t.Fatal("stderr not streamed")
	}
}

type failingProgress struct{}

func (failingProgress) Write([]byte) (int, error) { return 0, errors.New("diagnostic disk full") }

func TestProgressFailureIsReported(t *testing.T) {
	w := &progressWriter{out: failingProgress{}}
	if _, err := w.Write([]byte("evidence")); err == nil || w.failure() == nil {
		t.Fatal("diagnostic write failure ignored")
	}
}

func TestVerboseGuestOutputRetainsFinalEvidence(t *testing.T) {
	c, _, _ := fixture(t)
	c.Progress = io.Discard
	c.Command = func(context.Context, []byte, string, ...string) ([]byte, error) {
		return []byte(strings.Repeat("build progress\n", (9<<20)/15) + "SQLCMDPASSWORD=last-private-value\nlast startup diagnostic\nCLC_VALIDATION_PASSED\n"), nil
	}
	b, err := c.command(context.Background(), nil, c.Config.SSH, "validation")
	if err != nil || len(b) > 8<<20 || !strings.HasSuffix(string(b), "last startup diagnostic\nCLC_VALIDATION_PASSED\n") || strings.Contains(string(b), "last-private-value") {
		t.Fatal("bounded output lost final evidence or leaked a credential", err, len(b))
	}
}
