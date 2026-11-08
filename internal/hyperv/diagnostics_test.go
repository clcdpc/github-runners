package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestArchiveDiagnosticSourcesAndPartialFailure(t *testing.T) {
	c, _, image := fixture(t)
	name := c.newName()
	if _, err := c.Create(context.Background(), name, image.ID, 42); err != nil {
		t.Fatal(err)
	}
	c.Redactor.Add("synthetic-private-value")
	seen := map[string]bool{}
	c.Command = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program != c.Config.SSH || !bytes.Equal(input, collectDiagnostics) {
			t.Fatal("unexpected collection invocation")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 16*time.Second {
			t.Fatal("missing bounded capture deadline")
		}
		command := args[len(args)-1]
		if !strings.HasPrefix(command, "sudo -n /usr/bin/timeout --signal=KILL 10s /usr/bin/python3 - ") {
			t.Fatal(command)
		}
		parts := strings.Fields(command)
		source := parts[len(parts)-1]
		seen[source] = true
		if source == "worker" {
			return []byte("worker retained before failure\npassword=synthetic-private-value\nsynthetic-private-"), errors.New("transport lost token=unsafe-error-value")
		}
		if source == "boot" {
			return []byte(strings.Repeat("boot ordinary event\n", 140000) + "synthetic-private-value\nlatest service stopped\n"), nil
		}
		return []byte(source + " diagnostic event\n"), nil
	}
	if err := c.Archive(context.Background(), name, true); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(c.LogDir, "runners", name)
	for _, source := range diagnosticSources {
		if !seen[source.name] {
			t.Fatal("source omitted", source.name)
		}
		b, err := os.ReadFile(filepath.Join(dir, source.name+".log"))
		if err != nil || len(b) > source.limit+len(diagnosticTruncation) {
			t.Fatal(source.name, len(b), err)
		}
		for _, secret := range []string{"synthetic-private", "unsafe-error-value"} {
			if strings.Contains(string(b), secret) {
				t.Fatal("archive leaked private data", source.name)
			}
		}
		if source.name == "boot" && (!bytes.HasPrefix(b, []byte(diagnosticTruncation)) || !bytes.Contains(b, []byte("latest service stopped"))) {
			t.Fatal("bounded boot tail/marker missing")
		}
		if source.name == "worker" && !bytes.Contains(b, []byte("worker retained before failure")) {
			t.Fatal("partial evidence discarded")
		}
	}
	report, err := os.ReadFile(filepath.Join(dir, "collection.json"))
	var manifest struct {
		Sources []struct {
			Source, Error string
			Truncated     bool
		}
	}
	if err != nil || json.Unmarshal(report, &manifest) != nil || len(manifest.Sources) != len(diagnosticSources) {
		t.Fatal("invalid collection report", err)
	}
	if strings.Contains(string(report), "unsafe-error-value") {
		t.Fatal("error leaked secret")
	}
	for _, entry := range manifest.Sources {
		if entry.Source == "worker" && entry.Error == "" {
			t.Fatal("partial source labeled complete")
		}
		if entry.Source == "boot" && !entry.Truncated {
			t.Fatal("missing truncation status")
		}
	}
}

