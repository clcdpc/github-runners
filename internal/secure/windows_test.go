//go:build windows

package secure

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDPAPIRoundTripAndCorruption(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "protect.ps1")
	file := filepath.Join(dir, "pat.dpapi")
	// This is a synthetic test value, never an actual access token.
	source := `param($Target)
$bytes = [Text.Encoding]::UTF8.GetBytes('github_pat_synthetic_unit_fixture')
[IO.File]::WriteAllBytes($Target, [Security.Cryptography.ProtectedData]::Protect($bytes, $null, [Security.Cryptography.DataProtectionScope]::CurrentUser))
`
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("pwsh", "-NoProfile", "-NonInteractive", "-File", script, file).CombinedOutput(); err != nil {
		t.Fatal(err, string(b))
	}
	pat, err := ReadPAT(file)
	if err != nil || pat != "github_pat_synthetic_unit_fixture" {
		t.Fatal("DPAPI round trip failed", err)
	}
	if err = os.WriteFile(file, []byte("invalid encrypted data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadPAT(file); err == nil {
		t.Fatal("accepted corrupt DPAPI data")
	}
	if _, err = ReadPAT(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("accepted missing secret")
	}
}
func TestCrossProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Lock(path); err == nil {
		second()
		t.Fatal("duplicate lock succeeded")
	}
	unlock()
	unlocked, err := Lock(path)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	unlocked()
}
func TestProcessIdentityForStatus(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !ProcessRunning(os.Getpid(), time.Now(), executable) {
		t.Fatal("running process identity not recognized")
	}
	if ProcessRunning(os.Getpid(), time.Now(), executable+".other") || ProcessRunning(os.Getpid(), time.Now().Add(-time.Hour), executable) || ProcessRunning(-1, time.Now(), executable) {
		t.Fatal("stale or unrelated PID accepted")
	}
}
