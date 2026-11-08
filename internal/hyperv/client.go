package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/secure"
	"github.com/clcdpc/github-runners/internal/seed"
	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"
)

type Lifecycle struct {
	Ready      bool      `json:"ready"`
	Configured bool      `json:"configured"`
	Listening  bool      `json:"listening"`
	Started    bool      `json:"started"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	Completed  bool      `json:"completed"`
	Failed     bool      `json:"failed"`
	Exited     bool      `json:"exited"`
	Problem    string    `json:"problem,omitempty"`
}
type VM struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	Created              time.Time `json:"created"`
	RunnerID             int       `json:"runnerId"`
	Running              bool      `json:"running"`
	Status               string    `json:"status"`
	CPUs                 int       `json:"cpus,omitempty"`
	MemoryStartupBytes   int64     `json:"memoryStartupBytes,omitempty"`
	MemoryAssignedBytes  int64     `json:"memoryAssignedBytes,omitempty"`
	DynamicMemoryEnabled *bool     `json:"dynamicMemoryEnabled,omitempty"`
}

// Record is an intent, not just a cache of Hyper-V inventory. It precedes disks,
// seed media and VM creation, and survives every partial failure. No JIT lives here.
type Record struct {
	Version     int       `json:"version"`
	Owner       string    `json:"owner"`
	Scale       string    `json:"scale"`
	Name        string    `json:"name"`
	Nonce       string    `json:"nonce"`
	VMID        string    `json:"vmId"`
	VMSID       string    `json:"vmSid"`
	RunnerID    int       `json:"runnerId"`
	Purpose     string    `json:"purpose"`
	Created     time.Time `json:"created"`
	IP          string    `json:"ip"`
	MAC         string    `json:"mac"`
	Image       string    `json:"image"`
	Directory   string    `json:"directory"`
	SeedRemoved bool      `json:"seedRemoved"`
	SSHReady    bool      `json:"sshReady"`
	Removed     bool      `json:"removed"`
}

type Command func(context.Context, []byte, string, ...string) ([]byte, error)
type Client struct {
	Config                        config.HyperV
	Owner, Scale, DataDir, LogDir string
	Redactor                      *secure.Redactor
	Command                       Command // injection boundary for unit tests; defaults to bounded native processes
	Download                      func(context.Context, string, string) error
	Progress                      io.Writer // optional redacted, streaming validation progress
	ProgressOperation             string
	diagnosticLimit               int // archive-only: sanitize before bounded tail retention
	mu                            sync.Mutex
}
type buffer struct {
	bytes.Buffer
	limit     int
	tail      bool
	truncated bool
}

// Do not let the embedded bytes.Buffer StringWriter bypass the retention limit.
func (b *buffer) WriteString(s string) (int, error) { return b.Write([]byte(s)) }

func (b *buffer) Write(p []byte) (int, error) {
	n := len(p)
	b.truncated = b.truncated || b.Len()+n > b.limit
	if b.tail {
		if n >= b.limit {
			b.Reset()
			b.Buffer.Write(p[n-b.limit:])
		} else {
			if excess := b.Len() + n - b.limit; excess > 0 {
				b.Next(excess)
			}
			b.Buffer.Write(p)
		}
		return n, nil
	}
	if b.Len() < b.limit {
		b.Buffer.Write(p[:min(n, b.limit-b.Len())])
	}
	return n, nil
}
func (c *Client) command(ctx context.Context, input []byte, executable string, args ...string) ([]byte, error) {
	if c.diagnosticLimit > 0 {
		return c.diagnosticCommand(ctx, input, executable, args...)
	}
	if c.Command != nil {
		b, err := c.Command(ctx, input, executable, args...)
		if c.Progress != nil && executable == c.Config.SSH {
			out := &buffer{limit: 8 << 20, tail: true}
			stream := &redactedStream{out: io.MultiWriter(out, c.Progress), redactor: c.Redactor}
			_, writeErr := stream.Write(b)
			err = errors.Join(err, writeErr, stream.flush())
			b = out.Bytes()
		}
		return b, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Config.CommandTimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	hideWindow(cmd)
	cmd.Stdin = bytes.NewReader(input)
	out, stderr := &buffer{limit: 8 << 20}, &buffer{limit: 32 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	var streams []*redactedStream
	stop := func() {}
	if c.Progress != nil && executable == c.Config.SSH {
		// Keep the final diagnostics/success marker even after verbose builds.
		out.tail = true
		progress := &progressWriter{out: c.Progress}
		// Redact BEFORE either retention or streaming. Truncating raw output
		// first could leave a partial credential at the capture size boundary.
		streams = []*redactedStream{{out: io.MultiWriter(out, progress), redactor: c.Redactor}, {out: io.MultiWriter(stderr, progress), redactor: c.Redactor}}
		cmd.Stdout = streams[0]
		cmd.Stderr = streams[1]
		stop = validationHeartbeat(ctx, progress)
	}
	err := cmd.Run()
	stop()
	for _, stream := range streams {
		err = errors.Join(err, stream.flush())
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("native operation failed (%v): %s", err, c.Redactor.Text(stderr.String()))
	}
	return out.Bytes(), nil
}
func (c *Client) operation(ctx context.Context, operation string, records []Record) ([]byte, error) {
	return c.diskOperation(ctx, operation, records, nil)
}

type imageDiskRequest struct {
	Source  string `json:"source,omitempty"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

func (c *Client) diskOperation(ctx context.Context, operation string, records []Record, disk *imageDiskRequest) ([]byte, error) {
	input, err := json.Marshal(struct {
		Settings config.HyperV     `json:"settings"`
		Root     string            `json:"root"`
		Owner    string            `json:"owner"`
		Scale    string            `json:"scale"`
		Records  []Record          `json:"records"`
		Disk     *imageDiskRequest `json:"disk,omitempty"`
	}{c.Config, filepath.Join(c.DataDir, "vms"), c.Owner, c.Scale, records, disk})
	if err != nil {
		return nil, err
	}
	return c.command(ctx, input, c.Config.PowerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", c.Config.Script, "-Operation", operation)
}
func (c *Client) path(name string) string { return filepath.Join(c.DataDir, "vms", name) }
func safeName(name, scale, owner string) bool {
	return len(owner) >= 8 && strings.HasPrefix(name, scale+"-"+owner[:8]+"-") && regexp.MustCompile(`^[A-Za-z0-9_.-]+-[a-f0-9]{12}$`).MatchString(name)
}
func (c *Client) read(name string) (Record, error) {
	var r Record
	if !safeName(name, c.Scale, c.Owner) {
		return r, errors.New("invalid owned VM name")
	}
	b, err := os.ReadFile(filepath.Join(c.path(name), "record.json"))
	if err != nil {
		return r, err
	}
	if json.Unmarshal(b, &r) != nil || r.Version != 1 || r.Owner != c.Owner || r.Scale != c.Scale || r.Name != name || r.Directory != c.path(name) || r.Created.IsZero() || (r.Purpose != "runner" && r.Purpose != "image" && r.Purpose != "validation") || r.RunnerID < 0 {
		return r, errors.New("invalid VM ownership record; refusing resource operations")
	}
	if _, err := uuid.Parse(r.Nonce); err != nil {
		return r, errors.New("invalid VM ownership nonce")
	}
	if r.VMID != "" {
		if _, err := uuid.Parse(r.VMID); err != nil {
			return r, errors.New("invalid VM ID")
		}
	}
	if r.VMSID != "" && !regexp.MustCompile(`^S-1-5-83(-[0-9]+)+$`).MatchString(r.VMSID) {
		return r, errors.New("invalid virtual-machine security identity")
	}
	ip, err := netip.ParseAddr(r.IP)
	if err != nil {
		return r, errors.New("invalid VM address")
	}
	first, _ := netip.ParseAddr(c.Config.FirstAddress)
	last, _ := netip.ParseAddr(c.Config.LastAddress)
	if ip.Compare(first) < 0 || ip.Compare(last) > 0 || !regexp.MustCompile(`^00155D[0-9A-F]{6}$`).MatchString(r.MAC) {
		return r, errors.New("VM allocation differs from configured address pool")
	}
	return r, nil
}
func atomicJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".metadata-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func (c *Client) save(r Record) error {
	return atomicJSON(filepath.Join(r.Directory, "record.json"), r)
}
func (c *Client) records() ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(c.DataDir, "vms"))
	if os.IsNotExist(err) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []Record{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("unexpected entry in owned VM directory; inspect before cleanup")
		}
		r, err := c.read(entry.Name())
		if err != nil {
			// A crash after deleting the final manifest can leave only an empty
			// directory. This removes no VM, disk, key or unowned file.
			if os.IsNotExist(err) && safeName(entry.Name(), c.Scale, c.Owner) {
				children, e := os.ReadDir(c.path(entry.Name()))
				if e == nil && len(children) == 0 {
					if e = os.Remove(c.path(entry.Name())); e != nil {
						return nil, e
					}
					continue
				}
			}
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}
func (c *Client) List(ctx context.Context) ([]VM, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	records, err := c.records()
	if err != nil {
		return nil, err
	}
	vms, err := c.inventory(ctx, records)
	if err != nil {
		return nil, err
	}
	result := []VM{}
	for _, v := range vms {
		for _, r := range records {
			if r.Name == v.Name && r.Purpose == "runner" {
				result = append(result, v)
			}
		}
	}
	return result, nil
}

