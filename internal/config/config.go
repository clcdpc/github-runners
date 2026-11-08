package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type GitHub struct {
	OrganizationURL         string   `json:"organizationUrl"`
	RunnerGroup             string   `json:"runnerGroup"`
	ScaleSet                string   `json:"scaleSet"`
	Repositories            []string `json:"repositories"`
	AllowPublicRepositories bool     `json:"allowPublicRepositories"`
}
type Runners struct {
	Minimum                int `json:"minimum"`
	Maximum                int `json:"maximum"`
	StartupTimeoutSeconds  int `json:"startupTimeoutSeconds"`
	IdleGraceSeconds       int `json:"idleGraceSeconds"`
	CompletionGraceSeconds int `json:"completionGraceSeconds"`
}
type HyperV struct {
	PowerShell            string   `json:"powerShell"`
	Script                string   `json:"script"`
	SSH                   string   `json:"ssh"`
	SSHKeygen             string   `json:"sshKeygen"`
	ImageDirectory        string   `json:"imageDirectory"`
	ImageRecipe           string   `json:"imageRecipe"`
	QemuImg               string   `json:"qemuImg"`
	SwitchName            string   `json:"switchName"`
	NATName               string   `json:"natName"`
	Subnet                string   `json:"subnet"`
	Gateway               string   `json:"gateway"`
	FirstAddress          string   `json:"firstAddress"`
	LastAddress           string   `json:"lastAddress"`
	DNS                   []string `json:"dns"`
	CPUs                  int      `json:"cpus"`
	MemoryGiB             int      `json:"memoryGiB"`
	DiskGiB               int      `json:"diskGiB"`
	DiskGrowthGiB         int      `json:"diskGrowthGiB"`
	ReserveMemoryGiB      int      `json:"reserveMemoryGiB"`
	ReserveDiskGiB        int      `json:"reserveDiskGiB"`
	SecureBoot            bool     `json:"secureBoot"`
	CommandTimeoutSeconds int      `json:"commandTimeoutSeconds"`
}
type Controller struct {
	DataDirectory            string `json:"dataDirectory"`
	LogDirectory             string `json:"logDirectory"`
	PATFile                  string `json:"patFile"`
	StartupRunLevel          string `json:"startupRunLevel,omitempty"`
	ReconcileIntervalSeconds int    `json:"reconcileIntervalSeconds"`
	RetrySeconds             int    `json:"retrySeconds"`
	LogLevel                 string `json:"logLevel"`
	LogRetentionDays         int    `json:"logRetentionDays"`
}
type Config struct {
	StorageRoot string     `json:"storageRoot,omitempty"`
	GitHub      GitHub     `json:"github"`
	Runners     Runners    `json:"runners"`
	HyperV      HyperV     `json:"hyperv"`
	Controller  Controller `json:"controller"`
}

// Resources is the nonsecret, effective allocation recorded at controller startup.
// Values come from the dedicated configuration file, independently of the image.
type Resources struct {
	Minimum              int  `json:"minimum"`
	Maximum              int  `json:"maximum"`
	CPUs                 int  `json:"cpus"`
	MemoryGiB            int  `json:"memoryGiB"`
	DynamicMemoryEnabled bool `json:"dynamicMemoryEnabled"`
	DiskGiB              int  `json:"diskGiB"`
	DiskGrowthGiB        int  `json:"diskGrowthGiB"`
	ReserveMemoryGiB     int  `json:"reserveMemoryGiB"`
	ReserveDiskGiB       int  `json:"reserveDiskGiB"`
}

func (c Config) Resources() Resources {
	return Resources{Minimum: c.Runners.Minimum, Maximum: c.Runners.Maximum,
		CPUs: c.HyperV.CPUs, MemoryGiB: c.HyperV.MemoryGiB, DiskGiB: c.HyperV.DiskGiB,
		DiskGrowthGiB: c.HyperV.DiskGrowthGiB, ReserveMemoryGiB: c.HyperV.ReserveMemoryGiB,
		ReserveDiskGiB: c.HyperV.ReserveDiskGiB}
}