func TestArchiveAbsentCancelledAndWriteFailure(t *testing.T) {
	for _, scenario := range []string{"absent", "cancelled", "write-failure"} {
		t.Run(scenario, func(t *testing.T) {
			c, _, image := fixture(t)
			name := c.newName()
			if _, err := c.Create(context.Background(), name, image.ID, 42); err != nil {
				t.Fatal(err)
			}
			c.Command = func(context.Context, []byte, string, ...string) ([]byte, error) {
				if scenario != "write-failure" {
					t.Fatal("attempted unavailable/cancelled guest")
				}
				return []byte("retained evidence\n"), nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			dir := filepath.Join(c.LogDir, "runners", name)
			if scenario == "write-failure" {
				if err := os.MkdirAll(filepath.Join(dir, "service-state.log"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			err := c.Archive(ctx, name, scenario != "absent")
			if scenario == "write-failure" {
				if err == nil {
					t.Fatal("write failure did not block removal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(dir, "collection.json"))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cancelled" && !bytes.Contains(b, []byte("not attempted")) {
				t.Fatal("cancellation hidden")
			}
		})
	}
}

func TestDiagnosticCommandRedactsBeforeRetention(t *testing.T) {
	for _, limit := range []int{64, 128, 1024} {
		r := &secure.Redactor{}
		r.Add("registered-credential-value")
		c := &Client{Redactor: r, diagnosticLimit: limit}
		c.Command = func(context.Context, []byte, string, ...string) ([]byte, error) {
			return []byte(strings.Repeat("normal record\n", 200) + "registered-credential-value\nAuthorization: Bearer short\nlast record\n"), nil
		}
		b, err := c.diagnosticCommand(context.Background(), nil, "fixture")
		if err != nil || len(b) > limit+len(diagnosticTruncation) || bytes.Contains(b, []byte("credential-value")) || bytes.Contains(b, []byte("Bearer")) || !bytes.Contains(b, []byte("last record")) {
			t.Fatal(string(b), err)
		}
	}
}

func TestDiagnosticNativeProcess(t *testing.T) {
	if os.Getenv("CLC_DIAGNOSTIC_HELPER") == "1" {
		io.WriteString(os.Stdout, "native evidence\npassword=native-stdout-secret\n")
		io.WriteString(os.Stderr, "Bearer native-stderr-secret\n")
		os.Exit(23)
	}
	t.Setenv("CLC_DIAGNOSTIC_HELPER", "1")
	c := &Client{Redactor: &secure.Redactor{}, diagnosticLimit: 1024}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := c.diagnosticCommand(ctx, nil, os.Args[0], "-test.run=^TestDiagnosticNativeProcess$")
	if err == nil || !bytes.Contains(b, []byte("native evidence")) || strings.Contains(string(b)+err.Error(), "native-stdout-secret") || strings.Contains(err.Error(), "native-stderr-secret") {
		t.Fatal(string(b), err)
	}
}

func TestGuestDiagnosticPython(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python is required for isolated guest collector tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, script := range []string{"test_collect.py", "test_trace_common.py", "test_kernel_trace.py", "test_bus_trace.py", "test_state_trace.py", "test_bus_live.py"} {
		cmd := exec.CommandContext(ctx, python, "-B", filepath.Join("diagnostics", script))
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(script, err, string(b))
		}
	}
}

// Trace records receive exactly the same redaction as all other guest content.
// A forgeable prefix never exempts a record from credential suppression.
func TestMetadataTraceSurvivesArchiveRedaction(t *testing.T) {
	c := &Client{Redactor: &secure.Redactor{}, diagnosticLimit: 4096}
	c.Command = func(context.Context, []byte, string, ...string) ([]byte, error) {
		return []byte("CLC_TRACE source=bus event=bus_attempt request_serial=22 pid=234 uid=0 sender_major=1 sender_minor=20 observed_mono_ns=2000000\n" +
			"CLC_TRACE source=bus event=bus_reply request_serial=22 job_id=44 outcome=1\n" +
			"CLC_TRACE source=kernel event=signal_generated target_pid=55 signal=15 program_class=1\n" +
			"CLC_TRACE source=state event=state_journal kind=7 n_restarts=1\n" +
			"CLC_TRACE password=synthetic-private-value\n"), nil
	}
	b, err := c.diagnosticCommand(context.Background(), nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"event=bus_attempt", "event=bus_reply", "request_serial=22", "event=signal_generated", "event=state_journal"} {
		if !bytes.Contains(b, []byte(expected)) {
			t.Fatal("metadata was lost to redaction", expected, string(b))
		}
	}
	if bytes.Contains(b, []byte("synthetic-private-value")) {
		t.Fatal("trace prefix bypassed redaction")
	}
}

func TestObserverUnitsRemainIndependentAndOptional(t *testing.T) {
	runner, err := guest.ReadFile("guest/clc-runner.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runner), "Wants=network-online.target clc-trace-") || strings.Contains(string(runner), "Requires=clc-trace") {
		t.Fatal("observers must be weak Wants, not a runner availability dependency")
	}
	for _, kind := range []string{"bus", "kernel", "state"} {
		b, err := guest.ReadFile("guest/clc-trace-" + kind + ".service")
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, needed := range []string{"Before=cloud-final.service clc-runner.service", "LimitCORE=0", "LogsDirectoryMode=0700", "NoNewPrivileges=yes", "MemoryMax=", "CPUQuota="} {
			if !strings.Contains(s, needed) {
				t.Fatal(kind, needed)
			}
		}
		for _, forbidden := range []string{"PartOf=clc-runner", "BindsTo=clc-runner", "Requires=clc-runner", "CAP_SYS_ADMIN"} {
			if strings.Contains(s, forbidden) {
				t.Fatal("observer weakens isolation or dies with runner", kind, forbidden)
			}
		}
	}
	entries, _ := guest.ReadDir("guest")
	for _, entry := range entries {
		if entry.IsDir() || strings.Contains(entry.Name(), "__pycache__") {
			t.Fatal("generated artifacts embedded in image")
		}
	}
}
