package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/secure"
	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"
)

type hostMock struct {
	c         *Client
	ops       []string
	vms       map[string]VM
	fail      string
	sshOutput string
	stdin     []byte
	args      []string
}

func fixture(t *testing.T) (*Client, *hostMock, Image) {
	t.Helper()
	conf, err := config.Load("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	conf.HyperV.ImageDirectory = filepath.Join(dir, "images")
	c := &Client{Config: conf.HyperV, Owner: uuid.NewString(), Scale: "clc-linux", DataDir: filepath.Join(dir, "data"), LogDir: filepath.Join(dir, "logs"), Redactor: &secure.Redactor{}}
	if err = os.MkdirAll(c.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	m := makeImage(t, c, "ubuntu2404-20261028T120000Z-123456abcdef")
	f := &hostMock{c: c, vms: map[string]VM{}}
	c.Command = f.run
	return c, f, m
}
func makeImage(t *testing.T, c *Client, version string) Image {
	t.Helper()
	dir := filepath.Join(c.Config.ImageDirectory, version)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "base.vhdx")
	if err := os.WriteFile(path, []byte("synthetic VHDX content"), 0444); err != nil {
		t.Fatal(err)
	}
	sum, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := Image{Schema: 1, Owner: c.Owner, Version: version, ID: sum, Path: path, Created: time.Now(), Inputs: strings.Repeat("a", 64), SourceSHA256: strings.Repeat("b", 64), DiskGiB: c.Config.DiskGiB, Validation: map[string]bool{"cloneBoot": true, "dockerSmoke": true}}
	if err = atomicJSON(filepath.Join(dir, "image.json"), m); err != nil {
		t.Fatal(err)
	}
	if err = atomicJSON(filepath.Join(c.Config.ImageDirectory, "current.json"), map[string]string{"version": version}); err != nil {
		t.Fatal(err)
	}
	return m
}
func (f *hostMock) run(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
	if program == f.c.Config.SSHKeygen {
		path := args[len(args)-1]
		if err := os.WriteFile(path, []byte("synthetic private key"), 0600); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAATEST public"), 0600)
	}
	if program == f.c.Config.SSH {
		f.stdin = append([]byte{}, input...)
		f.args = append([]string{}, args...)
		return []byte(f.sshOutput), nil
	}
	var request struct {
		Records []Record `json:"records"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return nil, err
	}
	op := args[len(args)-1]
	f.ops = append(f.ops, op)
	if f.fail == op {
		return nil, fmt.Errorf("synthetic %s failure", op)
	}
	if op == "Inventory" {
		items := []VM{}
		for _, r := range request.Records {
			v, ok := f.vms[r.Name]
			if !ok {
				v = VM{Name: r.Name, RunnerID: r.RunnerID, Created: r.Created, Status: "Missing"}
			}
			items = append(items, v)
		}
		return json.Marshal(items)
	}
	if op == "ImageReferences" {
		return []byte(`[]`), nil
	}
	if op == "Admission" {
		return json.Marshal(Admission{Ready: true, Decision: "Accepted", CheckedAt: time.Now(), RequestedCPUs: f.c.Config.CPUs, RequestedMemoryGiB: f.c.Config.MemoryGiB, ReserveMemoryGiB: f.c.Config.ReserveMemoryGiB})
	}
	if op == "Capacity" || op == "Check" || op == "InspectDisk" {
		return []byte(`{}`), nil
	}
	r := request.Records[0]
	switch op {
	case "Create":
		if _, err := f.c.read(r.Name); err != nil {
			return nil, errors.New("VM side effect preceded durable intent")
		}
		if _, err := os.Stat(filepath.Join(r.Directory, "seed.iso")); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(r.Directory, "os.vhdx"), []byte("differencing disk"), 0600); err != nil {
			return nil, err
		}
		id := uuid.NewString()
		dynamic := false
		f.vms[r.Name] = VM{ID: id, Name: r.Name, Created: r.Created, RunnerID: r.RunnerID, Status: "Off", CPUs: f.c.Config.CPUs, MemoryStartupBytes: int64(f.c.Config.MemoryGiB) << 30, DynamicMemoryEnabled: &dynamic}
		return json.Marshal(map[string]string{"id": id})
	case "Start":
		v := f.vms[r.Name]
		v.Running = true
		v.Status = "Running"
		v.MemoryAssignedBytes = v.MemoryStartupBytes
		f.vms[r.Name] = v
	case "Stop":
		v, ok := f.vms[r.Name]
		if ok {
			v.Running = false
			v.Status = "Off"
			f.vms[r.Name] = v
		}
	case "Remove":
		delete(f.vms, r.Name)
	case "DeleteFiles":
		prefix := filepath.Join(f.c.DataDir, "vms") + string(os.PathSeparator)
		if !strings.HasPrefix(r.Directory, prefix) || !r.Removed {
			return nil, errors.New("unsafe mock deletion")
		}
		return nil, os.RemoveAll(r.Directory)
	}
	return []byte(`{}`), nil
}
func TestProvisionCommandsSeedOwnershipAndCleanup(t *testing.T) {
	c, f, m := fixture(t)
	name := c.newName()
	id, err := c.Create(context.Background(), name, m.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.read(name)
	if err != nil || r.VMID != id || r.RunnerID != 42 || r.Image != m.Path {
		t.Fatal("incomplete durable VM identity", r, err)
	}
	b, err := os.ReadFile(filepath.Join(r.Directory, "seed.iso"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"#cloud-config", "ssh_authorized_keys", "dhcp4: false", "192.168.231.10/24", "--no-block"} {
		if !strings.Contains(string(b), required) {
			t.Fatal("seed missing", required)
		}
	}
	if err = c.Start(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	f.sshOutput = "guest-ready\ncloud-finished\ncloud-ok\n"
	life, err := c.Lifecycle(context.Background(), name)
	if err != nil || !life.Ready {
		t.Fatal("guest readiness", life, err)
	}
	if _, err = os.Stat(filepath.Join(r.Directory, "seed.iso")); !os.IsNotExist(err) {
		t.Fatal("seed keys retained after initialization")
	}
	secret := "synthetic-jit-stdin-only"
	if err = c.Deliver(context.Background(), name, secret); err != nil {
		t.Fatal(err)
	}
	if string(f.stdin) != secret || strings.Contains(strings.Join(f.args, " "), secret) {
		t.Fatal("JIT not restricted to SSH stdin")
	}
	for _, option := range []string{"StrictHostKeyChecking=yes", "HostKeyAlias=" + name, "BatchMode=yes"} {
		if !strings.Contains(strings.Join(f.args, " "), option) {
			t.Fatal("SSH lacks", option)
		}
	}
	b, _ = os.ReadFile(filepath.Join(r.Directory, "record.json"))
	if strings.Contains(string(b), secret) {
		t.Fatal("JIT persisted")
	}
	if err = c.ForgetDiagnostics(name); err == nil {
		t.Fatal("files deleted before durable VM removal")
	}
	if err = c.destroy(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if err = c.destroy(context.Background(), name); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
	if len(f.vms) != 0 {
		t.Fatal("VM leaked")
	}
}
func TestPartialCreateRetainsAndCleansIntent(t *testing.T) {
	for _, failure := range []string{"Admission", "Create", "Start", "Remove"} {
		t.Run(failure, func(t *testing.T) {
			c, f, m := fixture(t)
			name := c.newName()
			f.fail = failure
			_, err := c.Create(context.Background(), name, m.ID, 1)
			if failure == "Admission" {
				if err == nil {
					t.Fatal("capacity bypassed")
				}
				if _, e := os.Stat(c.path(name)); !os.IsNotExist(e) {
					t.Fatal("allocated disk before capacity check")
				}
				return
			}
			if failure == "Create" && err == nil {
				t.Fatal("expected create failure")
			}
			if failure == "Start" {
				if e := c.Start(context.Background(), name); e == nil {
					t.Fatal("start failure hidden")
				}
			}
			if failure == "Remove" {
				if e := c.Remove(context.Background(), name); e == nil {
					t.Fatal("remove failure hidden")
				}
				r, _ := c.read(name)
				if r.Removed {
					t.Fatal("premature removal recorded")
				}
			}
			if _, e := c.read(name); e != nil {
				t.Fatal("lost ownership after partial failure", e)
			}
			f.fail = ""
			if e := c.destroy(context.Background(), name); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestForeignRecordAndUnsafePathFailClosed(t *testing.T) {
	c, f, m := fixture(t)
	name := c.newName()
	if _, err := c.Create(context.Background(), name, m.ID, 1); err != nil {
		t.Fatal(err)
	}
	r, _ := c.read(name)
	r.Owner = uuid.NewString()
	if err := c.save(r); err != nil {
		t.Fatal(err)
	}
	before := len(f.ops)
	if err := c.Remove(context.Background(), name); err == nil {
		t.Fatal("foreign record deleted")
	}
	if len(f.ops) != before {
		t.Fatal("native side effect before ownership check")
	}
	if err := c.ForgetDiagnostics("../foreign"); err == nil {
		t.Fatal("accepted path traversal")
	}
}
func TestAddressAllocationAndConcurrentCreation(t *testing.T) {
	c, _, m := fixture(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.Create(context.Background(), c.newName(), m.ID, 1); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	records, err := c.records()
	if err != nil || len(records) != 8 {
		t.Fatal(records, err)
	}
	seen := map[string]bool{}
	macs := map[string]bool{}
	for _, r := range records {
		if seen[r.IP] || macs[r.MAC] {
			t.Fatal("duplicate VM network identity")
		}
		seen[r.IP] = true
		macs[r.MAC] = true
	}
	if _, err = Allocate("192.168.231.10", "192.168.231.10", records); err == nil {
		t.Fatal("exhausted pool allocated")
	}
	if _, err = Allocate("192.168.231.10", "192.168.231.11", []Record{{IP: "192.168.231.10"}, {IP: "192.168.231.10"}}); err == nil {
		t.Fatal("duplicate durable allocation ignored")
	}
}
func TestImageValidationIntegrityAndAtomicPointer(t *testing.T) {
	c, _, m := fixture(t)
	if err := ValidateImage(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Config.ImageDirectory, "current.json"), []byte(`{"version":"../foreign"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImage(c.Config.ImageDirectory); err == nil {
		t.Fatal("accepted image traversal")
	}
	if err := os.Chmod(m.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateImage(m); err == nil {
		t.Fatal("tampered base image trusted")
	}
	if _, err := loadVersion(c.Config.ImageDirectory, m.Version); err == nil {
		t.Fatal("writable base image trusted")
	}
}
func TestBootProbeFailureIsNotIdleEvidence(t *testing.T) {
	c, _, m := fixture(t)
	name := c.newName()
	if _, err := c.Create(context.Background(), name, m.ID, 1); err != nil {
		t.Fatal(err)
	}
	c.Command = func(context.Context, []byte, string, ...string) ([]byte, error) {
		return nil, errors.New("SSH unavailable")
	}
	life, err := c.Lifecycle(context.Background(), name)
	if err != nil || life.Ready || life.Started {
		t.Fatal("boot unavailability not distinguished", life, err)
	}
	r, _ := c.read(name)
	r.SSHReady = true
	if err = c.save(r); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Lifecycle(context.Background(), name); err == nil {
		t.Fatal("lost contact after readiness treated as idle")
	}
}
func TestNativeErrorsRedactStdin(t *testing.T) {
	if _, err := exec.LookPath("pwsh.exe"); err != nil {
		t.Skip("Windows PowerShell unavailable")
	}
	c, _, _ := fixture(t)
	c.Command = nil
	secret := "synthetic-jit-" + strings.Repeat("x", 120)
	c.Redactor.Add(secret)
	_, err := c.command(context.Background(), []byte(secret), "pwsh.exe", "-NoProfile", "-NonInteractive", "-Command", "$p=[Console]::In.ReadToEnd(); [Console]::Error.WriteLine($p); exit 1")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("process stderr exposed stdin secret", err)
	}
}

func TestFinalEmptyDirectoryRecoveryDoesNotDeleteUnknownFiles(t *testing.T) {
	c, _, _ := fixture(t)
	name := c.newName()
	if err := os.MkdirAll(c.path(name), 0700); err != nil {
		t.Fatal(err)
	}
	if records, err := c.records(); err != nil || len(records) != 0 {
		t.Fatal("empty final-deletion directory not recovered", records, err)
	}
	if err := os.MkdirAll(c.path(name), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.path(name), "unknown.vhdx")
	if err := os.WriteFile(path, []byte("unowned content"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.records(); err == nil {
		t.Fatal("unjournaled disk silently adopted or removed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unknown disk deleted", err)
	}
}

func TestNativeOpenSSHKeysAndSeedRoundTrip(t *testing.T) {
	c, _, _ := fixture(t)
	if _, err := exec.LookPath(c.Config.SSHKeygen); err != nil {
		t.Skip("native Windows OpenSSH unavailable")
	}
	tar, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("independent ISO reader unavailable")
	}
	c.Command = nil
	// Match normal Install/Activate ACL preparation; generic Windows temp dirs
	// can grant other local groups read access, which OpenSSH correctly rejects.
	common, err := filepath.Abs("../../scripts/Common.ps1")
	if err != nil {
		t.Fatal(err)
	}
	protect := ". '" + strings.ReplaceAll(common, "'", "''") + "'; Protect-RunnerDirectory '" + strings.ReplaceAll(c.DataDir, "'", "''") + "'"
	if _, err = c.command(context.Background(), nil, c.Config.PowerShell, "-NoProfile", "-NonInteractive", "-Command", protect); err != nil {
		t.Fatal(err)
	}
	name := c.newName()
	r := Record{Name: name, Nonce: uuid.NewString(), Directory: c.path(name), IP: "192.168.231.10", MAC: "00155DA8E70A", Purpose: "runner"}
	if err = os.MkdirAll(r.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err = c.makeSeed(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	data, err := exec.Command(tar, "-xOf", filepath.Join(r.Directory, "seed.iso"), "user-data").Output()
	if err != nil {
		t.Fatal(err)
	}
	var user struct {
		Keys map[string]string `yaml:"ssh_keys"`
	}
	if err = yaml.Unmarshal(data, &user); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(r.Directory, "extracted-host-key")
	if err = os.WriteFile(privatePath, []byte(user.Keys["ed25519_private"]), 0600); err != nil {
		t.Fatal(err)
	}
	public, err := c.command(context.Background(), nil, c.Config.SSHKeygen, "-y", "-f", privatePath)
	if err != nil {
		t.Fatal("seed host key is not a usable OpenSSH private key", err)
	}
	if strings.TrimSpace(string(public)) != strings.TrimSpace(user.Keys["ed25519_public"]) {
		t.Fatal("seed host keys do not match")
	}
	known, err := os.ReadFile(filepath.Join(r.Directory, "known_hosts"))
	if err != nil || !strings.Contains(string(known), strings.TrimSpace(string(public))) {
		t.Fatal("controller does not pin generated guest key", err)
	}
	if _, err = os.Stat(filepath.Join(r.Directory, "host")); !os.IsNotExist(err) {
		t.Fatal("temporary host private key was not deleted")
	}
}

func TestNativeSSHParsesPinnedKeyPathWithSpaces(t *testing.T) {
	c, f, m := fixture(t)
	if _, err := exec.LookPath(c.Config.SSH); err != nil {
		t.Skip("native Windows OpenSSH unavailable")
	}
	c.DataDir = filepath.Join(c.DataDir, "with spaces")
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	name := c.newName()
	if _, err := c.Create(context.Background(), name, m.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.Deliver(context.Background(), name, "synthetic-jit"); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-G"}, f.args...)
	c.Command = nil
	b, err := c.command(context.Background(), nil, c.Config.SSH, args...)
	if err != nil {
		t.Fatal("SSH configuration rejected", err)
	}
	expected := "userknownhostsfile " + filepath.ToSlash(filepath.Join(c.path(name), "known_hosts"))
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == expected {
			found = true
		}
	}
	if !found {
		t.Fatal("pinned key path split at whitespace")
	}
}