func Load(path string) (Config, error) {
	var c Config
	c.StorageRoot = "."
	c.HyperV.DiskGrowthGiB = 20
	c.Controller.StartupRunLevel = "Limited"
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, errors.New("configuration must contain one JSON object")
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	if c.StorageRoot == "" {
		c.StorageRoot = "."
	}
	if !filepath.IsAbs(c.StorageRoot) {
		c.StorageRoot = filepath.Join(base, c.StorageRoot)
	}
	c.StorageRoot = filepath.Clean(c.StorageRoot)
	// Runtime storage follows storageRoot. Repository inputs stay relative to
	// the configuration file; explicit absolute overrides remain authoritative.
	for _, p := range []*string{&c.Controller.DataDirectory, &c.Controller.LogDirectory, &c.Controller.PATFile, &c.HyperV.ImageDirectory} {
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(c.StorageRoot, *p)
		}
		*p = filepath.Clean(*p)
	}
	for _, p := range []*string{&c.HyperV.ImageRecipe, &c.HyperV.Script} {
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
		*p = filepath.Clean(*p)
	}
	if c.Controller.DataDirectory == c.Controller.LogDirectory {
		return c, errors.New("dataDirectory and logDirectory must differ")
	}
	return c, nil
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func (c Config) Validate() error {
	u, err := url.Parse(c.GitHub.OrganizationURL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !namePattern.MatchString(strings.Trim(u.Path, "/")) {
		return errors.New("organizationUrl must be an HTTPS github.com organization URL")
	}
	if c.GitHub.RunnerGroup == "" || !namePattern.MatchString(c.GitHub.ScaleSet) || len(c.GitHub.ScaleSet) > 40 {
		return errors.New("runnerGroup and a scaleSet name of at most 40 safe characters are required")
	}
	seen := map[string]bool{}
	for _, repo := range c.GitHub.Repositories {
		key := strings.ToLower(repo)
		if !namePattern.MatchString(repo) || seen[key] {
			return errors.New("repositories must be unique repository names without an owner prefix")
		}
		seen[key] = true
	}
	if c.Runners.Minimum < 0 || c.Runners.Maximum < 1 || c.Runners.Maximum > 1000 || c.Runners.Minimum > c.Runners.Maximum {
		return errors.New("runner counts must satisfy 0 <= minimum <= maximum <= 1000 and maximum >= 1")
	}
	if c.Runners.StartupTimeoutSeconds < 60 || c.Runners.IdleGraceSeconds < 30 || c.Runners.CompletionGraceSeconds < 30 {
		return errors.New("startup timeout must be >= 60s and idle/completion grace >= 30s")
	}
	if c.HyperV.PowerShell == "" || c.HyperV.Script == "" || c.HyperV.SSH == "" || c.HyperV.SSHKeygen == "" || c.HyperV.ImageDirectory == "" || c.HyperV.ImageRecipe == "" || c.HyperV.QemuImg == "" || c.HyperV.CPUs < 1 || c.HyperV.MemoryGiB < 2 || c.HyperV.DiskGiB < 20 || c.HyperV.ReserveMemoryGiB < 2 || c.HyperV.ReserveDiskGiB < 5 || c.HyperV.CommandTimeoutSeconds < 15 {
		return errors.New("Hyper-V tools/paths, positive CPUs, memory >= 2 GiB, disk >= 20 GiB, memory reserve >= 2 GiB, disk reserve >= 5 GiB and command timeout >= 15s are required")
	}
	if c.HyperV.DiskGrowthGiB < 1 || c.HyperV.DiskGrowthGiB > c.HyperV.DiskGiB {
		return errors.New("diskGrowthGiB must be >= 1 GiB and <= diskGiB")
	}
	if !namePattern.MatchString(c.HyperV.SwitchName) || !namePattern.MatchString(c.HyperV.NATName) {
		return errors.New("Hyper-V switch and NAT names must be safe resource names")
	}
	prefix, err := netip.ParsePrefix(c.HyperV.Subnet)
	if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsPrivate() || prefix != prefix.Masked() || prefix.Bits() < 16 || prefix.Bits() > 28 {
		return errors.New("Hyper-V subnet must be a canonical private IPv4 /16 through /28 network")
	}
	addresses := []string{c.HyperV.Gateway, c.HyperV.FirstAddress, c.HyperV.LastAddress}
	parsed := make([]netip.Addr, 3)
	for i, value := range addresses {
		parsed[i], err = netip.ParseAddr(value)
		if err != nil || !parsed[i].Is4() || !prefix.Contains(parsed[i]) || parsed[i] == prefix.Addr() || !prefix.Contains(parsed[i].Next()) {
			return errors.New("gateway and allocation range must be usable addresses in the Hyper-V subnet")
		}
	}
	if parsed[1].Compare(parsed[2]) > 0 || (parsed[0].Compare(parsed[1]) >= 0 && parsed[0].Compare(parsed[2]) <= 0) {
		return errors.New("invalid address range or gateway overlaps allocation range")
	}
	count := 0
	for ip := parsed[1]; ip.Compare(parsed[2]) <= 0; ip = ip.Next() {
		count++
	}
	if count < c.Runners.Maximum+1 {
		return errors.New("address pool must accommodate maximum runners plus one image/validation VM")
	}
	if len(c.HyperV.DNS) == 0 {
		return errors.New("explicit guest DNS servers are required; NAT does not supply DHCP or DNS")
	}
	for _, value := range c.HyperV.DNS {
		ip, err := netip.ParseAddr(value)
		if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("DNS servers must be IPv4 addresses")
		}
	}
	if c.Controller.DataDirectory == "" || c.Controller.LogDirectory == "" || c.Controller.PATFile == "" || c.Controller.ReconcileIntervalSeconds < 1 || c.Controller.RetrySeconds < 1 || c.Controller.LogRetentionDays < 1 {
		return errors.New("controller paths, retention and positive reconcile/retry intervals are required")
	}
	if c.Controller.StartupRunLevel != "" && c.Controller.StartupRunLevel != "Limited" && c.Controller.StartupRunLevel != "Highest" {
		return errors.New("controller.startupRunLevel must be Limited or Highest")
	}
	switch c.Controller.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("logLevel must be debug, info, warn or error")
	}
	return nil
}

func (c Config) Organization() string {
	u, _ := url.Parse(c.GitHub.OrganizationURL)
	return strings.Trim(u.Path, "/")
}
