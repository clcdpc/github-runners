package restart

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/controller"
	"github.com/clcdpc/github-runners/internal/hyperv"
	"github.com/google/uuid"
)

type watchFixture struct {
	w     *Watcher
	now   time.Time
	c     config.Config
	image hyperv.Image
	m     Manifest
	root  string
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := controller.WriteJSON(path, v); err != nil {
		t.Fatal(err)
	}
}
func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func watcherFixture(t *testing.T) *watchFixture {
	t.Helper()
	root := t.TempDir()
	c, err := config.Load("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Controller.DataDirectory = filepath.Join(root, "data")
	c.Controller.LogDirectory = filepath.Join(root, "logs")
	c.Controller.PATFile = filepath.Join(root, "credential.dpapi")
	c.HyperV.ImageDirectory = filepath.Join(root, "images")
	c.HyperV.Script = filepath.Join(root, "HyperV.ps1")
	c.HyperV.ImageRecipe = filepath.Join(root, "recipe.json")
	for path, value := range map[string]string{c.Controller.PATFile: "encrypted fixture", c.HyperV.Script: "pinned bridge", c.HyperV.ImageRecipe: "recipe", filepath.Join(root, "bin", "clc-runners.exe"): "compiled executable"} {
		writeFile(t, path, value)
	}
	image := hyperv.Image{Schema: 1, Owner: uuid.NewString(), Version: "ubuntu2404-20261101T120000Z-123456abcdef", ID: strings.Repeat("a", 64), Inputs: strings.Repeat("b", 64), SourceSHA256: strings.Repeat("c", 64), Created: time.Now(), Validation: map[string]bool{"cloneBoot": true, "dockerSmoke": true}}
	image.Path = filepath.Join(c.HyperV.ImageDirectory, image.Version, "base.vhdx")
	writeFile(t, image.Path, "non-sparse readonly fixture")
	if err := os.Chmod(image.Path, 0400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(image.Path, 0600) })
	writeJSON(t, filepath.Join(filepath.Dir(image.Path), "image.json"), image)
	writeJSON(t, filepath.Join(c.HyperV.ImageDirectory, "current.json"), map[string]string{"version": image.Version})
	source := filepath.Join(root, "scripts", "Restart.ps1")
	writeFile(t, source, "prepared helper source")
	bundle := filepath.Join(root, "bin", "restart", "fixture")
	helper := filepath.Join(bundle, "scripts", "Restart-Controller.ps1")
	writeFile(t, helper, "prepared helper")
	binary := filepath.Join(root, "bin", "clc-runners.exe")
	writeJSON(t, binary+".build.json", map[string]string{"binary": Digest([]byte("compiled executable"))})
	m := Manifest{Version: 1, Bundle: bundle, Binary: binary, BinaryHash: Digest([]byte("compiled executable")), Files: map[string]string{helper: Digest([]byte("prepared helper"))}, Sources: map[string]string{source: Digest([]byte("prepared helper source"))}}
	for _, name := range []string{"Common.ps1", "Startup.ps1", "Restart.ps1"} {
		path := filepath.Join(bundle, "scripts", name)
		writeFile(t, path, "prepared "+name)
		m.Files[path] = Digest([]byte("prepared " + name))
	}
	path := filepath.Join(root, "controller.json")
	writeJSON(t, path, c)
	manifest := filepath.Join(root, "bin", "restart-runtime.json")
	writeJSON(t, manifest, m)
	now := time.Now()
	w := &Watcher{ConfigPath: path, ManifestPath: manifest, Executable: binary, Running: c, Debounce: 5 * time.Second}
	applied, blocker := w.Capture(now)
	if blocker != "" {
		t.Fatal(blocker)
	}
	w.Applied = applied
	return &watchFixture{w: w, now: now, c: c, image: image, m: m, root: root}
}

