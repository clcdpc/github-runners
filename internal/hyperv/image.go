package hyperv

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/clcdpc/github-runners/internal/secure"
	"github.com/google/uuid"
)

//go:embed guest/*.sh guest/*.py guest/*.bt guest/*.service
var guest embed.FS

type Recipe struct {
	Version             int    `json:"version"`
	SourceURL           string `json:"sourceUrl"`
	SourceSHA256        string `json:"sourceSha256"`
	QemuVersion         string `json:"qemuVersion"`
	BuildTimeoutMinutes int    `json:"buildTimeoutMinutes"`
}
type Image struct {
	Schema       int               `json:"schema"`
	Owner        string            `json:"owner"`
	Version      string            `json:"version"`
	ID           string            `json:"id"`
	Path         string            `json:"path"`
	Created      time.Time         `json:"created"`
	Inputs       string            `json:"inputs"`
	InputHashes  map[string]string `json:"inputHashes,omitempty"`
	SourceURL    string            `json:"sourceUrl"`
	SourceSHA256 string            `json:"sourceSha256"`
	DiskGiB      int               `json:"diskGiB"`
	Validation   map[string]bool   `json:"validation"`
}

var versionName = regexp.MustCompile(`^ubuntu2404-[0-9]{8}T[0-9]{6}Z-[a-f0-9]{12}$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type buildIntent struct {
	Owner   string `json:"owner"`
	Version string `json:"version"`
}

// A directory name alone never establishes ownership. Links (including Windows
// junctions) cannot provide the durable authority used for reuse or deletion.
func (c *Client) ownsBuild(dir string) bool {
	if owner, err := uuid.Parse(c.Owner); err != nil || owner == uuid.Nil {
		return false
	}
	if !versionName.MatchString(filepath.Base(dir)) || filepath.Clean(filepath.Dir(dir)) != filepath.Clean(c.Config.ImageDirectory) {
		return false
	}
	for _, path := range []string{c.Config.ImageDirectory, dir, filepath.Join(dir, "build-intent.json")} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (path == filepath.Join(dir, "build-intent.json") && !info.Mode().IsRegular()) {
			return false
		}
	}
	var intent buildIntent
	b, err := os.ReadFile(filepath.Join(dir, "build-intent.json"))
	return err == nil && json.Unmarshal(b, &intent) == nil && intent.Owner == c.Owner && intent.Version == filepath.Base(dir)
}

func readRecipe(path string) (Recipe, []byte, error) {
	var r Recipe
	b, err := os.ReadFile(path)
	if err != nil {
		return r, nil, err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&r); err != nil {
		return r, nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return r, nil, errors.New("image recipe must contain one JSON object")
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(r.QemuVersion) {
		return r, nil, errors.New("image recipe requires a pinned QEMU version")
	}
	u, err := url.Parse(r.SourceURL)
	if err != nil || u.Scheme != "https" || u.Host != "cloud-images.ubuntu.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/ubuntu-24.04-server-cloudimg-amd64.img") || r.Version != 1 || r.BuildTimeoutMinutes < 10 || r.BuildTimeoutMinutes > 180 || (r.SourceSHA256 != "" && !shaPattern.MatchString(r.SourceSHA256)) {
		return r, nil, errors.New("invalid image recipe: use official general-purpose Ubuntu 24.04 amd64 QCOW2, SHA256 and a 10..180 minute timeout")
	}
	return r, b, nil
}
func loadVersion(root, version string) (Image, error) {
	var m Image
	if !versionName.MatchString(version) {
		return m, errors.New("invalid base image version")
	}
	dir := filepath.Join(root, version)
	for _, path := range []string{root, dir, filepath.Join(dir, "base.vhdx"), filepath.Join(dir, "image.json")} {
		info, err := os.Lstat(path)
		if err != nil {
			return m, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return m, errors.New("image paths must not be links")
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		return m, err
	}
	if json.Unmarshal(b, &m) != nil || m.Schema != 1 || m.Version != version || m.Path != filepath.Join(dir, "base.vhdx") || !shaPattern.MatchString(m.ID) || !shaPattern.MatchString(m.SourceSHA256) || !shaPattern.MatchString(m.Inputs) || m.Created.IsZero() || !m.Validation["cloneBoot"] || !m.Validation["dockerSmoke"] {
		return m, errors.New("invalid/unvalidated base image manifest")
	}
	if len(m.InputHashes) > 0 && imageInputDigest(m.InputHashes) != m.Inputs {
		return m, errors.New("base image component hashes do not match its input fingerprint")
	}
	if _, err = uuid.Parse(m.Owner); err != nil {
		return m, errors.New("invalid base image ownership")
	}
	info, err := os.Stat(m.Path)
	if err != nil {
		return m, err
	}
	if info.Size() == 0 || info.Mode().Perm()&0200 != 0 {
		return m, errors.New("base VHDX must be nonempty and read-only")
	}
	if err = validateVHDXAttributes(m.Path); err != nil {
		return m, err
	}
	return m, nil
}
func LoadImage(root string) (Image, error) {
	var pointer struct {
		Version string `json:"version"`
	}
	b, err := os.ReadFile(filepath.Join(root, "current.json"))
	if err != nil {
		return Image{}, fmt.Errorf("base image unavailable; run runners.ps1 PrepareImage: %w", err)
	}
	if json.Unmarshal(b, &pointer) != nil {
		return Image{}, errors.New("invalid image promotion pointer")
	}
	return loadVersion(root, pointer.Version)
}
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Rename on one volume; verified complete copy on different configured volumes.
// The source is removed only after the destination file is flushed and promoted.
func moveDisk(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(destination), ".disk-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	before, err := hashFile(source)
	if err != nil {
		return err
	}
	after, err := hashFile(out.Name())
	if err != nil {
		return err
	}
	if before != after {
		return errors.New("cross-volume image copy integrity check failed")
	}
	if err = os.Rename(out.Name(), destination); err != nil {
		return err
	}
	in.Close()
	return os.Remove(source)
}
func ValidateImage(m Image) error {
	if err := validateVHDXAttributes(m.Path); err != nil {
		return err
	}
	sum, err := hashFile(m.Path)
	if err != nil {
		return err
	}
	if sum != m.ID {
		return errors.New("base image SHA256 mismatch; restore immutable VHDX or rebuild; active parents must never be modified")
	}
	return nil
}

func fetch(ctx context.Context, address, path string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.Host != "cloud-images.ubuntu.com" || len(via) > 5 {
			return errors.New("untrusted Ubuntu download redirect")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Ubuntu download returned HTTP %d", response.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, io.LimitReader(response.Body, 4<<30))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// Cache only checksum-verified sources, independently of disposable candidates.
// Partial downloads stay in their unique build directory and are never trusted
// on retry. The manifest is refreshed because the recipe URL may track a release.
func (c *Client) cachedSource(ctx context.Context, address, sha, buildDir string, download func(context.Context, string, string) error) (string, error) {
	root := filepath.Join(c.Config.ImageDirectory, "sources")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Ubuntu source cache must be a regular directory, not a link")
	}
	cached := filepath.Join(root, sha+".qcow2")
	if info, err = os.Lstat(cached); err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("Ubuntu source cache is not a regular file: %s", cached)
		}
		sum, e := hashFile(cached)
		if e != nil {
			return "", e
		}
		if sum != sha {
			return "", fmt.Errorf("cached Ubuntu QCOW2 integrity verification failed: %s; move this corrupt cache file aside and rerun PrepareImage", cached)
		}
		return cached, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	// Reuse a complete source left by an older/interrupted build only after
	// checking both its durable ownership and the newly obtained official SHA.
	entries, err := os.ReadDir(c.Config.ImageDirectory)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !versionName.MatchString(entry.Name()) {
			continue
		}
		dir := filepath.Join(c.Config.ImageDirectory, entry.Name())
		if !c.ownsBuild(dir) {
			continue
		}
		path := filepath.Join(dir, "source.qcow2")
		if info, e := os.Lstat(path); e == nil && info.Mode().IsRegular() {
			if sum, e := hashFile(path); e == nil && sum == sha {
				// Reconcile legacy sources into the independent verified cache
				// before their incomplete build directory can be cleaned up.
				if err = moveDisk(path, cached); err != nil {
					return "", err
				}
				return cached, nil
			}
		}
	}
	archive := filepath.Join(buildDir, "source.qcow2")
	if err = download(ctx, address, archive); err != nil {
		return "", err
	}
	sum, err := hashFile(archive)
	if err != nil {
		return "", err
	}
	if sum != sha {
		return "", errors.New("Ubuntu QCOW2 integrity verification failed")
	}
	if err = os.Rename(archive, cached); err != nil {
		return "", err
	}
	return cached, nil
}

func (c *Client) newName() string {
	return c.Scale + "-" + c.Owner[:8] + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}
func (c *Client) destroy(ctx context.Context, name string) error {
	if _, err := c.read(name); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := c.Stop(ctx, name); err != nil {
		return err
	}
	if err := c.Remove(ctx, name); err != nil {
		return err
	}
	return c.ForgetDiagnostics(name)
}

func (c *Client) sshLong(ctx context.Context, r Record, input []byte, command string) ([]byte, error) {
	executor := &Client{Config: c.Config, Redactor: c.Redactor, Command: c.Command, Progress: c.Progress}
	executor.Config.CommandTimeoutSeconds = 1800
	return executor.ssh(ctx, r, input, command)
}
func (c *Client) waitSSH(ctx context.Context, r Record, command, expected string) error {
	probe := `cloud_state=$(sudo -n cloud-init status --format json 2>/dev/null | jq -r .status)
if test -f /home/runner/.clc/failed || test "$cloud_state" = error; then
  echo CLC_BOOT_FAILED
  sudo -n cat /var/log/clc-image-failure 2>/dev/null || true
  sudo -n tail -c 16384 /var/log/clc-image-build.log /var/log/cloud-init-output.log 2>/dev/null || true
  sudo -n cloud-init status --long 2>/dev/null || true
  exit 1
fi
test "$cloud_state" = done && ` + command
	nextProgress := time.Now()
	for {
		currentProbe := probe
		if !time.Now().Before(nextProgress) {
			if err := c.progress("Waiting for guest SSH, cloud-init and %s", expected); err != nil {
				return fmt.Errorf("validation progress output failed: %w", err)
			}
			if r.Purpose == "image" {
				currentProbe += `
if test "$cloud_state" != done; then sudo -n grep -E '^\+*\[PrepareImage' /var/log/clc-image-build.log 2>/dev/null | tail -n 1; fi`
			}
			nextProgress = time.Now().Add(20 * time.Second)
		}
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		b, err := c.ssh(probeCtx, r, nil, currentProbe)
		cancel()
		if strings.Contains(string(b), "CLC_BOOT_FAILED") {
			detail := provisioningFailure(c.Redactor.Text(string(b)))
			if detail == "" {
				detail = "cloud-init reported failure; inspect the retained cloud-init and image logs"
			}
			return fmt.Errorf("guest cloud-init/tool provisioning failed: %s", c.Redactor.Text(detail))
		}
		if err == nil && strings.Contains(string(b), expected) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest bootstrap timed out; inspect cloud-init/image logs: %w", ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

// PrepareImage automates source verification, conversion, unattended installation,
// sealing, clone boot/smoke validation and atomic promotion. Existing versions are
// never overwritten, so running differencing disks retain their original parent.
func (c *Client) PrepareImage(ctx context.Context, force bool) (result Image, returnErr error) {
	if err := os.MkdirAll(c.Config.ImageDirectory, 0700); err != nil {
		return result, err
	}
	unlock, err := secure.Lock(filepath.Join(c.Config.ImageDirectory, "image.lock"))
	if err != nil {
		return result, err
	}
	defer unlock()
	// Provision from the same recipe snapshot used to compute the manifest.
	recipe, _, err := readRecipe(c.Config.ImageRecipe)
	if err != nil {
		return result, err
	}
	inputs, inputHashes, err := provisioningInputs(recipe, c.Config.DiskGiB, guest)
	if err != nil {
		return result, err
	}
	if current, loadErr := LoadImage(c.Config.ImageDirectory); loadErr == nil {
		if current.Owner != c.Owner {
			return result, errors.New("current base image belongs to a different deployment; use a dedicated imageDirectory")
		}
		status, err := CheckImageInputs(current, c.Config.ImageRecipe, c.Config.DiskGiB)
		if err != nil {
			return result, err
		}
		if status.Current && !force {
			if err = c.progress("Reusing existing image %s: %s; verifying VHDX SHA256", current.Version, status.Reason); err != nil {
				return result, err
			}
			return current, ValidateImage(current)
		}
		reason := status.Reason
		if force {
			reason = "explicit force/no-cache requested"
		}
		if err = c.progress("Rebuilding image %s: %s", current.Version, reason); err != nil {
			return result, err
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return result, loadErr
	} else if err = c.progress("Building image: no published base image exists"); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(recipe.BuildTimeoutMinutes)*time.Minute)
	defer cancel()
	if err = c.Check(ctx); err != nil {
		return result, err
	}
	logDir := filepath.Join(c.LogDir, "images")
	if err = os.MkdirAll(logDir, 0700); err != nil {
		return result, err
	}
	if err = c.RecoverAuxiliary(ctx); err != nil {
		return result, err
	}
	version := "ubuntu2404-" + time.Now().UTC().Format("20060102T150405Z") + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	dir := filepath.Join(c.Config.ImageDirectory, version)
	if err = os.Mkdir(dir, 0700); err != nil {
		return result, err
	}
	if err = atomicJSON(filepath.Join(dir, "build-intent.json"), buildIntent{Owner: c.Owner, Version: version}); err != nil {
		_ = os.Remove(dir) // Still empty: no image operation was attempted.
		return result, err
	}
	log := filepath.Join(logDir, version+".log")
	if err = os.WriteFile(log, []byte("Image build started; no GitHub credentials are used.\n"), 0600); err != nil {
		return result, err
	}
	defer func() {
		if returnErr != nil {
			returnErr = fmt.Errorf("%w; image diagnostics: %s", returnErr, log)
		}
		report := map[string]any{"version": version, "diagnosticLog": log, "passed": returnErr == nil, "checkedAt": time.Now().UTC()}
		if returnErr != nil {
			report["error"] = c.Redactor.Text(returnErr.Error())
		}
		returnErr = errors.Join(returnErr, atomicJSON(filepath.Join(c.DataDir, "image-build.json"), report))
	}()
	c.progress("Preparing %s; diagnostics: %s", version, log)
	sums := filepath.Join(dir, "SHA256SUMS")
	converted := filepath.Join(dir, "converted.vhdx")
	download := c.Download
	if download == nil {
		download = fetch
	}
	c.progress("Fetching official Ubuntu checksum manifest")
	if err = download(ctx, recipe.SourceURL[:strings.LastIndex(recipe.SourceURL, "/")+1]+"SHA256SUMS", sums); err != nil {
		return result, err
	}
	manifest, err := os.ReadFile(sums)
	if err != nil {
		return result, err
	}
	sourceSHA := recipe.SourceSHA256
	for _, line := range strings.Split(string(manifest), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == "ubuntu-24.04-server-cloudimg-amd64.img" {
			if sourceSHA != "" && sourceSHA != fields[0] {
				return result, errors.New("pinned Ubuntu SHA differs from official manifest; update recipe deliberately")
			}
			sourceSHA = fields[0]
		}
	}
	if !shaPattern.MatchString(sourceSHA) {
		return result, errors.New("Ubuntu manifest has no valid SHA256 for the general-purpose cloud image")
	}
	c.progress("Locating/downloading Ubuntu QCOW2 and verifying SHA256 %s", sourceSHA)
	archive, err := c.cachedSource(ctx, recipe.SourceURL, sourceSHA, dir, download)
	if err != nil {
		return result, err
	}
	c.progress("Using verified Ubuntu source %s; recovering incomplete owned candidates", archive)
	if err = c.cleanupIncompleteBuilds(ctx, version); err != nil {
		return result, fmt.Errorf("recover incomplete image builds: %w", err)
	}
	// Reclaim incomplete owned candidates before testing free disk capacity.
	if _, err = c.operation(ctx, "Capacity", nil); err != nil {
		return result, err
	}
	// diskOperation serializes deployment identity as well as configuration. Keep
	// that identity when giving conversion its longer native command timeout.
	converter := &Client{Config: c.Config, Owner: c.Owner, Scale: c.Scale, DataDir: c.DataDir, LogDir: c.LogDir, Command: c.Command, Redactor: c.Redactor}
	converter.Config.CommandTimeoutSeconds = 1800
	c.progress("Converting official QCOW2 to VHDX, resizing and validating disk structure")
	if _, err = converter.diskOperation(ctx, "ConvertImage", nil, &imageDiskRequest{Source: archive, Path: converted, Version: version}); err != nil {
		return result, fmt.Errorf("convert official QCOW2 to VHDX: %w", err)
	}
	scripts := map[string]string{}
	entries, _ := guest.ReadDir("guest")
	for _, e := range entries {
		b, err := guest.ReadFile("guest/" + e.Name())
		if err != nil {
			return result, err
		}
		path := "/opt/clc/" + e.Name()
		if strings.HasSuffix(e.Name(), ".service") {
			path = "/etc/systemd/system/" + e.Name()
		}
		scripts[path] = strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	name := c.newName()
	c.progress("Creating image VM %s; cloud-init will install the toolchain and run smoke verification", name)
	_, err = c.create(ctx, name, converted, 0, "image", scripts, false)
	buildArchived := false
	defer func() {
		if r, e := c.read(name); e == nil {
			if !buildArchived {
				captureCtx, captureCancel := context.WithTimeout(context.Background(), time.Minute)
				detail, captureErr := c.archiveBuild(captureCtx, r, log)
				captureCancel()
				if detail != "" && returnErr != nil && !strings.Contains(returnErr.Error(), detail) {
					returnErr = fmt.Errorf("%w; %s", returnErr, detail)
				}
				if captureErr != nil {
					returnErr = errors.Join(returnErr, fmt.Errorf("image diagnostic capture: %w", captureErr))
				}
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cleanupCancel()
			c.progress("Removing disposable image VM %s; published images are preserved", name)
			if e = c.destroy(cleanupCtx, name); e != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("image VM cleanup incomplete, rerun PrepareImage: %w", e))
			}
		}
	}()
	if err != nil {
		return result, err
	}
	if err = c.Start(ctx, name); err != nil {
		return result, err
	}
	r, err := c.read(name)
	if err != nil {
		return result, err
	}
	if err = c.waitSSH(ctx, r, "test -f /home/runner/.clc/build-ready && echo CLC_BUILD_READY", "CLC_BUILD_READY"); err != nil {
		return result, err
	}
	builderIdentity, err := c.ssh(ctx, r, nil, "cat /etc/machine-id")
	if err != nil {
		return result, err
	}
	if _, err = c.archiveBuild(ctx, r, log); err != nil {
		return result, fmt.Errorf("archive image provisioning evidence: %w", err)
	}
	buildArchived = true
	c.progress("Provisioning and smoke checks passed; sealing machine and SSH identities")
	if _, err = c.operation(ctx, "DetachSeed", []Record{r}); err != nil {
		return result, err
	}
	sealOutput, sealError := c.ssh(ctx, r, nil, "sudo /opt/clc/seal.sh")
	if !strings.Contains(string(sealOutput), "CLC_IMAGE_SEALED") {
		return result, fmt.Errorf("image sealing did not acknowledge identity/credential cleanup: %v", sealError)
	}
	for {
		b, err := c.operation(ctx, "Inventory", []Record{r})
		if err != nil {
			return result, err
		}
		var vms []VM
		if json.Unmarshal(b, &vms) != nil || len(vms) != 1 {
			return result, errors.New("invalid sealing VM inventory")
		}
		if vms[0].Status == "Off" {
			break
		}
		select {
		case <-ctx.Done():
			return result, errors.New("build VM did not shut down cleanly after sealing")
		case <-time.After(2 * time.Second):
		}
	}
	if err = c.Remove(ctx, name); err != nil {
		return result, err
	}
	base := filepath.Join(dir, "base.vhdx")
	if err = moveDisk(filepath.Join(r.Directory, "os.vhdx"), base); err != nil {
		return result, err
	}
	// A cross-volume move can inherit compression/encryption from imageDirectory.
	if _, err = c.diskOperation(ctx, "InspectDisk", nil, &imageDiskRequest{Path: base}); err != nil {
		return result, fmt.Errorf("validate sealed base VHDX before promotion: %w", err)
	}
	if err = os.Chmod(base, 0444); err != nil {
		return result, err
	}
	if err = c.ForgetDiagnostics(name); err != nil {
		return result, err
	}
	// Boot a NEW differencing clone with new machine/SSH/network identities. The
	// candidate cannot become current merely because package installation passed.
	clone := c.newName()
	defer func() {
		if returnErr != nil {
			if r, e := c.read(clone); e == nil {
				captureCtx, captureCancel := context.WithTimeout(context.Background(), time.Minute)
				_, captureErr := c.archiveBuild(captureCtx, r, log)
				captureCancel()
				returnErr = errors.Join(returnErr, captureErr)
			}
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		returnErr = errors.Join(returnErr, c.destroy(cleanupCtx, clone))
	}()
	c.progress("Booting fresh differencing clone %s and checking regenerated identities", clone)
	if _, err = c.create(ctx, clone, base, 0, "validation", nil, true); err != nil {
		return result, err
	}
	if err = c.Start(ctx, clone); err != nil {
		return result, err
	}
	cloneRecord, err := c.read(clone)
	if err != nil {
		return result, err
	}
	if err = c.waitSSH(ctx, cloneRecord, "test -f /home/runner/.clc/guest-ready && echo CLC_CLONE_READY", "CLC_CLONE_READY"); err != nil {
		return result, err
	}
	cloneIdentity, err := c.ssh(ctx, cloneRecord, nil, "cat /etc/machine-id")
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(string(cloneIdentity)) == strings.TrimSpace(string(builderIdentity)) {
		return result, errors.New("clone retained builder machine-id; image cannot be promoted")
	}
	verifier, err := guest.ReadFile("guest/verify.sh")
	if err != nil {
		return result, err
	}
	c.progress("Running current smoke verification as runner in the fresh clone")
	b, err := c.sshLong(ctx, cloneRecord, []byte(strings.ReplaceAll(string(verifier), "\r\n", "\n")), "bash -s -- smoke")
	_, evidenceErr := validationEvidence(string(b), false)
	if err != nil || evidenceErr != nil {
		return result, fmt.Errorf("candidate clone smoke validation failed: %w; %s", errors.Join(err, evidenceErr), provisioningFailure(c.Redactor.Text(string(b))))
	}
	if err = c.destroy(ctx, clone); err != nil {
		return result, err
	}
	id, err := hashFile(base)
	if err != nil {
		return result, err
	}
	result = Image{Schema: 1, Owner: c.Owner, Version: version, ID: id, Path: base, Created: time.Now().UTC(), Inputs: inputs, InputHashes: inputHashes, SourceURL: recipe.SourceURL, SourceSHA256: sourceSHA, DiskGiB: c.Config.DiskGiB, Validation: map[string]bool{"cloneBoot": true, "dockerSmoke": true}}
	if err = atomicJSON(filepath.Join(dir, "image.json"), result); err != nil {
		return result, err
	}
	if err = atomicJSON(filepath.Join(c.Config.ImageDirectory, "current.json"), map[string]string{"version": version}); err != nil {
		return result, err
	}
	c.progress("Promoted validated image %s; diagnostics retained at %s", version, log)
	return result, nil
}

func (c *Client) Integration(ctx context.Context) error { return c.integration(ctx, true) }
func (c *Client) Probe(ctx context.Context) error       { return c.integration(ctx, false) }
func (c *Client) integration(ctx context.Context, full bool) (returnErr error) {
	c.progress("Verifying immutable Ubuntu base image and SHA256 before VM allocation")
	m, err := LoadImage(c.Config.ImageDirectory)
	if err != nil {
		return err
	}
	if err = ValidateImage(m); err != nil {
		return err
	}
	script, err := guest.ReadFile("guest/verify.sh")
	if err != nil {
		return err
	}
	script = []byte(strings.ReplaceAll(string(script), "\r\n", "\n"))
	name := c.newName()
	logPath := filepath.Join(c.LogDir, "validation", name+".log")
	if err = os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	var destination io.Writer = log
	if c.Progress != nil {
		destination = io.MultiWriter(log, c.Progress)
	}
	progress := &progressWriter{out: destination}
	// A fresh executor avoids copying Client's mutex or changing the caller.
	c = &Client{Config: c.Config, Owner: c.Owner, Scale: c.Scale, DataDir: c.DataDir, LogDir: c.LogDir, Redactor: c.Redactor, Command: c.Command, Progress: progress}
	mode := "smoke"
	if full {
		mode = "full"
	}
	report := map[string]any{"checkedAt": time.Now().UTC(), "image": m.ID, "vm": name, "passed": false, "guestValidationPassed": false, "cleanupPassed": false, "githubWorkflowValidated": false, "fullPlatformTest": false,
		"validationMode": mode, "requiredStages": requiredValidationStages(full), "completedStages": []string{}}
	report["diagnosticLog"] = logPath
	reportName := "hyperv-validation.json"
	if !full {
		reportName = "hyperv-startup-probe.json"
	}
	reportPath := filepath.Join(c.DataDir, reportName)
	defer func() {
		// Use a fresh bounded context: a cancelled guest operation must not skip
		// evidence recovery or cleanup. Capture BEFORE stopping/removing the VM.
		if returnErr != nil {
			if r, e := c.read(name); e == nil {
				c.progress("Recovering guest transcript and startup diagnostics before VM removal")
				diagnosticCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				_, e = c.sshLong(diagnosticCtx, r, script, "if test -f /home/runner/.clc/validation.log; then tail -c 1048576 /home/runner/.clc/validation.log; fi; bash -s -- diagnostics")
				cancel()
				if e != nil {
					report["diagnosticsCaptureError"] = c.Redactor.Text(e.Error())
					c.progress("Guest diagnostic recovery failed: %v", e)
				}
			} else if !os.IsNotExist(e) {
				report["diagnosticsCaptureError"] = c.Redactor.Text(e.Error())
				c.progress("Guest diagnostic recovery refused: %v", e)
			}
		}
		if returnErr != nil {
			report["error"] = c.Redactor.Text(returnErr.Error())
		}
		// A durable snapshot and transcript already exist if removal is interrupted.
		returnErr = errors.Join(returnErr, atomicJSON(reportPath, report), progress.failure(), log.Sync())
		c.progress("Removing disposable VM %s", name)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupErr := c.destroy(cleanupCtx, name)
		if cleanupErr == nil {
			if _, e := os.Stat(c.path(name)); !os.IsNotExist(e) {
				cleanupErr = errors.New("integration VM files remain after cleanup")
			}
		}
		returnErr = errors.Join(returnErr, cleanupErr)
		report["cleanupPassed"] = cleanupErr == nil
		c.progress("Cleanup passed=%t. Report: %s; diagnostics: %s", cleanupErr == nil, reportPath, logPath)
		returnErr = errors.Join(returnErr, progress.failure(), log.Sync(), log.Close())
		report["passed"] = returnErr == nil && report["guestValidationPassed"] == true
		report["fullPlatformTest"] = full && report["passed"] == true
		if returnErr != nil {
			report["error"] = c.Redactor.Text(returnErr.Error())
		}
		returnErr = errors.Join(returnErr, atomicJSON(reportPath, report))
	}()
	c.progress("Creating disposable Ubuntu validation VM %s (image %s)", name, m.Version)
	_, err = c.create(ctx, name, m.Path, 0, "validation", nil, true)
	if err != nil {
		return err
	}
	if err = c.Start(ctx, name); err != nil {
		return err
	}
	hardware, err := c.VerifyHardware(ctx, name)
	report["hardware"] = hardware
	report["hardwareVerified"] = err == nil
	if err != nil {
		return err
	}
	c.progress("Verified native VM hardware: %d vCPUs, %d GiB static startup/assigned RAM", hardware.CPUs, hardware.MemoryStartupBytes>>30)
	r, err := c.read(name)
	if err != nil {
		return err
	}
	bootCtx, bootCancel := context.WithTimeout(ctx, 15*time.Minute)
	err = c.waitSSH(bootCtx, r, "test -f /home/runner/.clc/guest-ready && echo CLC_READY", "CLC_READY")
	bootCancel()
	if err != nil {
		return err
	}
	c.progress("Running current %s guest validation", mode)
	b, err := c.sshLong(ctx, r, script, "bash -s -- "+mode)
	completed, evidenceErr := validationEvidence(string(b), full)
	report["completedStages"] = completed
	report["guestValidationPassed"] = err == nil && evidenceErr == nil
	report["output"] = c.Redactor.Text(string(b))
	return errors.Join(err, evidenceErr)
}

func (c *Client) imageReferences(ctx context.Context) (map[string]bool, error) {
	refsJSON, err := c.operation(ctx, "ImageReferences", nil)
	if err != nil {
		return nil, err
	}
	var paths []string
	if json.Unmarshal(refsJSON, &paths) != nil || paths == nil {
		return nil, errors.New("invalid Hyper-V image references; refusing image cleanup")
	}
	records, err := c.records()
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		paths = append(paths, r.Image)
	}
	referenced := map[string]bool{}
	for _, p := range paths {
		referenced[strings.ToLower(filepath.Clean(p))] = true
	}
	return referenced, nil
}

func imageDirectoryReferenced(dir string, referenced map[string]bool) bool {
	prefix := strings.ToLower(filepath.Clean(dir)) + string(os.PathSeparator)
	for path := range referenced {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// Caller must hold image.lock and establish ownership and absence of references.
// Inspect every file before deletion, never recurse, and keep ownership metadata
// until disk deletion succeeds so an interrupted cleanup can safely be retried.
func (c *Client) removeImageDirectory(dir string) error {
	if !versionName.MatchString(filepath.Base(dir)) || filepath.Clean(filepath.Dir(dir)) != filepath.Clean(c.Config.ImageDirectory) {
		return errors.New("image cleanup path escapes image directory")
	}
	for _, path := range []string{c.Config.ImageDirectory, dir} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("image directory is a link or not a directory; refusing deletion")
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	allowed := map[string]bool{"base.vhdx": true, "image.json": true, "SHA256SUMS": true, "build-intent.json": true, "source.qcow2": true, "converted.vhdx": true}
	for _, f := range files {
		info, err := os.Lstat(filepath.Join(dir, f.Name()))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !allowed[f.Name()] {
			return fmt.Errorf("unexpected file in image build %s; refusing deletion", dir)
		}
	}
	remove := func(name string) error {
		path := filepath.Join(dir, name)
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
		return os.Remove(path)
	}
	for _, f := range files {
		if f.Name() != "image.json" && f.Name() != "build-intent.json" {
			if err = remove(f.Name()); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"image.json", "build-intent.json"} {
		for _, f := range files {
			if f.Name() == name {
				if err = remove(name); err != nil {
					return err
				}
			}
		}
	}
	return os.Remove(dir)
}

// PrepareImage holds image.lock. Only abandoned, unpublished builds are eligible;
// published images, the promotion pointer, caches and foreign files are retained.
func (c *Client) cleanupIncompleteBuilds(ctx context.Context, activeVersion string) error {
	var current struct{ Version string }
	b, err := os.ReadFile(filepath.Join(c.Config.ImageDirectory, "current.json"))
	if err == nil {
		if json.Unmarshal(b, &current) != nil || !versionName.MatchString(current.Version) {
			return errors.New("invalid image promotion pointer; refusing image cleanup")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(c.Config.ImageDirectory)
	if err != nil {
		return err
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == activeVersion || entry.Name() == current.Version {
			continue
		}
		dir := filepath.Join(c.Config.ImageDirectory, entry.Name())
		if !c.ownsBuild(dir) {
			continue
		}
		if _, err = os.Lstat(filepath.Join(dir, "image.json")); err == nil {
			continue // Even an invalid published manifest is not a disposable build.
		} else if !os.IsNotExist(err) {
			return err
		}
		candidates = append(candidates, dir)
	}
	if len(candidates) == 0 {
		return nil
	}
	referenced, err := c.imageReferences(ctx)
	if err != nil {
		return err
	}
	for _, dir := range candidates {
		if !imageDirectoryReferenced(dir, referenced) {
			if err = c.removeImageDirectory(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// PruneImages retains current, the newest two validated images, and every parent
// referenced by any registered VM or durable partial-create record. It refuses
// unknown files instead of recursively removing an arbitrary directory tree.
func (c *Client) PruneImages(ctx context.Context) error {
	unlock, err := secure.Lock(filepath.Join(c.Config.ImageDirectory, "image.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	current, err := LoadImage(c.Config.ImageDirectory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	referenced, err := c.imageReferences(ctx)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(c.Config.ImageDirectory)
	if err != nil {
		return err
	}
	versions := []string{}
	for _, e := range entries {
		if e.IsDir() && versionName.MatchString(e.Name()) {
			versions = append(versions, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(versions)))
	validated := 0
	for _, version := range versions {
		if version == current.Version {
			validated++
			continue
		}
		m, err := loadVersion(c.Config.ImageDirectory, version)
		dir := filepath.Join(c.Config.ImageDirectory, version)
		if err == nil {
			validated++
			if validated <= 2 || m.Owner != c.Owner {
				continue
			}
		} else {
			if !c.ownsBuild(dir) {
				continue
			}
		}
		if imageDirectoryReferenced(dir, referenced) {
			continue
		}
		if err = c.removeImageDirectory(dir); err != nil {
			return err
		}
	}
	return nil
}
