package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clcdpc/github-runners/internal/config"
)

func TestHardwareAndConcurrencyChangesAffectRuntimeIdentity(t *testing.T) {
	c, err := config.Load("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c.Controller.PATFile = filepath.Join(dir, "pat.dpapi")
	if err := os.WriteFile(c.Controller.PATFile, []byte("synthetic encrypted credential"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "controller.json")
	write := func() {
		t.Helper()
		b, _ := json.Marshal(c)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	first, err := runtimeInputs(path, c, "same-base-image")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(){func() { c.HyperV.CPUs++ }, func() { c.HyperV.MemoryGiB++ }, func() { c.Runners.Maximum++ }} {
		change()
		write()
		next, err := runtimeInputs(path, c, "same-base-image")
		if err != nil || next.ConfigHash == first.ConfigHash || next.ImageID != first.ImageID || next.Resources == nil || *next.Resources != c.Resources() {
			t.Fatal("hardware/concurrency change was absent from runtime identity", err)
		}
		first = next
	}
}

func TestRuntimeInputsDetectConfigCredentialAndImageChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	pat := filepath.Join(dir, "credential.dpapi")
	for _, file := range []string{path, pat} {
		if err := os.WriteFile(file, []byte("initial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := config.Config{Controller: config.Controller{PATFile: pat}}
	first, err := runtimeInputs(path, c, "image-one")
	if err != nil || len(first.ConfigHash) != 64 || len(first.PATHash) != 64 || len(first.BinaryHash) != 64 {
		t.Fatal("incomplete runtime fingerprint", first, err)
	}
	if err := os.WriteFile(path, []byte("changed-config"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pat, []byte("changed-encrypted-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := runtimeInputs(path, c, "image-two")
	if err != nil || first.ConfigHash == second.ConfigHash || first.PATHash == second.PATHash || first.ImageID == second.ImageID || first.BinaryHash != second.BinaryHash {
		t.Fatal("runtime did not record the actual inputs", err)
	}
	if err := os.Remove(pat); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeInputs(path, c, "image-two"); err == nil {
		t.Fatal("missing runtime input was silently accepted")
	}
}

func TestScaleLockIdentityIsSharedAcrossConfigurationsAndSessions(t *testing.T) {
	c, err := config.Load("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	first := scaleLockPath(cache, c)
	c.Controller.DataDirectory = "different-data-directory"
	c.HyperV.Subnet = "192.168.232.0/24"
	c.GitHub.OrganizationURL = "https://github.com/CLCDPC/"
	c.GitHub.RunnerGroup = "clc-runners"
	if first != scaleLockPath(cache, c) {
		t.Fatal("same scale set was not fenced across configurations or logon sessions")
	}
	c.GitHub.ScaleSet = "other-scale"
	if first == scaleLockPath(cache, c) || !strings.HasSuffix(first, ".lock") {
		t.Fatal("distinct scale sets share an incorrect lock identity")
	}
}

func TestRuntimeScriptSnapshotIsImmutableAcrossSourceUpdates(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "bridge.ps1")
	if err := os.WriteFile(source, []byte("original bridge"), 0600); err != nil {
		t.Fatal(err)
	}
	hash, err := fileHash(source)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := snapshotRuntime(source, dir, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, []byte("updated bridge"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = snapshotRuntime(source, dir, hash); err == nil {
		t.Fatal("source mutation during startup accepted")
	}
	b, err := os.ReadFile(pinned)
	if err != nil || string(b) != "original bridge" {
		t.Fatal("active bridge changed with source edit", err)
	}
}
