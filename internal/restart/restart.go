// Package restart owns update identities and the durable handoff record. It
// never owns runners or credentials; those remain in the controller journal.
package restart

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/controller"
	"github.com/clcdpc/github-runners/internal/hyperv"
)

type Manifest struct {
	Version    int               `json:"version"`
	Bundle     string            `json:"bundle"`
	Binary     string            `json:"binary"`
	BinaryHash string            `json:"binaryHash"`
	Files      map[string]string `json:"files"`
	Sources    map[string]string `json:"sources"`
}

type Target struct {
	Identity     string `json:"identity"`
	ConfigHash   string `json:"configHash"`
	PATHash      string `json:"patHash"`
	BinaryHash   string `json:"binaryHash"`
	RuntimeHash  string `json:"runtimeHash"`
	ImageID      string `json:"imageId"`
	ManifestPath string `json:"manifestPath"`
	ManifestHash string `json:"manifestHash"`
}

type State struct {
	Version           int       `json:"version"`
	State             string    `json:"state"`
	Pending           bool      `json:"pending"`
	Automatic         bool      `json:"automatic"`
	Reason            string    `json:"reason,omitempty"`
	RequestID         string    `json:"requestId,omitempty"`
	ControllerNonce   string    `json:"controllerNonce,omitempty"`
	PID               int       `json:"pid,omitempty"`
	StartedAt         time.Time `json:"startedAt,omitempty"`
	Config            string    `json:"config,omitempty"`
	Root              string    `json:"root,omitempty"`
	Executable        string    `json:"executable,omitempty"`
	AccountSID        string    `json:"accountSid,omitempty"`
	Target            Target    `json:"target"`
	DetectedAt        time.Time `json:"detectedAt,omitempty"`
	DrainingAt        time.Time `json:"drainingAt,omitempty"`
	LastAttempt       time.Time `json:"lastAttempt,omitempty"`
	LastSuccess       time.Time `json:"lastSuccess,omitempty"`
	LastFailure       string    `json:"lastFailure,omitempty"`
	Blocker           string    `json:"blocker,omitempty"`
	ActiveAssignments int       `json:"activeAssignments"`
	CleanupPending    int       `json:"cleanupPending"`
	StartAllowed      bool      `json:"startAllowed,omitempty"`
	HelperPID         int       `json:"helperPid,omitempty"`
	ReplacementNonce  string    `json:"replacementNonce,omitempty"`
}

func Read(path string) State {
	s := State{Version: 1, State: "None"}
	b, err := os.ReadFile(path)
	if err == nil && json.Unmarshal(b, &s) != nil {
		return State{Version: 1, State: "Failed", Pending: true, Blocker: "Restart record is corrupt; inspect restart.json before recovery"}
	}
	return s
}
func Write(path string, s State) error { return controller.WriteJSON(path, s) }

type cachedFile struct {
	size    int64
	mod     time.Time
	checked time.Time
	hash    string
}
type Watcher struct {
	ConfigPath, ManifestPath, Executable string
	Running                              config.Config
	Applied                              Target
	cache                                map[string]cachedFile
	candidate                            string
	since                                time.Time
	Debounce                             time.Duration
}

func Digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func (w *Watcher) hash(path string, now time.Time) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if w.cache == nil {
		w.cache = map[string]cachedFile{}
	}
	old, ok := w.cache[path]
	if ok && old.size == st.Size() && old.mod.Equal(st.ModTime()) && now.Sub(old.checked) < time.Minute {
		return old.hash, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := Digest(b)
	w.cache[path] = cachedFile{st.Size(), st.ModTime(), now, h}
	return h, nil
}

