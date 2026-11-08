package hyperv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func imageScriptsFixture(t *testing.T) fstest.MapFS {
	t.Helper()
	entries, err := guest.ReadDir("guest")
	if err != nil {
		t.Fatal(err)
	}
	scripts := fstest.MapFS{}
	for _, entry := range entries {
		b, err := guest.ReadFile("guest/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		scripts["guest/"+entry.Name()] = &fstest.MapFile{Data: b}
	}
	return scripts
}

func TestImageFingerprintScopesProvisioningInputs(t *testing.T) {
	c, _, _ := fixture(t)
	baseline, parts, err := imageInputs(c.Config.ImageRecipe, c.Config.DiskGiB, guest)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"guest/verify.sh", "guest/prepare.sh", "controller.go", "scripts/Activate.ps1", "samples/platform-validation.yml", "internal/hyperv/validation.go", "guest/provision.sh", "guest/runner.sh", "guest/seal.sh", "guest/clc-runner.service", "guest/job-started.sh", "guest/job-completed.sh", "guest/service-control.py", "guest/trace_common.py", "guest/trace-kernel.bt", "guest/trace-kernel.py", "guest/trace-bus.py", "guest/trace-state.py", "guest/clc-trace-kernel.service", "guest/new-input.sh"} {
		t.Run(name, func(t *testing.T) {
			scripts := imageScriptsFixture(t)
			b := []byte("# changed input\n")
			if original, ok := scripts[name]; ok {
				b = append(append([]byte{}, original.Data...), b...)
			}
			scripts[name] = &fstest.MapFile{Data: b}
			changed, _, err := imageInputs(c.Config.ImageRecipe, c.Config.DiskGiB, scripts)
			if err != nil {
				t.Fatal(err)
			}
			provisioning := strings.HasPrefix(name, "guest/") && name != "guest/verify.sh" && name != "guest/prepare.sh"
			if (changed != baseline) != provisioning {
				t.Fatal("incorrect image invalidation", name, changed, baseline)
			}
			status, err := checkImageInputs(Image{Inputs: baseline, InputHashes: parts, DiskGiB: c.Config.DiskGiB}, c.Config.ImageRecipe, c.Config.DiskGiB, scripts)
			if err != nil || status.Current == provisioning {
				t.Fatal(status, err)
			}
			if provisioning && strings.Join(status.ChangedInputs, ",") != name {
				t.Fatal("did not explain exactly which input changed", status)
			}
		})
	}
	scripts := imageScriptsFixture(t)
	for _, file := range scripts {
		file.Data = []byte(strings.ReplaceAll(strings.ReplaceAll(string(file.Data), "\r\n", "\n"), "\n", "\r\n"))
	}
	if normalized, _, err := imageInputs(c.Config.ImageRecipe, c.Config.DiskGiB, scripts); err != nil || normalized != baseline {
		t.Fatal("checkout line endings invalidated image", normalized, err)
	}
	delete(scripts, "guest/job-started.sh")
	if removed, _, err := imageInputs(c.Config.ImageRecipe, c.Config.DiskGiB, scripts); err != nil || removed == baseline {
		t.Fatal("removed provisioning input ignored", removed, err)
	}
}

func TestImageRecipeAndDiskInvalidation(t *testing.T) {
	c, _, _ := fixture(t)
	r, raw, err := readRecipe(c.Config.ImageRecipe)
	if err != nil {
		t.Fatal(err)
	}
	baseline, parts, err := imageInputs(c.Config.ImageRecipe, c.Config.DiskGiB, guest)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"format", "timeout", "sourceUrl", "sourceSha256", "qemuVersion", "diskGiB"} {
		t.Run(change, func(t *testing.T) {
			modified, disk := r, c.Config.DiskGiB
			switch change {
			case "timeout":
				modified.BuildTimeoutMinutes++
			case "sourceUrl":
				modified.SourceURL = strings.Replace(modified.SourceURL, "/release/", "/20261028/", 1)
			case "sourceSha256":
				modified.SourceSHA256 = strings.Repeat("a", 64)
			case "qemuVersion":
				modified.QemuVersion = "11.1.1"
			case "diskGiB":
				disk++
			}
			b, _ := json.MarshalIndent(modified, "", "    ")
			if change == "format" {
				b = []byte(strings.ReplaceAll(string(raw), "\r\n", "\n"))
			}
			path := filepath.Join(t.TempDir(), "recipe.json")
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			status, err := CheckImageInputs(Image{Inputs: baseline, InputHashes: parts, DiskGiB: c.Config.DiskGiB}, path, disk)
			unrelated := change == "format" || change == "timeout"
			if err != nil || status.Current != unrelated {
				t.Fatal(change, status, err)
			}
			if !unrelated && !strings.Contains(status.Reason, change) {
				t.Fatal("changed recipe input not named", status)
			}
		})
	}
}

