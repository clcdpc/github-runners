package hyperv

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/clcdpc/github-runners/internal/secure"
)

// Sent by the collector, not installed in the immutable image. Existing images
// gain collection after a controller upgrade without a guest configuration edit.
//
//go:embed diagnostics/collect.py
var collectDiagnostics []byte

var diagnosticSources = []struct {
	name  string
	limit int
}{
	{"trace-state", 2 << 20},
	{"trace-bus", 2 << 20},
	{"trace-kernel", 4 << 20},
	{"service-state", 128 << 10},
	{"services", 512 << 10},
	{"kernel", 512 << 10},
	{"auth", 512 << 10},
	{"packages", 512 << 10},
	{"dpkg-files", 512 << 10},
	{"apt-files", 512 << 10},
	{"unattended-files", 512 << 10},
	{"runner", 1 << 20},
	{"worker", 1 << 20},
	{"boot", 2 << 20},
}

const diagnosticTruncation = "[diagnostics: earlier output omitted by byte limit]\n"

// Archive runs before controller cleanup stops/removes the VM. Each source has
// its own bound so a verbose boot journal cannot crowd out worker/auth evidence.
// Capture failure is recorded but never prevents eventual disk/secret cleanup.
// Host write failure still blocks removal to avoid silently losing collected data.
func (c *Client) Archive(ctx context.Context, name string, present bool) error {
	r, err := c.read(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	dest := filepath.Join(c.LogDir, "runners", name)
	if err = os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	type result struct {
		Source      string `json:"source"`
		Bytes       int    `json:"bytes"`
		Limit       int    `json:"limitBytes"`
		Truncated   bool   `json:"truncated"`
		FileUpdated bool   `json:"fileUpdated"`
		Error       string `json:"error,omitempty"`
	}
	report := struct {
		Version    int       `json:"version"`
		CapturedAt time.Time `json:"capturedAt"`
		Present    bool      `json:"guestPresent"`
		Sources    []result  `json:"sources"`
	}{Version: 1, CapturedAt: time.Now().UTC(), Present: present, Sources: []result{}}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if present {
		// Do not copy Client's mutex or inherit a public progress sink: guest evidence
		// belongs only in the private runner archive, never the controller transcript.
		capture := &Client{Config: c.Config, Redactor: c.Redactor, Command: c.Command}
		capture.Config.CommandTimeoutSeconds = 15
		for _, source := range diagnosticSources {
			entry := result{Source: source.name, Limit: source.limit}
			if ctx.Err() != nil {
				entry.Error = "collection deadline reached; source not attempted"
			} else {
				sourceCtx, sourceCancel := context.WithTimeout(ctx, 15*time.Second)
				capture.diagnosticLimit = source.limit
				// timeout also bounds the remote process group if SSH disconnects. Input
				// and source are fixed embedded bytes/allowlisted names, not workflow data.
				b, probeErr := capture.ssh(sourceCtx, r, collectDiagnostics,
					"sudo -n /usr/bin/timeout --signal=KILL 10s /usr/bin/python3 - "+source.name)
				sourceCancel()
				entry.Bytes, entry.Truncated = len(b), bytes.HasPrefix(b, []byte(diagnosticTruncation))
				if probeErr != nil {
					entry.Error = diagnosticError(c.Redactor, probeErr)
				}
				// Preserve partial stdout on remote failure/timeout. Never label it complete.
				if len(b) > 0 {
					entry.FileUpdated = true
					if err = os.WriteFile(filepath.Join(dest, source.name+".log"), b, 0600); err != nil {
						return err
					}
				}
			}
			report.Sources = append(report.Sources, entry)
		}
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dest, "collection.json"), append(b, '\n'), 0600); err != nil {
		return err
	}
	if !present {
		return os.WriteFile(filepath.Join(dest, "availability.log"), []byte("Guest absent; no live diagnostics could be collected before resource cleanup.\n"), 0600)
	}
	return nil
}

func diagnosticError(r *secure.Redactor, err error) string {
	var out bytes.Buffer
	w := secure.NewDiagnosticWriter(&out, r, 16<<10)
	_, _ = io.WriteString(w, err.Error())
	_ = w.Close()
	return strings.TrimSpace(out.String())
}

// This path always redacts BEFORE bounded retention, including native stderr,
// mocked commands, chunk boundaries and partial output on a failed SSH process.
func (c *Client) diagnosticCommand(ctx context.Context, input []byte, executable string, args ...string) ([]byte, error) {
	out, stderr := &buffer{limit: c.diagnosticLimit, tail: true}, &buffer{limit: 32 << 10, tail: true}
	stdout := secure.NewDiagnosticWriter(out, c.Redactor, 64<<10)
	errorOut := secure.NewDiagnosticWriter(stderr, c.Redactor, 64<<10)
	var err error
	if c.Command != nil {
		var b []byte
		b, err = c.Command(ctx, input, executable, args...)
		_, writeErr := stdout.Write(b)
		err = errors.Join(err, writeErr)
	} else {
		cmd := exec.CommandContext(ctx, executable, args...)
		hideWindow(cmd)
		cmd.Stdin = bytes.NewReader(input)
		cmd.Stdout, cmd.Stderr = stdout, errorOut
		cmd.WaitDelay = 2 * time.Second
		err = cmd.Run()
	}
	if err != nil {
		err = errors.Join(err, stdout.Abort(), errorOut.Abort())
	} else {
		err = errors.Join(err, stdout.Close(), errorOut.Close())
	}
	b := out.Bytes()
	if out.truncated {
		// Discard the first partial (already sanitized) line and mark evidence loss.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			b = nil
		}
		b = append([]byte(diagnosticTruncation), b...)
	}
	if err != nil {
		return b, fmt.Errorf("diagnostic capture failed (%v): %s", err, stderr.String())
	}
	return b, nil
}