func (c *Client) inventory(ctx context.Context, records []Record) ([]VM, error) {
	b, err := c.operation(ctx, "Inventory", records)
	if err != nil {
		return nil, err
	}
	var vms []VM
	if json.Unmarshal(b, &vms) != nil {
		return nil, errors.New("invalid Hyper-V inventory; refusing cleanup")
	}
	return vms, nil
}
func Allocate(first, last string, records []Record) (string, error) {
	a, err := netip.ParseAddr(first)
	if err != nil {
		return "", err
	}
	z, err := netip.ParseAddr(last)
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for _, r := range records {
		if used[r.IP] {
			return "", errors.New("duplicate durable IP allocations")
		}
		used[r.IP] = true
	}
	for ip := a; ip.IsValid() && ip.Compare(z) <= 0; ip = ip.Next() {
		if !used[ip.String()] {
			return ip.String(), nil
		}
	}
	return "", errors.New("Hyper-V guest address pool exhausted")
}
func (c *Client) Create(ctx context.Context, name, image string, runnerID int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	manifest, err := LoadImage(c.Config.ImageDirectory)
	if err != nil {
		return "", err
	}
	if image != manifest.ID {
		return "", errors.New("current image changed during provisioning; retry activation")
	}
	if manifest.DiskGiB != c.Config.DiskGiB {
		return "", errors.New("configured disk capacity differs from immutable base; run Activate to build the matching image")
	}
	if manifest.Owner != c.Owner {
		return "", errors.New("current base image belongs to a different deployment; use a dedicated imageDirectory and PrepareImage")
	}
	return c.create(ctx, name, manifest.Path, runnerID, "runner", nil, true)
}
func (c *Client) create(ctx context.Context, name, image string, id int, purpose string, extra map[string]string, differencing bool) (string, error) {
	if !safeName(name, c.Scale, c.Owner) {
		return "", errors.New("unsafe VM name")
	}
	records, err := c.records()
	if err != nil {
		return "", err
	}
	ip, err := Allocate(c.Config.FirstAddress, c.Config.LastAddress, records)
	if err != nil {
		return "", err
	}
	admission, err := c.admission(ctx, records)
	if err != nil {
		return "", err
	}
	if !admission.Ready {
		return "", &CapacityError{Admission: admission}
	}
	address, _ := netip.ParseAddr(ip)
	octets := address.As4()
	r := Record{Version: 1, Owner: c.Owner, Scale: c.Scale, Name: name, Nonce: uuid.NewString(), RunnerID: id, Purpose: purpose, Created: time.Now().UTC(), IP: ip, MAC: fmt.Sprintf("00155D%02X%02X%02X", octets[1], octets[2], octets[3]), Image: image, Directory: c.path(name)}
	if err = os.MkdirAll(filepath.Dir(r.Directory), 0700); err != nil {
		return "", err
	}
	if err = os.Mkdir(r.Directory, 0700); err != nil {
		return "", err
	}
	if err = c.save(r); err != nil {
		_ = os.Remove(r.Directory) // Still empty: no native side effect was attempted.
		return "", err
	}
	if err = c.makeSeed(ctx, r, extra); err != nil {
		return "", err
	}
	op := "Create"
	if !differencing {
		op = "CreateBuild"
	}
	b, err := c.operation(ctx, op, []Record{r})
	if err != nil {
		return "", err
	}
	var result struct {
		ID  string `json:"id"`
		SID string `json:"sid"`
	}
	if json.Unmarshal(b, &result) != nil {
		return "", errors.New("Hyper-V create returned invalid VM ID; recover from durable intent")
	}
	if _, err = uuid.Parse(result.ID); err != nil {
		return "", err
	}
	r.VMID = result.ID
	r.VMSID = result.SID
	return result.ID, c.save(r)
}
func (c *Client) makeSeed(ctx context.Context, r Record, extra map[string]string) error {
	for _, key := range []string{"client", "host"} {
		if _, err := c.command(ctx, nil, c.Config.SSHKeygen, "-q", "-t", "ed25519", "-N", "", "-C", r.Name, "-f", filepath.Join(r.Directory, key)); err != nil {
			return err
		}
	}
	client, err := os.ReadFile(filepath.Join(r.Directory, "client.pub"))
	if err != nil {
		return err
	}
	host, err := os.ReadFile(filepath.Join(r.Directory, "host.pub"))
	if err != nil {
		return err
	}
	private, err := os.ReadFile(filepath.Join(r.Directory, "host"))
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(r.Directory, "known_hosts"), []byte(r.Name+" "+strings.TrimSpace(string(host))+"\n"), 0600); err != nil {
		return err
	}
	files := []map[string]any{}
	// Also supply the small attribution helper to new clones of older images.
	// Existing running guests are never modified by diagnostic collection.
	control, err := guest.ReadFile("guest/service-control.py")
	if err != nil {
		return err
	}
	files = append(files, map[string]any{"path": "/opt/clc/service-control.py", "permissions": "0755", "owner": "root:root", "content": strings.ReplaceAll(string(control), "\r\n", "\n")})
	for path, body := range extra {
		if path == "/opt/clc/service-control.py" {
			continue
		}
		permissions := "0755"
		if strings.HasSuffix(path, ".service") {
			permissions = "0644"
		}
		files = append(files, map[string]any{"path": path, "permissions": permissions, "owner": "root:root", "content": body})
	}
	run := []any{[]string{"bash", "-c", "mkdir -p /home/runner/.clc /opt/hostedtoolcache; chown -R runner:runner /home/runner/.clc /opt/hostedtoolcache; python3 /opt/clc/service-control.py start clone-boot --no-block"}}
	if r.Purpose == "image" {
		run = []any{[]string{"bash", "/opt/clc/prepare.sh"}}
	}
	groups := []string{"sudo"}
	if r.Purpose != "image" {
		groups = append(groups, "docker")
	}
	user := map[string]any{"ssh_pwauth": false, "disable_root": true, "ssh_deletekeys": true, "ssh_keys": map[string]any{"ed25519_private": string(private), "ed25519_public": strings.TrimSpace(string(host))}, "users": []any{map[string]any{"name": "runner", "shell": "/bin/bash", "lock_passwd": true, "sudo": "ALL=(ALL) NOPASSWD:ALL", "groups": groups, "ssh_authorized_keys": []string{strings.TrimSpace(string(client))}}}, "write_files": files, "runcmd": run, "growpart": map[string]any{"mode": "auto", "devices": []string{"/"}}, "resize_rootfs": true}
	userData, err := yaml.Marshal(user)
	if err != nil {
		return err
	}
	prefix, _ := netip.ParsePrefix(c.Config.Subnet)
	mac := strings.ToLower(r.MAC)
	parts := []string{}
	for i := 0; i < 12; i += 2 {
		parts = append(parts, mac[i:i+2])
	}
	network := map[string]any{"version": 2, "ethernets": map[string]any{"runner": map[string]any{"match": map[string]any{"macaddress": strings.Join(parts, ":")}, "set-name": "eth0", "dhcp4": false, "dhcp6": false, "addresses": []string{fmt.Sprintf("%s/%d", r.IP, prefix.Bits())}, "routes": []any{map[string]any{"to": "default", "via": c.Config.Gateway}}, "nameservers": map[string]any{"addresses": c.Config.DNS}}}}
	netData, err := yaml.Marshal(network)
	if err != nil {
		return err
	}
	meta, err := yaml.Marshal(map[string]string{"instance-id": r.Nonce, "local-hostname": r.Name})
	if err != nil {
		return err
	}
	err = seed.Write(filepath.Join(r.Directory, "seed.iso"), map[string][]byte{"user-data": append([]byte("#cloud-config\n"), userData...), "meta-data": meta, "network-config": netData})
	clear(private)
	if err != nil {
		return err
	}
	for _, file := range []string{"host", "host.pub", "client.pub"} {
		if err = os.Remove(filepath.Join(r.Directory, file)); err != nil {
			return err
		}
	}
	return nil
}
func (c *Client) one(ctx context.Context, name, op string) error {
	r, err := c.read(name)
	if err != nil {
		return err
	}
	_, err = c.operation(ctx, op, []Record{r})
	return err
}
func (c *Client) Start(ctx context.Context, name string) error { return c.one(ctx, name, "Start") }
func (c *Client) Stop(ctx context.Context, name string) error  { return c.one(ctx, name, "Stop") }
func (c *Client) ssh(ctx context.Context, r Record, input []byte, command string) ([]byte, error) {
	knownHosts := strings.ReplaceAll(filepath.ToSlash(filepath.Join(r.Directory, "known_hosts")), "%", "%%")
	return c.command(ctx, input, c.Config.SSH, "-F", "NUL", "-i", filepath.Join(r.Directory, "client"), "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no", "-o", "StrictHostKeyChecking=yes", "-o", "GlobalKnownHostsFile=NUL", "-o", "UserKnownHostsFile=\""+knownHosts+"\"", "-o", "HostKeyAlias="+r.Name, "-o", "ConnectTimeout=3", "-o", "ConnectionAttempts=1", "-o", "LogLevel=ERROR", "runner@"+r.IP, command)
}