func TestLegacyImageFingerprintsRejectChangedGuestAndCheckIntegrity(t *testing.T) {
	c, f, image := fixture(t)
	_, recipe, err := readRecipe(c.Config.ImageRecipe)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduced from ec5960e and the on-disk LF recipe. This is the
	// fingerprint saved in ubuntu2404-20261028T221210Z-afc80db927a3/image.json.
	raw := strings.ReplaceAll(string(recipe), "\r\n", "\n")
	originalScripts := imageScriptsFixture(t)
	for name := range originalScripts {
		if strings.HasPrefix(name, "guest/trace") || strings.HasPrefix(name, "guest/clc-trace-") || name == "guest/service-control.py" {
			delete(originalScripts, name)
		}
	}
	for name, archived := range map[string]string{"provision.sh": "provision-v1.txt", "seal.sh": "seal-v1.txt", "clc-runner.service": "clc-runner-v1.txt"} {
		b, err := os.ReadFile(filepath.Join("legacy", archived))
		if err != nil {
			t.Fatal(err)
		}
		originalScripts["guest/"+name].Data = b
	}
	legacy, err := legacyImageFingerprint([]byte(raw), 100, originalScripts)
	if err != nil || legacy != "2b054e95ee248fed24dd608781e5c23c688b03df4252959ad1f3ecd9c12e8ac8" {
		t.Fatal("legacy provenance no longer matches the saved image", legacy, err)
	}
	// The real old image must now be rejected: service-control provisioning changed.
	if status, err := CheckImageInputs(Image{Inputs: legacy, DiskGiB: 100}, c.Config.ImageRecipe, 100); err != nil || status.Current {
		t.Fatal("historical image accepted after attribution helper changed provisioning", status, err)
	}
	// Synthetic v1 metadata matching all current bytes still exercises exact
	// legacy reuse/integrity, without granting a blanket exception to old images.
	currentLegacy, err := legacyImageFingerprint([]byte(raw), 100, guest)
	if err != nil {
		t.Fatal(err)
	}
	c.Config.DiskGiB, image.DiskGiB, image.Inputs = 100, 100, currentLegacy
	manifest := filepath.Join(filepath.Dir(image.Path), "image.json")
	if err := atomicJSON(manifest, image); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	c.Download = func(context.Context, string, string) error { t.Fatal("legacy reuse downloaded a source"); return nil }
	if result, err := c.PrepareImage(context.Background(), false); err != nil || result.Version != image.Version {
		t.Fatal("unchanged legacy provisioning was rebuilt", result, err)
	}
	if len(f.ops) != 0 {
		t.Fatal("reuse performed native mutations", f.ops)
	}
	after, err := os.ReadFile(manifest)
	if err != nil || string(after) != string(before) {
		t.Fatal("reuse rewrote the immutable manifest", err)
	}
	for _, name := range []string{"provision.sh", "seal.sh", "runner.sh", "clc-runner.service", "job-started.sh", "job-completed.sh", "service-control.py"} {
		scripts := imageScriptsFixture(t)
		scripts["guest/"+name].Data = append(append([]byte{}, scripts["guest/"+name].Data...), []byte("# genuine change\n")...)
		status, err := checkImageInputs(image, c.Config.ImageRecipe, 100, scripts)
		if err != nil || status.Current {
			t.Fatal("legacy compatibility ignored provisioning change", name, status, err)
		}
	}
	image.Inputs = strings.Repeat("f", 64)
	if status, err := CheckImageInputs(image, c.Config.ImageRecipe, 100); err != nil || status.Current {
		t.Fatal("unknown legacy fingerprint accepted", status, err)
	}
	if err := os.Chmod(image.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image.Path, []byte("corrupt immutable disk"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(image.Path, 0444); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PrepareImage(context.Background(), false); err == nil || !strings.Contains(err.Error(), "SHA256 mismatch") {
		t.Fatal("legacy compatibility bypassed VHDX integrity", err)
	}
}
