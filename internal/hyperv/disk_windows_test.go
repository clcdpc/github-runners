//go:build windows

package hyperv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestIncompleteBuildCleanupRetainsIntentUntilDiskDeletionSucceeds(t *testing.T) {
	c, _, _ := fixture(t)
	version := "ubuntu2404-20261028T120001Z-123456abcdef"
	dir := filepath.Join(c.Config.ImageDirectory, version)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	intent := filepath.Join(dir, "build-intent.json")
	if err := atomicJSON(intent, buildIntent{Owner: c.Owner, Version: version}); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "converted.vhdx")
	sums := filepath.Join(dir, "SHA256SUMS")
	for _, path := range []string{disk, sums} {
		if err := os.WriteFile(path, []byte("partial build"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	name, err := syscall.UTF16PtrFromString(disk)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a converter still holding the candidate without delete sharing.
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.CloseHandle(handle) })
	if err = c.cleanupIncompleteBuilds(context.Background(), ""); err == nil {
		t.Fatal("locked disk deletion unexpectedly succeeded")
	}
	if _, err = os.Stat(sums); !os.IsNotExist(err) {
		t.Fatal("fixture did not exercise a partially completed deletion", err)
	}
	if _, err = os.Stat(intent); err != nil {
		t.Fatal("failed deletion lost durable ownership", err)
	}
	if err = syscall.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = syscall.InvalidHandle
	if err = c.cleanupIncompleteBuilds(context.Background(), ""); err != nil {
		t.Fatal("cleanup could not resume from its durable intent", err)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("retried cleanup left an incomplete build", err)
	}
}

func TestSparseVHDXRejectedEvenWithUnchangedContent(t *testing.T) {
	c, _, image := fixture(t)
	if err := os.Chmod(image.Path, 0600); err != nil {
		t.Fatal(err)
	}
	// FSCTL_SET_SPARSE changes a real NTFS attribute, not the file bytes/hash.
	if b, err := exec.Command("fsutil.exe", "sparse", "setflag", image.Path).CombinedOutput(); err != nil {
		t.Skipf("temp filesystem cannot create sparse files: %s (%v)", b, err)
	}
	if err := os.Chmod(image.Path, 0444); err != nil {
		t.Fatal(err)
	}
	if sum, err := hashFile(image.Path); err != nil || sum != image.ID {
		t.Fatal("sparse fixture contents changed", err)
	}
	for _, err := range []error{ValidateImage(image), func() error { _, err := LoadImage(c.Config.ImageDirectory); return err }()} {
		if err == nil || !strings.Contains(err.Error(), "SparseFile") || !strings.Contains(err.Error(), image.Path) {
			t.Fatal("hash-valid sparse base was accepted or lacks actionable diagnostics", err)
		}
	}
}

func TestCompressedVHDXRejected(t *testing.T) {
	_, _, image := fixture(t)
	if err := os.Chmod(image.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("compact.exe", "/C", "/I", "/Q", image.Path).CombinedOutput(); err != nil {
		t.Skipf("temp filesystem cannot compress files: %s (%v)", b, err)
	}
	if err := validateVHDXAttributes(image.Path); err == nil || !strings.Contains(err.Error(), "Compressed") {
		t.Fatal("NTFS-compressed VHDX was accepted", err)
	}
}
