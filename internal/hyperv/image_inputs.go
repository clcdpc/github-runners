package hyperv

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// This archived verifier is provenance data only; it is never sent to a guest
// or executed. It permits exact reconstruction of the original v1 fingerprint,
// rather than trusting an image ID or rewriting an existing manifest.
//
//go:embed legacy/verify-v1.txt
var legacyImageInputs embed.FS

type ImageInputStatus struct {
	Current       bool     `json:"current"`
	Reason        string   `json:"reason"`
	ChangedInputs []string `json:"changedInputs,omitempty"`
}

func inputDigest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func imageInputDigest(parts map[string]string) string {
	b, _ := json.Marshal(parts) // encoding/json sorts string map keys.
	return inputDigest(append([]byte("clc-image-provisioning-v2\n"), b...))
}

func imageInputs(recipePath string, diskGiB int, scripts fs.FS) (string, map[string]string, error) {
	r, _, err := readRecipe(recipePath)
	if err != nil {
		return "", nil, err
	}
	return provisioningInputs(r, diskGiB, scripts)
}

func provisioningInputs(r Recipe, diskGiB int, scripts fs.FS) (string, map[string]string, error) {
	parts := map[string]string{}
	for name, value := range map[string]string{
		"recipe.version": strconv.Itoa(r.Version), "recipe.sourceUrl": r.SourceURL,
		"recipe.sourceSha256": r.SourceSHA256, "recipe.qemuVersion": r.QemuVersion,
		"diskGiB": strconv.Itoa(diskGiB),
	} {
		parts[name] = inputDigest([]byte(value))
	}
	entries, err := fs.ReadDir(scripts, "guest")
	if err != nil {
		return "", nil, err
	}
	for _, entry := range entries {
		// These run only during build/validation; current verification is also
		// supplied over SSH. provision.sh contains all installed toolchain inputs.
		if entry.Name() == "verify.sh" || entry.Name() == "prepare.sh" {
			continue
		}
		b, err := fs.ReadFile(scripts, "guest/"+entry.Name())
		if err != nil {
			return "", nil, err
		}
		parts["guest/"+entry.Name()] = inputDigest([]byte(strings.ReplaceAll(string(b), "\r\n", "\n")))
	}
	return imageInputDigest(parts), parts, nil
}

func ImageInputs(recipePath string, diskGiB int) (string, error) {
	inputs, _, err := imageInputs(recipePath, diskGiB, guest)
	return inputs, err
}

// Recreate v1 using CURRENT provisioning bytes, the original diagnostic harness
// and archived verifier. Any recipe, disk, service, hook, seal or provisioning
// change changes this digest too. No legacy image gets a blanket exemption.
func legacyImageFingerprint(recipe []byte, diskGiB int, scripts fs.FS) (string, error) {
	provision, err := fs.ReadFile(scripts, "guest/provision.sh")
	if err != nil {
		return "", err
	}
	body, ok := strings.CutPrefix(strings.ReplaceAll(string(provision), "\r\n", "\n"), "#!/usr/bin/env bash\nset -euo pipefail\n")
	if !ok {
		return "", nil // Changed provisioning preamble cannot match a v1 image.
	}
	prepare := "#!/usr/bin/env bash\nset -euo pipefail\nexport DEBIAN_FRONTEND=noninteractive\nexec > >(tee /var/log/clc-image-build.log) 2>&1\ntrap 'touch /home/runner/.clc/failed' ERR\n" + body + "/opt/clc/verify.sh smoke\ntouch /home/runner/.clc/build-ready\n"
	verifier, err := legacyImageInputs.ReadFile("legacy/verify-v1.txt")
	if err != nil {
		return "", err
	}
	entries, err := fs.ReadDir(scripts, "guest")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(recipe)
	fmt.Fprintf(h, "\ndiskGiB=%d\n", diskGiB)
	for _, entry := range entries {
		if entry.Name() == "provision.sh" {
			continue
		}
		b, err := fs.ReadFile(scripts, "guest/"+entry.Name())
		if err != nil {
			return "", err
		}
		switch entry.Name() {
		case "prepare.sh":
			b = []byte(prepare)
		case "verify.sh":
			b = verifier
		}
		h.Write([]byte(entry.Name()))
		h.Write([]byte(strings.ReplaceAll(string(b), "\r\n", "\n")))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func CheckImageInputs(image Image, recipePath string, diskGiB int) (ImageInputStatus, error) {
	return checkImageInputs(image, recipePath, diskGiB, guest)
}

func checkImageInputs(image Image, recipePath string, diskGiB int, scripts fs.FS) (ImageInputStatus, error) {
	r, recipe, err := readRecipe(recipePath)
	if err != nil {
		return ImageInputStatus{}, err
	}
	inputs, parts, err := provisioningInputs(r, diskGiB, scripts)
	if err != nil {
		return ImageInputStatus{}, err
	}
	if image.DiskGiB == diskGiB && image.Inputs == inputs {
		return ImageInputStatus{Current: true, Reason: "provisioning inputs unchanged"}, nil
	}
	if len(image.InputHashes) == 0 && image.DiskGiB == diskGiB {
		// Old recipes were hashed as raw checkout bytes. Reconstruct both text
		// encodings, so Windows/Linux checkout normalization cannot force a build.
		lf := strings.ReplaceAll(string(recipe), "\r\n", "\n")
		for _, raw := range []string{lf, strings.ReplaceAll(lf, "\n", "\r\n")} {
			legacy, err := legacyImageFingerprint([]byte(raw), diskGiB, scripts)
			if err != nil {
				return ImageInputStatus{}, err
			}
			if image.Inputs == legacy {
				return ImageInputStatus{Current: true, Reason: "legacy fingerprint reconstructed exactly; provisioning unchanged; current verifier supplied separately"}, nil
			}
		}
	}
	changed := []string{}
	for key, value := range parts {
		if image.InputHashes[key] != value {
			changed = append(changed, key)
		}
	}
	for key := range image.InputHashes {
		if _, ok := parts[key]; !ok {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	reason := "provisioning inputs changed: " + strings.Join(changed, ", ")
	if len(image.InputHashes) == 0 {
		reason = "legacy fingerprint differs from current provisioning; original manifest has no component hashes to identify individual changes"
		changed = nil
	}
	if image.DiskGiB != diskGiB {
		reason = fmt.Sprintf("diskGiB changed from %d to %d; %s", image.DiskGiB, diskGiB, reason)
	}
	return ImageInputStatus{Reason: reason, ChangedInputs: changed}, nil
}
