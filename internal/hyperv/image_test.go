package hyperv

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutomatedImagePipelinePromotesOnlyValidatedClone(t *testing.T) {
	for _, failure := range []string{"fresh", "", "checksum", "convert", "resize", "disk-validation", "cloud-init", "cloud-init-unbound", "cloud-init-cancelled", "cloud-init-cleanup", "capture", "clone-smoke", "seal"} {
		t.Run(failure, func(t *testing.T) {
			c, f, previous := fixture(t)
			buildCtx, cancelBuild := context.WithCancel(context.Background())
			defer cancelBuild()
			if failure == "fresh" {
				for _, path := range []string{previous.Path, filepath.Join(filepath.Dir(previous.Path), "image.json"), filepath.Join(c.Config.ImageDirectory, "current.json")} {
					if err := os.Chmod(path, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(filepath.Dir(previous.Path)); err != nil {
					t.Fatal(err)
				}
				previous = Image{}
				failure = ""
			}
			if failure == "disk-validation" {
				f.fail = "InspectDisk"
			}
			if failure == "cloud-init-cleanup" {
				f.fail = "Remove"
			}
			source := []byte("verified synthetic QCOW2")
			sourceDownloads := 0
			var candidates []string
			c.Download = func(_ context.Context, address, path string) error {
				data := source
				if strings.HasSuffix(address, "SHA256SUMS") {
					data = []byte(fmt.Sprintf("%x  ubuntu-24.04-server-cloudimg-amd64.img\n", sha256.Sum256(source)))
					if failure == "checksum" {
						data = []byte(strings.Repeat("0", 64) + "  ubuntu-24.04-server-cloudimg-amd64.img\n")
					}
				} else {
					sourceDownloads++
				}
				return os.WriteFile(path, data, 0600)
			}
			c.Command = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == c.Config.QemuImg {
					t.Fatal("QEMU must populate a Hyper-V-created VHDX through ConvertImage")
				}
				if args[len(args)-1] == "ConvertImage" {
					var request struct {
						Owner, Scale, Root string
						Disk               imageDiskRequest `json:"disk"`
					}
					if err := json.Unmarshal(input, &request); err != nil {
						return nil, err
					}
					if request.Owner != c.Owner || request.Scale != c.Scale || request.Root != filepath.Join(c.DataDir, "vms") {
						t.Fatalf("conversion lost deployment identity: owner=%q scale=%q root=%q", request.Owner, request.Scale, request.Root)
					}
					dir := filepath.Dir(request.Disk.Path)
					var intent struct{ Owner, Version string }
					b, err := os.ReadFile(filepath.Join(dir, "build-intent.json"))
					if err != nil || json.Unmarshal(b, &intent) != nil || intent.Owner != request.Owner || intent.Version != filepath.Base(dir) ||
						request.Disk.Version != intent.Version || filepath.Dir(dir) != c.Config.ImageDirectory || filepath.Base(request.Disk.Path) != "converted.vhdx" {
						t.Fatal("conversion preceded matching durable intent or targeted another directory", string(b), request.Disk, err)
					}
					candidates = append(candidates, dir)
					if _, err := os.Stat(request.Disk.Path); !os.IsNotExist(err) {
						t.Fatal("retry reused a partial VHDX", request.Disk.Path)
					}
					if _, err := os.Stat(request.Disk.Source); err != nil {
						t.Fatal("verified source was not retained", err)
					}
					err = os.WriteFile(request.Disk.Path, []byte("synthetic prepared VHDX"), 0600)
					if failure == "convert" {
						return nil, errors.New("conversion failed after writing a partial VHDX")
					}
					if failure == "resize" {
						return nil, errors.New("Resize-VHD failed after conversion")
					}
					return []byte(`{"validated":true}`), err
				}
				if program == c.Config.SSH {
					command := args[len(args)-1]
					var r Record
					records, err := c.records()
					if err != nil {
						return nil, err
					}
					for _, item := range records {
						if strings.Contains(strings.Join(args, " "), "HostKeyAlias="+item.Name) {
							r = item
						}
					}
					switch {
					case strings.Contains(command, "build-ready"):
						if strings.HasPrefix(failure, "cloud-init") {
							if failure == "cloud-init-cancelled" {
								cancelBuild()
							}
							output := "CLC_BOOT_FAILED\nCLC_VERIFICATION_FAILED exit=43 line=250 phase=Docker command=fixture-verify\nCLC_PROVISIONING_FAILED exit=43 script=/opt/clc/prepare.sh line=38 command=runuser -u runner -- /opt/clc/verify.sh smoke\n"
							if failure == "cloud-init-unbound" {
								output = "CLC_BOOT_FAILED\n/opt/clc/verify.sh: line 143: HOME: unbound variable\n"
							}
							return []byte(output), errors.New("SSH guest provisioning failure")
						}
						return []byte("CLC_BUILD_READY"), nil
					case strings.Contains(command, "guest-ready"):
						return []byte("CLC_CLONE_READY"), nil
					case strings.Contains(command, "machine-id"):
						return []byte("machine-" + r.Nonce), nil
					case strings.Contains(command, "seal.sh"):
						if failure == "seal" {
							return nil, errors.New("seal failed")
						}
						v := f.vms[r.Name]
						v.Running = false
						v.Status = "Off"
						f.vms[r.Name] = v
						return []byte("CLC_IMAGE_SEALED"), nil
					case command == "bash -s -- smoke":
						verifier, _ := guest.ReadFile("guest/verify.sh")
						if string(input) != strings.ReplaceAll(string(verifier), "\r\n", "\n") {
							t.Fatal("clone verification used stale baked checks")
						}
						if failure == "clone-smoke" {
							return nil, errors.New("Docker broken in fresh clone")
						}
						return []byte(successfulValidationOutput(false)), nil
					default:
						if failure == "capture" {
							return []byte("partial guest diagnostics\n"), errors.New("SSH capture failed")
						}
						if !strings.Contains(command, "/var/log/cloud-init.log") || !strings.Contains(command, "/var/log/cloud-init-output.log") {
							t.Fatal("missing cloud-init evidence recovery", command)
						}
						if commandCtxErr := ctx.Err(); commandCtxErr != nil {
							t.Fatal("cancelled context prevented evidence recovery", commandCtxErr)
						}
						return []byte("build diagnostics"), nil
					}
				}
				if args[len(args)-1] == "CreateBuild" {
					var request struct {
						Records []Record `json:"records"`
					}
					if err := json.Unmarshal(input, &request); err != nil {
						return nil, err
					}
					r := request.Records[0]
					if err := os.Rename(r.Image, filepath.Join(r.Directory, "os.vhdx")); err != nil {
						return nil, err
					}
					// Reuse the native VM creation mock; it asserts journal/seed ordering.
					args = append([]string{}, args...)
					args[len(args)-1] = "Create"
				}
				return f.run(ctx, input, program, args...)
			}
			image, err := c.PrepareImage(buildCtx, true)
			current, e := LoadImage(c.Config.ImageDirectory)
			if e != nil {
				t.Fatal(e)
			}
			if failure != "" {
				if err == nil {
					t.Fatal("failed build was reported successful")
				}
				if current.Version != previous.Version {
					t.Fatal("failed image replaced current")
				}
				if strings.HasPrefix(failure, "cloud-init") {
					want := "exit=43"
					if failure == "cloud-init-unbound" {
						want = "HOME: unbound variable"
					}
					b, reportErr := os.ReadFile(filepath.Join(c.DataDir, "image-build.json"))
					var report struct {
						Passed               bool
						Error, DiagnosticLog string
					}
					if reportErr != nil || json.Unmarshal(b, &report) != nil || report.Passed || !strings.Contains(report.Error, want) || !strings.Contains(err.Error(), want) {
						t.Fatal("specific cloud-init error lost", string(b), err, reportErr)
					}
					log, logErr := os.ReadFile(report.DiagnosticLog)
					if logErr != nil || !strings.Contains(string(log), "Image build started") || !strings.Contains(string(log), "build diagnostics") {
						t.Fatal("build evidence overwritten/lost during cleanup", string(log), logErr)
					}
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if current.Version != image.Version || current.Version == previous.Version || !current.Validation["cloneBoot"] {
					t.Fatal("validated clone not atomically promoted")
				}
				if e := ValidateImage(current); e != nil {
					t.Fatal(e)
				}
				if _, e = c.PrepareImage(context.Background(), true); e != nil || sourceDownloads != 1 {
					t.Fatal("forced rebuild did not reuse verified source", sourceDownloads, e)
				}
				c.Download = func(context.Context, string, string) error {
					t.Fatal("unchanged image unnecessarily rebuilt")
					return nil
				}
				if _, e = c.PrepareImage(context.Background(), false); e != nil {
					t.Fatal(e)
				}
			}
			if failure == "convert" || failure == "resize" {
				failedDir := candidates[0]
				failure = ""
				if _, e = c.PrepareImage(context.Background(), true); e != nil || sourceDownloads != 1 {
					t.Fatal("retry did not complete using the retained verified source", sourceDownloads, e)
				}
				if _, e = os.Stat(failedDir); !os.IsNotExist(e) {
					t.Fatal("retry left the incomplete owned build", failedDir, e)
				}
				if len(candidates) != 2 || candidates[0] == candidates[1] {
					t.Fatal("retry did not allocate a fresh owned candidate", candidates)
				}
				if current, e = LoadImage(c.Config.ImageDirectory); e != nil || current.Version == previous.Version {
					t.Fatal("successful retry did not promote the new image", current, e)
				}
			}
			if failure == "cloud-init-cleanup" {
				records, e := c.records()
				if e != nil || len(records) != 1 || !strings.Contains(err.Error(), "cleanup incomplete") {
					t.Fatal("failed cleanup lost its durable recovery record", records, e, err)
				}
				f.fail = ""
				if e = c.destroy(context.Background(), records[0].Name); e != nil {
					t.Fatal(e)
				}
			}
			if len(f.vms) != 0 {
				t.Fatal("build/validation VM leaked", f.vms)
			}
			records, e := c.records()
			if e != nil || len(records) != 0 {
				t.Fatal("temporary image VM resources leaked", records, e)
			}
		})
	}
}

func TestVerifiedSourceCacheAndInterruptedDownloads(t *testing.T) {
	c, _, _ := fixture(t)
	source := []byte("verified Ubuntu source")
	sha := fmt.Sprintf("%x", sha256.Sum256(source))
	first := filepath.Join(c.Config.ImageDirectory, "ubuntu2404-20261028T120001Z-123456abcdef")
	second := filepath.Join(c.Config.ImageDirectory, "ubuntu2404-20261028T120002Z-123456abcdef")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := atomicJSON(filepath.Join(dir, "build-intent.json"), map[string]string{"owner": c.Owner, "version": filepath.Base(dir)}); err != nil {
			t.Fatal(err)
		}
	}
	partial := filepath.Join(first, "source.qcow2")
	if _, err := c.cachedSource(context.Background(), "source", sha, first, func(_ context.Context, _, path string) error {
		if err := os.WriteFile(path, source[:5], 0600); err != nil {
			return err
		}
		return errors.New("download interrupted")
	}); err == nil {
		t.Fatal("partial download accepted")
	}
	downloads := 0
	download := func(_ context.Context, _, path string) error {
		downloads++
		return os.WriteFile(path, source, 0600)
	}
	cache, err := c.cachedSource(context.Background(), "source", sha, second, download)
	if err != nil || downloads != 1 || filepath.Dir(cache) != filepath.Join(c.Config.ImageDirectory, "sources") {
		t.Fatal("complete source was not cached", cache, err)
	}
	if b, err := os.ReadFile(partial); err != nil || string(b) != string(source[:5]) {
		t.Fatal("retry removed/overwrote a previous build's partial file", err)
	}
	if reused, err := c.cachedSource(context.Background(), "source", sha, first, download); err != nil || reused != cache || downloads != 1 {
		t.Fatal("verified asset downloaded again", reused, downloads, err)
	}
	if err := os.WriteFile(cache, []byte("corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.cachedSource(context.Background(), "source", sha, first, download); err == nil || !strings.Contains(err.Error(), cache) || downloads != 1 {
		t.Fatal("corrupt cache was accepted/overwritten instead of diagnosed", downloads, err)
	}
}

func TestSourceReuseRequiresOwnedIntentAndChecksum(t *testing.T) {
	for _, scenario := range []string{"owned", "foreign", "partial"} {
		t.Run(scenario, func(t *testing.T) {
			c, _, _ := fixture(t)
			source := []byte("verified Ubuntu source")
			sha := fmt.Sprintf("%x", sha256.Sum256(source))
			old := filepath.Join(c.Config.ImageDirectory, "ubuntu2404-20261028T120001Z-123456abcdef")
			fresh := filepath.Join(c.Config.ImageDirectory, "ubuntu2404-20261028T120002Z-123456abcdef")
			for _, path := range []string{old, fresh} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			owner := c.Owner
			if scenario == "foreign" {
				owner = "foreign-owner"
			}
			if err := atomicJSON(filepath.Join(old, "build-intent.json"), map[string]string{"owner": owner, "version": filepath.Base(old)}); err != nil {
				t.Fatal(err)
			}
			data := source
			if scenario == "partial" {
				data = source[:5]
			}
			oldSource := filepath.Join(old, "source.qcow2")
			if err := os.WriteFile(oldSource, data, 0600); err != nil {
				t.Fatal(err)
			}
			downloads := 0
			path, err := c.cachedSource(context.Background(), "source", sha, fresh, func(_ context.Context, _, path string) error {
				downloads++
				return os.WriteFile(path, source, 0600)
			})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "owned" {
				if path != filepath.Join(c.Config.ImageDirectory, "sources", sha+".qcow2") || downloads != 0 {
					t.Fatal("complete owned legacy source was not reconciled into the verified cache")
				}
				if b, err := os.ReadFile(path); err != nil || string(b) != string(data) {
					t.Fatal("legacy source contents lost during cache reconciliation", err)
				}
			} else if path == oldSource || downloads != 1 {
				t.Fatal("foreign or partial source was reused")
			}
			if b, err := os.ReadFile(oldSource); scenario != "owned" && (err != nil || string(b) != string(data)) {
				t.Fatal("previous source was modified", err)
			}
		})
	}
}

func TestImageRetryRecoversOnlyOwnedBuildResources(t *testing.T) {
	for _, recovery := range []string{"prepare-running", "prepare-missing", "startup-running", "startup-missing"} {
		t.Run(recovery, func(t *testing.T) {
			missingVM := strings.HasSuffix(recovery, "-missing")
			c, f, previous := fixture(t)
			runner := c.newName()
			if _, err := c.Create(context.Background(), runner, previous.ID, 42); err != nil {
				t.Fatal(err)
			}
			builder := c.newName()
			if _, err := c.create(context.Background(), builder, previous.Path, 0, "image", nil, true); err != nil {
				t.Fatal(err)
			}
			if missingVM {
				delete(f.vms, builder)
			} else if err := c.Start(context.Background(), builder); err != nil {
				t.Fatal(err)
			}
			f.vms["unrelated-vm"] = VM{Name: "unrelated-vm", Running: true}
			foreign := filepath.Join(c.Config.ImageDirectory, "unrelated.txt")
			if err := os.WriteFile(foreign, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			c.Download = func(context.Context, string, string) error { return errors.New("stop after recovery") }
			recoveryLog := filepath.Join(c.LogDir, "images", builder+"-recovery.log")
			c.Command = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == c.Config.SSH {
					if ctx.Err() != nil {
						t.Fatal("recovery reused a cancelled context")
					}
					if missingVM {
						return nil, errors.New("SSH unavailable for missing VM")
					}
					return []byte("Interrupted cloud-init failure evidence\n"), nil
				}
				if args[len(args)-1] == "Stop" {
					b, err := os.ReadFile(recoveryLog)
					want := "Interrupted cloud-init failure evidence"
					if missingVM {
						want = "SSH unavailable for missing VM"
					}
					if err != nil || !strings.Contains(string(b), want) {
						t.Fatal("VM stopped before recovery evidence was retained", string(b), err)
					}
				}
				return f.run(ctx, input, program, args...)
			}
			if strings.HasPrefix(recovery, "startup-") {
				if err := c.RecoverAuxiliary(context.Background()); err != nil {
					t.Fatal("startup recovery failed", err)
				}
			} else {
				if _, err := c.PrepareImage(context.Background(), true); err == nil || !strings.Contains(err.Error(), "stop after recovery") {
					t.Fatal("recovery failed before reaching downloads", err)
				}
			}
			records, err := c.records()
			if err != nil || len(records) != 1 || records[0].Name != runner {
				t.Fatal("recovery removed runner or leaked build resources", records, err)
			}
			if _, ok := f.vms[runner]; !ok || len(f.vms) != 2 || !f.vms["unrelated-vm"].Running {
				t.Fatal("recovery changed an unrelated/job VM", f.vms)
			}
			if b, err := os.ReadFile(foreign); err != nil || string(b) != "preserve" {
				t.Fatal("recovery modified an unrelated file", err)
			}
		})
	}
}

func TestIncompleteImageBuildCleanup(t *testing.T) {
	for _, scenario := range []string{"owned", "active", "current", "published", "foreign-owner", "wrong-version", "missing-intent", "unknown-file", "nested-directory", "referenced-vm", "referenced-record", "reference-error", "invalid-references", "intent-link", "disk-link", "directory-link"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, previous := fixture(t)
			version := "ubuntu2404-20261028T120001Z-123456abcdef"
			dir := filepath.Join(c.Config.ImageDirectory, version)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			intentPath := filepath.Join(dir, "build-intent.json")
			intent := buildIntent{Owner: c.Owner, Version: version}
			if scenario == "foreign-owner" {
				intent.Owner = "a8270034-4ce5-42f1-8b12-749821b3cfb7"
			}
			if scenario == "wrong-version" {
				intent.Version = previous.Version
			}
			if scenario != "missing-intent" {
				if err := atomicJSON(intentPath, intent); err != nil {
					t.Fatal(err)
				}
			}
			disk := filepath.Join(dir, "converted.vhdx")
			if err := os.WriteFile(disk, []byte("incomplete conversion or resize"), 0600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "unrelated.vhdx")
			if err := os.WriteFile(outside, []byte("unrelated disk"), 0600); err != nil {
				t.Fatal(err)
			}
			f.vms["unrelated-vm"] = VM{Name: "unrelated-vm", Running: true}
			active := "another-build"
			wantError := false
			switch scenario {
			case "active":
				active = version
			case "current":
				if err := atomicJSON(filepath.Join(c.Config.ImageDirectory, "current.json"), map[string]string{"version": version}); err != nil {
					t.Fatal(err)
				}
			case "published":
				// A damaged published manifest still requires explicit diagnosis.
				if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte("damaged published manifest"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-file":
				wantError = true
				if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "nested-directory":
				wantError = true
				if err := os.Mkdir(filepath.Join(dir, "unrelated"), 0700); err != nil {
					t.Fatal(err)
				}
			case "referenced-vm":
				base := c.Command
				c.Command = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
					if args[len(args)-1] == "ImageReferences" {
						return json.Marshal([]string{strings.ToUpper(filepath.ToSlash(disk))})
					}
					return base(ctx, input, program, args...)
				}
			case "referenced-record":
				name := c.newName()
				if _, err := c.Create(context.Background(), name, previous.ID, 42); err != nil {
					t.Fatal(err)
				}
				r, err := c.read(name)
				if err != nil {
					t.Fatal(err)
				}
				r.Image = disk
				if err = c.save(r); err != nil {
					t.Fatal(err)
				}
			case "reference-error":
				wantError = true
				f.fail = "ImageReferences"
			case "invalid-references":
				wantError = true
				c.Command = func(context.Context, []byte, string, ...string) ([]byte, error) { return []byte(`null`), nil }
			case "intent-link", "disk-link", "directory-link":
				path, target := intentPath, outside
				if scenario == "disk-link" {
					path = disk
					wantError = true
				}
				if scenario == "directory-link" {
					path = dir
					target = filepath.Dir(outside)
					for _, file := range []string{intentPath, disk} {
						if err := os.Remove(file); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			err := c.cleanupIncompleteBuilds(context.Background(), active)
			if (err != nil) != wantError {
				t.Fatal("unexpected cleanup result", err)
			}
			if scenario == "owned" {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatal("unreferenced incomplete build was retained", err)
				}
				if err := c.cleanupIncompleteBuilds(context.Background(), active); err != nil {
					t.Fatal("repeating cleanup was not safe", err)
				}
			} else if _, err := os.Lstat(dir); err != nil {
				t.Fatal("protected build was deleted", err)
			} else if scenario != "directory-link" {
				if _, err := os.Lstat(disk); err != nil {
					t.Fatal("protected disk was deleted before all checks passed", err)
				}
				if scenario != "missing-intent" {
					if _, err := os.Lstat(intentPath); err != nil {
						t.Fatal("cleanup lost durable ownership intent", err)
					}
				}
			}
			if b, err := os.ReadFile(outside); err != nil || string(b) != "unrelated disk" {
				t.Fatal("cleanup modified an unrelated disk", err)
			}
			if !f.vms["unrelated-vm"].Running {
				t.Fatal("cleanup modified an unrelated VM")
			}
			for _, op := range f.ops {
				if op == "Stop" || op == "Remove" || op == "DeleteFiles" {
					t.Fatal("image directory cleanup attempted VM mutation", f.ops)
				}
			}
			if err := ValidateImage(previous); err != nil {
				t.Fatal("cleanup changed the existing validated image", err)
			}
		})
	}
}

func TestImagePruningKeepsCurrentAndReferencedParents(t *testing.T) {
	c, f, _ := fixture(t)
	versions := []string{"ubuntu2404-20261028T120001Z-123456abcdef", "ubuntu2404-20261028T120002Z-123456abcdef", "ubuntu2404-20261028T120003Z-123456abcdef"}
	images := []Image{}
	for _, version := range versions {
		images = append(images, makeImage(t, c, version))
	}
	base := c.Command
	c.Command = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if args[len(args)-1] == "ImageReferences" {
			return json.Marshal([]string{images[0].Path})
		}
		return base(ctx, input, program, args...)
	}
	if err := c.PruneImages(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(images[0].Path); err != nil {
		t.Fatal("active parent pruned", err)
	}
	if len(f.vms) != 0 {
		t.Fatal("prune created VM")
	}
	c.Command = base
	if err := c.PruneImages(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(images[0].Path); !os.IsNotExist(err) {
		t.Fatal("obsolete unreferenced image retained")
	}
	if current, err := LoadImage(c.Config.ImageDirectory); err != nil || current.Version != versions[2] {
		t.Fatal("current image pruned", err)
	}
}

func TestIntegrationReportRequiresGuestAndCleanupSuccess(t *testing.T) {
	for _, failure := range []string{"", "Start", "Remove"} {
		t.Run(failure, func(t *testing.T) {
			c, f, _ := fixture(t)
			f.fail = failure
			f.sshOutput = "CLC_READY\n" + successfulValidationOutput(true)
			err := c.Integration(context.Background())
			var report struct {
				Passed  bool `json:"passed"`
				Guest   bool `json:"guestValidationPassed"`
				Cleanup bool `json:"cleanupPassed"`
				GitHub  bool `json:"githubWorkflowValidated"`
				Full    bool `json:"fullPlatformTest"`
			}
			b, e := os.ReadFile(filepath.Join(c.DataDir, "hyperv-validation.json"))
			if e != nil || json.Unmarshal(b, &report) != nil {
				t.Fatal("integration report missing", e)
			}
			if report.GitHub {
				t.Fatal("platform test claimed a GitHub workflow")
			}
			if failure == "" {
				if err != nil || !report.Passed || !report.Cleanup || !report.Guest || !report.Full {
					t.Fatal(report, err)
				}
			} else {
				if err == nil || report.Passed || report.Full {
					t.Fatal("failed integration reported success", report, err)
				}
			}
			if failure == "Remove" {
				if report.Cleanup || !report.Guest {
					t.Fatal("software and cleanup results conflated", report)
				}
				f.fail = ""
				records, _ := c.records()
				for _, r := range records {
					if e = c.destroy(context.Background(), r.Name); e != nil {
						t.Fatal(e)
					}
				}
			}
		})
	}
}

func TestIntegrationUsesCurrentChecksAndPreservesEvidenceBeforeCleanup(t *testing.T) {
	for _, failure := range []string{"", "guest", "cancelled", "missing-marker", "missing-sql", "missing-browser", "bare-marker", "smoke-only", "capture", "progress"} {
		t.Run(failure, func(t *testing.T) {
			c, f, image := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "progress" {
				c.Progress = failingProgress{}
			}
			guestCalled, recovered, stopped := false, false, false
			c.Command = func(commandCtx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				command := args[len(args)-1]
				if program == c.Config.SSH {
					switch {
					case strings.Contains(command, "guest-ready"):
						return []byte("CLC_READY\n"), nil
					case command == "bash -s -- full":
						guestCalled = true
						current, _ := guest.ReadFile("guest/verify.sh")
						if string(input) != strings.ReplaceAll(string(current), "\r\n", "\n") || !strings.Contains(string(input), "Chromium launch and page assertion") {
							t.Fatal("validation used stale baked checks instead of current embedded script")
						}
						if failure == "cancelled" {
							cancel()
						}
						if failure == "guest" || failure == "cancelled" || failure == "capture" {
							return []byte("SQL timeout\nSQLCMDPASSWORD=private-guest-value\nexitCode=137 OOMKilled=true\n"), errors.New("synthetic SQL validation failure")
						}
						if failure == "missing-marker" {
							return []byte("Chromium did not finish\n"), nil
						}
						if failure == "bare-marker" {
							return []byte("CLC_VALIDATION_PASSED\n"), nil
						}
						if failure == "smoke-only" {
							return []byte(successfulValidationOutput(false)), nil
						}
						output := successfulValidationOutput(true)
						if failure == "missing-sql" {
							output = strings.ReplaceAll(output, "CLC_VALIDATION_STAGE_PASSED:sql-query-host\n", "")
						}
						if failure == "missing-browser" {
							output = strings.ReplaceAll(output, "CLC_VALIDATION_STAGE_PASSED:chromium-browser\n", "")
						}
						return []byte(output), nil
					case strings.Contains(command, "validation.log"):
						if stopped || commandCtx.Err() != nil {
							t.Fatal("evidence capture happened after shutdown or reused cancelled context")
						}
						if deadline, ok := commandCtx.Deadline(); !ok || time.Until(deadline) > 121*time.Second {
							t.Fatal("diagnostic capture must have an independent bounded deadline")
						}
						recovered = true
						if failure == "capture" {
							return nil, errors.New("SSH diagnostic recovery unavailable")
						}
						return []byte("preserved startup diagnostics\n"), nil
					default:
						t.Fatal("unexpected guest command", command)
					}
				}
				if command == "Stop" {
					stopped = true
					var report struct{ DiagnosticLog string }
					b, err := os.ReadFile(filepath.Join(c.DataDir, "hyperv-validation.json"))
					if err != nil || json.Unmarshal(b, &report) != nil {
						t.Fatal("VM destroyed before durable report", err)
					}
					if strings.Contains(string(b), `"fullPlatformTest": true`) || strings.Contains(string(b), `"passed": true`) {
						t.Fatal("report claimed full success before VM disposal", string(b))
					}
					log, err := os.ReadFile(report.DiagnosticLog)
					if err != nil || !strings.Contains(string(log), "Creating disposable Ubuntu") || strings.Contains(string(log), "private-guest-value") {
						t.Fatal("diagnostics not preserved/redacted before destruction", err, string(log))
					}
					if failure == "guest" || failure == "cancelled" || failure == "missing-marker" {
						if !recovered || !strings.Contains(string(log), "preserved startup diagnostics") {
							t.Fatal("lost guest diagnostics before removal", string(log))
						}
					}
					if failure == "capture" && !strings.Contains(string(b), "diagnosticsCaptureError") {
						t.Fatal("unavailable diagnostics silently ignored", string(b))
					}
				}
				return f.run(commandCtx, input, program, args...)
			}
			err := c.Integration(ctx)
			if guestCalled != (failure != "progress") || !stopped || (err == nil) != (failure == "") {
				t.Fatal("wrong integration result", failure, guestCalled, stopped, err)
			}
			if len(f.vms) != 0 {
				t.Fatal("validation VM leaked", f.vms)
			}
			var report struct {
				Passed, GuestValidationPassed, FullPlatformTest bool
				RequiredStages, CompletedStages                 []string
			}
			b, readErr := os.ReadFile(filepath.Join(c.DataDir, "hyperv-validation.json"))
			if readErr != nil || json.Unmarshal(b, &report) != nil || report.Passed != (failure == "") || report.FullPlatformTest != (failure == "") || report.GuestValidationPassed != (failure == "") {
				t.Fatal("incomplete validation claimed success", report, readErr)
			}
			if strings.Join(report.RequiredStages, " ") != strings.Join(expectedValidationStages(true), " ") {
				t.Fatal("report did not record the mandatory platform contract", report)
			}
			if current, err := LoadImage(c.Config.ImageDirectory); err != nil || current.ID != image.ID {
				t.Fatal("validation changed the immutable base", current, err)
			}
		})
	}
}

func TestStartupProbeCannotClaimFullPlatformValidation(t *testing.T) {
	c, f, _ := fixture(t)
	f.sshOutput = "CLC_READY\n" + successfulValidationOutput(false)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Passed, GuestValidationPassed, CleanupPassed, FullPlatformTest bool
		ValidationMode                                                 string
		CompletedStages                                                []string
	}
	b, err := os.ReadFile(filepath.Join(c.DataDir, "hyperv-startup-probe.json"))
	if err != nil || json.Unmarshal(b, &report) != nil || !report.Passed || !report.GuestValidationPassed || !report.CleanupPassed || report.FullPlatformTest || report.ValidationMode != "smoke" || strings.Join(report.CompletedStages, " ") != strings.Join(expectedValidationStages(false), " ") {
		t.Fatal("startup probe conflated smoke and full validation", report, err)
	}
}