func TestWatcherChangeAllowlist(t *testing.T) {
	for _, kind := range []string{"none", "configuration", "executable", "image", "runtime", "irrelevant", "source-only", "credential", "startup-probe"} {
		t.Run(kind, func(t *testing.T) {
			f := watcherFixture(t)
			switch kind {
			case "configuration":
				f.c.Controller.LogLevel = "debug"
				writeJSON(t, f.w.ConfigPath, f.c)
			case "executable":
				writeFile(t, f.m.Binary, "validated replacement")
				f.m.BinaryHash = Digest([]byte("validated replacement"))
				writeJSON(t, f.m.Binary+".build.json", map[string]any{"binary": f.m.BinaryHash, "validated": true})
				writeJSON(t, f.w.ManifestPath, f.m)
			case "image":
				f.image.ID = strings.Repeat("d", 64)
				writeJSON(t, filepath.Join(filepath.Dir(f.image.Path), "image.json"), f.image)
			case "runtime":
				writeFile(t, f.c.HyperV.Script, "changed bridge")
			case "credential":
				writeFile(t, f.c.Controller.PATFile, "rotated encrypted credential")
			case "startup-probe":
				for path := range f.m.Sources {
					writeFile(t, path, "changed startup source")
				}
			case "irrelevant":
				writeFile(t, filepath.Join(f.root, "data", "status.json"), "heartbeat")
				writeFile(t, filepath.Join(f.root, "logs", "controller.log"), "job done")
			case "source-only":
				writeFile(t, filepath.Join(f.root, "cmd", "main.go"), "unvalidated source edit")
			}
			_, _, ready := f.w.Poll(f.now.Add(time.Second))
			if ready {
				t.Fatal("missing debounce")
			}
			target, _, ready := f.w.Poll(f.now.Add(7 * time.Second))
			want := kind != "none" && kind != "irrelevant" && kind != "source-only"
			if ready != want {
				t.Fatalf("kind=%s ready=%v want=%v", kind, ready, want)
			}
			if want {
				f.w.Applied = target
				if _, _, ready = f.w.Poll(f.now.Add(20 * time.Second)); ready {
					t.Fatal("restarted repeatedly for applied identity")
				}
			}
		})
	}
}

func TestEquivalentReceiptAndCandidatePathsDoNotRestartUnchangedRuntime(t *testing.T) {
	f := watcherFixture(t)
	candidate := filepath.Join(f.root, "bin", "staged", "clc-runners.exe")
	writeFile(t, candidate, "compiled executable")
	writeJSON(t, candidate+".build.json", map[string]any{"validated": true, "binary": f.m.BinaryHash, "inputs": "explicitly tested"})
	f.m.Binary = candidate
	writeJSON(t, f.w.ManifestPath, f.m)
	_, _, _ = f.w.Poll(f.now.Add(time.Second))
	target, blocker, changed := f.w.Poll(f.now.Add(7 * time.Second))
	if changed || blocker != "" || target.Identity != f.w.Applied.Identity {
		t.Fatal("same effective installation restarted because its receipt/path changed", blocker)
	}
}

func TestMultipleInvalidDependenciesHaveOneStableBlockedIdentity(t *testing.T) {
	f := watcherFixture(t)
	f.c.HyperV.PowerShell = filepath.Join(f.root, "missing-pwsh.exe")
	f.c.HyperV.SSH = filepath.Join(f.root, "missing-ssh.exe")
	f.c.HyperV.SSHKeygen = filepath.Join(f.root, "missing-keygen.exe")
	writeJSON(t, f.w.ConfigPath, f.c)
	first, blocker := f.w.Capture(f.now)
	if blocker == "" {
		t.Fatal("missing tools were accepted")
	}
	for i := 1; i < 20; i++ {
		next, reason := f.w.Capture(f.now.Add(time.Duration(i) * time.Second))
		if next.Identity != first.Identity || reason != blocker {
			t.Fatal("unchanged invalid inputs kept resetting debounce/failure identity")
		}
	}
}

func TestPartialInvalidAndRapidChangesAreDebouncedAndBlocked(t *testing.T) {
	f := watcherFixture(t)
	writeFile(t, f.w.ConfigPath, `{"controller":`)
	_, _, _ = f.w.Poll(f.now.Add(time.Second))
	_, blocker, ready := f.w.Poll(f.now.Add(7 * time.Second))
	if !ready || !strings.Contains(blocker, "Invalid configuration") {
		t.Fatal("invalid config was accepted", ready, blocker)
	}
	for i := 0; i < 4; i++ {
		f.c.Controller.RetrySeconds++
		writeJSON(t, f.w.ConfigPath, f.c)
		if _, _, ready := f.w.Poll(f.now.Add(time.Duration(8+i) * time.Second)); ready {
			t.Fatal("partial rapid save bypassed debounce")
		}
	}
	target, blocker, ready := f.w.Poll(f.now.Add(18 * time.Second))
	if !ready || blocker != "" {
		t.Fatal("latest complete config did not settle", blocker)
	}
	f.c.Controller.LogLevel = "warn"
	writeJSON(t, f.w.ConfigPath, f.c)
	_, _, ready = f.w.Poll(f.now.Add(19 * time.Second))
	if ready {
		t.Fatal("change during pending request skipped debounce")
	}
	latest, _, ready := f.w.Poll(f.now.Add(25 * time.Second))
	if !ready || latest.Identity == target.Identity {
		t.Fatal("additional pending edit lost")
	}
}