// JIT goes only through encrypted, pinned-host-key SSH stdin. The guest file is
// private, atomically published and unlinked before starting Runner.Listener.
func (c *Client) Deliver(ctx context.Context, name, jit string) error {
	r, err := c.read(name)
	if err != nil {
		return err
	}
	_, err = c.ssh(ctx, r, []byte(jit), "umask 077; test -f /home/runner/.clc/guest-ready && test ! -e /home/runner/.clc/jit-consumed && cat > /home/runner/.clc/jit.pending && mv /home/runner/.clc/jit.pending /home/runner/.clc/jit")
	return err
}
func (c *Client) Lifecycle(ctx context.Context, name string) (Lifecycle, error) {
	r, err := c.read(name)
	if err != nil {
		return Lifecycle{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	b, err := c.ssh(probeCtx, r, nil, "for f in guest-ready jit-consumed listening job-started job-completed failed listener-exited; do if test -f /home/runner/.clc/$f; then echo $f; fi; done; if test -f /home/runner/.clc/job-started; then printf 'job-start-time='; stat -c %Y /home/runner/.clc/job-started 2>/dev/null || true; fi; if test -f /var/lib/cloud/instance/boot-finished; then echo cloud-finished; fi; if sudo cloud-init status --format json 2>/dev/null | jq -e '.status == \"done\"' >/dev/null; then echo cloud-ok; fi")
	if err != nil {
		if !r.SSHReady {
			return Lifecycle{Problem: "guest SSH not ready; inspect boot, static networking and cloud-init"}, nil
		}
		return Lifecycle{}, fmt.Errorf("guest lifecycle unknown; preserving VM: %w", err)
	}
	s := string(b)
	life := Lifecycle{Ready: strings.Contains(s, "guest-ready"), Configured: strings.Contains(s, "jit-consumed"), Started: strings.Contains(s, "job-started"), Completed: strings.Contains(s, "job-completed"), Failed: strings.Contains(s, "failed"), Exited: strings.Contains(s, "listener-exited")}
	for line := range strings.SplitSeq(s, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "job-start-time="); ok {
			if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= time.Now().Unix() {
				life.StartedAt = time.Unix(seconds, 0).UTC()
			}
		}
	}
	life.Listening = strings.Contains(s, "listening")
	if strings.Contains(s, "cloud-finished") && !strings.Contains(s, "cloud-ok") {
		life.Failed = true
		life.Problem = "cloud-init reported errors; inspect guest cloud-init status/logs"
	}
	if life.Ready && !r.SeedRemoved {
		if _, err = c.operation(ctx, "DetachSeed", []Record{r}); err != nil {
			return life, err
		}
		if err = os.Remove(filepath.Join(r.Directory, "seed.iso")); err != nil && !os.IsNotExist(err) {
			return life, err
		}
		r.SeedRemoved = true
	}
	if !r.SSHReady || life.Ready {
		r.SSHReady = true
		if err = c.save(r); err != nil {
			return life, err
		}
	}
	return life, nil
}
func (c *Client) Remove(ctx context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.read(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = c.operation(ctx, "Remove", []Record{r}); err != nil {
		return err
	}
	r.Removed = true
	return c.save(r)
}
func (c *Client) ForgetDiagnostics(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.read(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !r.Removed {
		return errors.New("VM removal must complete before deleting owned files")
	}
	// read validates the exact name, owner and absolute directory. Never follow a
	// junction planted underneath it (Windows helper checks all reparse points).
	if _, err = c.operation(context.Background(), "DeleteFiles", []Record{r}); err != nil {
		return err
	}
	return nil
}
func (c *Client) Check(ctx context.Context) error {
	_, err := c.operation(ctx, "Check", nil)
	return err
}

// The caller holds the controller data lock, so these non-job VMs can only be
// leftovers from an interrupted builder or explicitly invoked validation.
func (c *Client) RecoverAuxiliary(ctx context.Context) error {
	records, err := c.records()
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Purpose != "runner" {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = c.recoverAuxiliaryVM(r); err != nil {
				return err
			}
		}
	}
	return nil
}

var _ io.Writer = (*buffer)(nil)