// Capture uses the same raw configuration, image identity and SHA256 hashes as
// startup/Activate. It hashes only a fixed allowlist, with stat caching and a
// bounded full recheck to catch tools that preserve timestamps.
func (w *Watcher) Capture(now time.Time) (t Target, blocker string) {
	t.ManifestPath = w.ManifestPath
	parts := map[string]string{}
	record := func(key, path string) string {
		h, err := w.hash(path, now)
		if err != nil {
			h = "unavailable"
			if blocker == "" {
				blocker = "Required update input unavailable: " + key
			}
		}
		parts[key] = h
		return h
	}
	t.ConfigHash = record("config", w.ConfigPath)
	if h, e := w.hash(filepath.Join(w.Running.Controller.DataDirectory, "unattended-validation.json"), now); e == nil {
		parts["unattended-validation"] = h
	}
	desired, err := config.Load(w.ConfigPath)
	if err != nil {
		blocker = "Invalid configuration; correct and save the configuration"
		desired = w.Running
	}
	if controller.Scope(desired) != controller.Scope(w.Running) || desired.Controller.DataDirectory != w.Running.Controller.DataDirectory || desired.Controller.PATFile != w.Running.Controller.PATFile {
		blocker = "Ownership, data directory or credential path changed; explicit stopped activation required"
	}
	t.PATHash = record("credential", desired.Controller.PATFile)
	t.RuntimeHash = record("runtime", desired.HyperV.Script)
	record("recipe", desired.HyperV.ImageRecipe)
	record("image-manifest", filepath.Join(desired.HyperV.ImageDirectory, "current.json"))
	if image, e := hyperv.LoadImage(desired.HyperV.ImageDirectory); e == nil {
		t.ImageID = image.ID
		record("image-provenance", filepath.Join(filepath.Dir(image.Path), "image.json"))
		// Hash small immutable provenance; the large VHDX is validated once by the
		// preflight/helper, never on every poll.
		if st, e := os.Stat(image.Path); e == nil {
			parts["image-file"] = fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
		} else {
			blocker = "Published image unavailable"
		}
	} else {
		blocker = "Published image is invalid or unavailable; explicit image preparation required"
	}
	parts["imageId"] = t.ImageID
	t.ManifestHash = record("restart-runtime", w.ManifestPath)
	delete(parts, "restart-runtime") // receipt serialization/path is not an effective runtime change
	b, e := os.ReadFile(w.ManifestPath)
	var m Manifest
	if e != nil || json.Unmarshal(b, &m) != nil || m.Version != 1 || m.Bundle == "" || len(m.Files) == 0 {
		blocker = "Restart runtime is not prepared; run Activate"
	}
	binary := m.Binary
	if binary == "" {
		binary = w.Executable
	}
	t.BinaryHash = record("binary", binary)
	metadataHash := record("build-metadata", binary+".build.json")
	delete(parts, "build-metadata")
	var metadata struct {
		Binary    string `json:"binary"`
		Validated bool   `json:"validated"`
	}
	metadataBytes, metadataErr := os.ReadFile(binary + ".build.json")
	if metadataErr != nil || json.Unmarshal(metadataBytes, &metadata) != nil || !strings.EqualFold(metadata.Binary, t.BinaryHash) || (binary != w.Executable && !metadata.Validated) {
		parts["build-validation"] = "invalid:" + metadataHash
		blocker = "Executable lacks matching validated installation metadata; run Activate"
	} else {
		parts["build-validation"] = "valid"
	}
	// Installed binary replacement is relevant even while a staged candidate exists.
	record("installed-binary", w.Executable)
	for path, expected := range m.Sources {
		if record("source:"+path, path) != strings.ToLower(expected) {
			blocker = "Startup/runtime scripts changed; run Activate to prepare a validated update"
		}
	}
	for path, expected := range m.Files {
		relative, _ := filepath.Rel(m.Bundle, path)
		if record("bundle:"+relative, path) != strings.ToLower(expected) {
			blocker = "Prepared restart bundle was modified; run Activate to repair it"
		}
	}
	if m.BinaryHash != "" && !strings.EqualFold(t.BinaryHash, m.BinaryHash) {
		blocker = "Candidate executable does not match validated installation metadata"
	}
	for _, tool := range []struct{ key, path string }{{"powershell", desired.HyperV.PowerShell}, {"ssh", desired.HyperV.SSH}, {"ssh-keygen", desired.HyperV.SSHKeygen}} {
		// Absolute tools are fingerprinted. PATH-resolved tools are pinned by the
		// preflight; their resolution is validated before the handoff.
		resolved, e := exec.LookPath(tool.path)
		if e == nil {
			record(tool.key, resolved)
		} else {
			parts[tool.key] = "unavailable"
			if blocker == "" {
				blocker = "Required runtime tool unavailable: " + tool.key
			}
		}
	}
	if blocker != "" {
		parts["blocked-input"] = blocker
	}
	bytes, _ := json.Marshal(parts)
	t.Identity = Digest(bytes)
	return
}

func (w *Watcher) Poll(now time.Time) (Target, string, bool) {
	t, blocker := w.Capture(now)
	if t.Identity == w.Applied.Identity {
		w.candidate = ""
		return t, "", false
	}
	if t.Identity != w.candidate {
		w.candidate = t.Identity
		w.since = now
		return t, blocker, false
	}
	delay := w.Debounce
	if delay == 0 {
		delay = 5 * time.Second
	}
	return t, blocker, now.Sub(w.since) >= delay
}

// ValidateManifest is also used before launching any prepared PowerShell code.
func ValidateManifest(path, expected string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if !strings.EqualFold(Digest(b), expected) {
		return m, errors.New("update changed during handoff; wait for the next stable poll")
	}
	if json.Unmarshal(b, &m) != nil || m.Version != 1 || !filepath.IsAbs(m.Bundle) || !filepath.IsAbs(m.Binary) || len(m.Files) == 0 {
		return m, errors.New("invalid prepared restart runtime")
	}
	for _, name := range []string{"Common.ps1", "Startup.ps1", "Restart.ps1", "Restart-Controller.ps1"} {
		if _, tracked := m.Files[filepath.Join(m.Bundle, "scripts", name)]; !tracked {
			return m, errors.New("restart runtime omits a required executable script checksum")
		}
	}
	for file, hash := range m.Files {
		rel, e := filepath.Rel(m.Bundle, file)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return m, errors.New("restart bundle path escapes its installation")
		}
		data, e := os.ReadFile(file)
		if e != nil || !strings.EqualFold(Digest(data), hash) {
			return m, errors.New("restart bundle checksum mismatch")
		}
	}
	return m, nil
}