func TestHashCacheHasBoundedRecheckAndDetectsSameTimestampEdits(t *testing.T) {
	f := watcherFixture(t)
	st, _ := os.Stat(f.c.HyperV.Script)
	writeFile(t, f.c.HyperV.Script, "edited bridge") // same length
	if err := os.Chtimes(f.c.HyperV.Script, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, _, changed := f.w.Poll(f.now.Add(time.Second)); changed {
		t.Fatal("cache unexpectedly rehashed unchanged metadata")
	}
	target, _ := f.w.Capture(f.now.Add(61 * time.Second))
	if target.RuntimeHash == f.w.Applied.RuntimeHash {
		t.Fatal("bounded full recheck missed same-mtime edit")
	}
}

func TestManifestValidationRejectsTamperingAndTraversal(t *testing.T) {
	f := watcherFixture(t)
	target, _ := f.w.Capture(f.now)
	if _, err := ValidateManifest(f.w.ManifestPath, target.ManifestHash); err != nil {
		t.Fatal(err)
	}
	for file := range f.m.Files {
		writeFile(t, file, "tampered code")
	}
	if _, err := ValidateManifest(f.w.ManifestPath, target.ManifestHash); err == nil {
		t.Fatal("modified helper code executed")
	}
	outside := filepath.Join(f.root, "outside.ps1")
	writeFile(t, outside, "outside")
	f.m.Files = map[string]string{outside: Digest([]byte("outside"))}
	writeJSON(t, f.w.ManifestPath, f.m)
	b, _ := os.ReadFile(f.w.ManifestPath)
	if _, err := ValidateManifest(f.w.ManifestPath, Digest(b)); err == nil {
		t.Fatal("bundle traversal accepted")
	}
	if _, err := ValidateManifest(f.w.ManifestPath, "stale"); err == nil {
		t.Fatal("stale manifest accepted")
	}
}

func TestManifestMustCoverEveryScriptTheHelperExecutes(t *testing.T) {
	f := watcherFixture(t)
	delete(f.m.Files, filepath.Join(f.m.Bundle, "scripts", "Common.ps1"))
	writeJSON(t, f.w.ManifestPath, f.m)
	b, _ := os.ReadFile(f.w.ManifestPath)
	if _, err := ValidateManifest(f.w.ManifestPath, Digest(b)); err == nil {
		t.Fatal("untracked helper dependency could execute")
	}
}

func TestRestartRecordAndFailureRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.json")
	if got := Read(path); got.State != "None" {
		t.Fatal(got)
	}
	s := State{Version: 1, State: "Failed", Pending: true, Target: Target{Identity: "failed-target"}, LastFailure: "helper timeout", LastSuccess: time.Now().UTC()}
	if err := Write(path, s); err != nil {
		t.Fatal(err)
	}
	got := Read(path)
	if got.LastFailure != s.LastFailure || got.Target != s.Target || !got.LastSuccess.Equal(s.LastSuccess) {
		t.Fatal("restart recovery evidence lost", got)
	}
	writeFile(t, path, "{")
	if got = Read(path); got.State != "Failed" || got.Blocker == "" {
		t.Fatal("corrupt handoff record silently discarded")
	}
}

func TestHelperArgumentsKeepPathsAsArguments(t *testing.T) {
	m := Manifest{Bundle: `C:\path with spaces\literal $(code)`}
	args := HelperArguments(m, `C:\request with spaces\restart.json`, true)
	b, _ := json.Marshal(args)
	if !strings.Contains(string(b), "-File") || args[len(args)-1] != "-Check" || args[len(args)-2] != `C:\request with spaces\restart.json` {
		t.Fatal(args)
	}
}
