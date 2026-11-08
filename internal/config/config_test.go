package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func example(t *testing.T) Config {
	t.Helper()
	c, err := Load("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProductionResourceDefaultsAndExplicitLegacyAllocations(t *testing.T) {
	c := example(t)
	r := c.Resources()
	if r.Minimum != 0 || r.Maximum != 3 || r.CPUs != 2 || r.MemoryGiB != 10 || r.DynamicMemoryEnabled || r.DiskGiB != 20 || r.DiskGrowthGiB != 20 || r.ReserveMemoryGiB != 8 {
		t.Fatal("unexpected production resources", r)
	}
	// Existing explicit hardware remains authoritative until the operator edits it.
	c.HyperV.CPUs, c.HyperV.MemoryGiB, c.Runners.Maximum = 4, 16, 2
	c.HyperV.DiskGiB, c.HyperV.DiskGrowthGiB = 100, 25
	if err := c.Validate(); err != nil {
		t.Fatal("existing allocation is no longer supported", err)
	}
}
func TestValidation(t *testing.T) {
	tests := map[string]func(*Config){
		"repository scope URL":   func(c *Config) { c.GitHub.OrganizationURL += "/repo" },
		"HTTP URL":               func(c *Config) { c.GitHub.OrganizationURL = "http://github.com/clcdpc" },
		"credential URL":         func(c *Config) { c.GitHub.OrganizationURL = "https://token@github.com/clcdpc" },
		"ambiguous URL":          func(c *Config) { c.GitHub.OrganizationURL += "?token=value" },
		"negative minimum":       func(c *Config) { c.Runners.Minimum = -1 },
		"maximum too small":      func(c *Config) { c.Runners.Minimum = c.Runners.Maximum + 1 },
		"maximum zero":           func(c *Config) { c.Runners.Maximum = 0 },
		"unbounded concurrency":  func(c *Config) { c.Runners.Maximum = 1001 },
		"public subnet":          func(c *Config) { c.HyperV.Subnet = "8.8.8.0/24" },
		"unaligned subnet":       func(c *Config) { c.HyperV.Subnet = "192.168.231.10/24" },
		"gateway in pool":        func(c *Config) { c.HyperV.Gateway = c.HyperV.FirstAddress },
		"missing DNS":            func(c *Config) { c.HyperV.DNS = nil },
		"insufficient pool":      func(c *Config) { c.HyperV.LastAddress = c.HyperV.FirstAddress },
		"unsafe scale name":      func(c *Config) { c.GitHub.ScaleSet = "../other" },
		"CPU zero":               func(c *Config) { c.HyperV.CPUs = 0 },
		"invalid memory":         func(c *Config) { c.HyperV.MemoryGiB = 0 },
		"disk below minimum":     func(c *Config) { c.HyperV.DiskGiB = 19 },
		"zero disk growth":       func(c *Config) { c.HyperV.DiskGrowthGiB = 0 },
		"negative disk growth":   func(c *Config) { c.HyperV.DiskGrowthGiB = -1 },
		"excessive disk growth":  func(c *Config) { c.HyperV.DiskGrowthGiB = c.HyperV.DiskGiB + 1 },
		"unsafe startup timeout": func(c *Config) { c.Runners.StartupTimeoutSeconds = 1 },
		"unsafe idle timeout":    func(c *Config) { c.Runners.IdleGraceSeconds = 1 },
		"missing secret path":    func(c *Config) { c.Controller.PATFile = "" },
		"unknown log level":      func(c *Config) { c.Controller.LogLevel = "trace" },
		"duplicate repos":        func(c *Config) { c.GitHub.Repositories = []string{"repo", "REPO"} },
		"owner in repo":          func(c *Config) { c.GitHub.Repositories = []string{"clcdpc/repo"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			c := example(t)
			mutate(&c)
			if c.Validate() == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
func TestDiskGrowthBudget(t *testing.T) {
	b, err := os.ReadFile("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		field string
		want  int
		disk  int
	}{
		{"missing budget", "", 20, 20},
		{"custom budget", `"diskGrowthGiB": 10,`, 10, 20},
		{"full disk budget", `"diskGrowthGiB": 20,`, 20, 20},
		{"explicit legacy allocation", `"diskGrowthGiB": 25,`, 25, 100},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "controller.json")
			data := strings.Replace(string(b), `"diskGrowthGiB": 20,`, tt.field, 1)
			if tt.disk == 100 {
				data = strings.Replace(data, `"diskGiB": 20,`, `"diskGiB": 100,`, 1)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.HyperV.DiskGrowthGiB != tt.want || c.HyperV.DiskGiB != tt.disk {
				t.Fatalf("got disk growth %d GiB and logical disk %d GiB", c.HyperV.DiskGrowthGiB, c.HyperV.DiskGiB)
			}
		})
	}
}
func TestStorageRoot(t *testing.T) {
	b, err := os.ReadFile("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(t.TempDir(), "runtime")
	for _, tt := range []struct {
		name      string
		root      string
		omit      bool
		overrides bool
	}{
		{name: "existing configuration", omit: true},
		{name: "empty root"},
		{name: "default root", root: "."},
		{name: "relative root", root: "runtime"},
		{name: "normalized root", root: "unused/../runtime"},
		{name: "absolute root", root: absolute},
		{name: "absolute overrides", root: "runtime", overrides: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "controller.json")
			var raw map[string]any
			if err := json.Unmarshal(b, &raw); err != nil {
				t.Fatal(err)
			}
			if tt.omit {
				delete(raw, "storageRoot")
			} else {
				raw["storageRoot"] = tt.root
			}
			wantRoot := dir
			if tt.root != "" && tt.root != "." {
				if filepath.IsAbs(tt.root) {
					wantRoot = tt.root
				} else {
					wantRoot = filepath.Join(dir, tt.root)
				}
			}
			wantPaths := wantRoot
			if tt.overrides {
				wantPaths = filepath.Join(dir, "overrides")
				controller := raw["controller"].(map[string]any)
				controller["dataDirectory"] = filepath.Join(wantPaths, "data")
				controller["logDirectory"] = filepath.Join(wantPaths, "logs")
				controller["patFile"] = filepath.Join(wantPaths, "secrets", "github-pat.dpapi")
				raw["hyperv"].(map[string]any)["imageDirectory"] = filepath.Join(wantPaths, "images")
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.StorageRoot != wantRoot || c.Controller.DataDirectory != filepath.Join(wantPaths, "data") || c.Controller.LogDirectory != filepath.Join(wantPaths, "logs") || c.Controller.PATFile != filepath.Join(wantPaths, "secrets", "github-pat.dpapi") || c.HyperV.ImageDirectory != filepath.Join(wantPaths, "images") {
				t.Fatal("storage paths did not follow the root/absolute overrides", c)
			}
			if c.HyperV.Script != filepath.Clean(filepath.Join(dir, "../scripts/HyperV.ps1")) || c.HyperV.ImageRecipe != filepath.Clean(filepath.Join(dir, "../image/recipe.json")) || c.HyperV.QemuImg != raw["hyperv"].(map[string]any)["qemuImg"] {
				t.Fatal("storage root redirected repository inputs or installed tools", c.HyperV)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != string(data) {
				t.Fatal("loading rewrote the configuration", err)
			}
			if _, err := os.Stat(c.Controller.DataDirectory); !os.IsNotExist(err) {
				t.Fatal("loading unexpectedly created runtime storage", err)
			}
			effective, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			effectivePath := path + ".effective.json"
			if err := os.WriteFile(effectivePath, effective, 0600); err != nil {
				t.Fatal(err)
			}
			again, err := Load(effectivePath)
			if err != nil || !reflect.DeepEqual(c, again) {
				t.Fatal("effective snapshot changed resolved storage paths", again, err)
			}
		})
	}
}
func TestStartupRunLevel(t *testing.T) {
	b, err := os.ReadFile("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		field string
		want  string
	}{
		{"existing configuration", "", "Limited"},
		{"limited startup", `"startupRunLevel": "Limited",`, "Limited"},
		{"elevated diagnostics", `"startupRunLevel": "Highest",`, "Highest"},
		{"unknown level", `"startupRunLevel": "System",`, ""},
		{"incorrect case", `"startupRunLevel": "highest",`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "controller.json")
			data := strings.Replace(string(b), `"startupRunLevel": "Limited",`, tt.field, 1)
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if tt.want == "" {
				if err == nil || !strings.Contains(err.Error(), "startupRunLevel must be Limited or Highest") {
					t.Fatalf("unexpected validation result: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Controller.StartupRunLevel != tt.want {
				t.Fatalf("got startup run level %q, want %q", c.Controller.StartupRunLevel, tt.want)
			}
		})
	}
}
func TestStrictFileAndRelativePaths(t *testing.T) {
	b, err := os.ReadFile("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "controller.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Controller.DataDirectory != filepath.Join(dir, "data") || c.Organization() != "clcdpc" {
		t.Fatal("paths must resolve relative to the configuration file")
	}
	for _, bad := range []string{strings.Replace(string(b), `"maximum": 3`, `"maximum": 3, "typo": true`, 1), string(b) + "{}"} {
		if err = os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = Load(path); err == nil {
			t.Fatal("accepted unknown or trailing configuration")
		}
	}
}
