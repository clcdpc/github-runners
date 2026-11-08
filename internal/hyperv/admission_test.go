package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clcdpc/github-runners/internal/config"
)

func TestAdmissionDefersBeforeAnyVMIntentOrFiles(t *testing.T) {
	for _, response := range []string{"deferred", "malformed", "wrong-allocation", "inconsistent"} {
		t.Run(response, func(t *testing.T) {
			c, f, image := fixture(t)
			base := c.Command
			c.Command = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if args[len(args)-1] == "Admission" {
					a := Admission{Decision: "Deferred", Reason: "Insufficient available memory", CheckedAt: time.Now(), RequestedCPUs: c.Config.CPUs, RequestedMemoryGiB: c.Config.MemoryGiB, ReserveMemoryGiB: c.Config.ReserveMemoryGiB}
					switch response {
					case "malformed":
						return []byte(`{"ready":true}`), nil
					case "wrong-allocation":
						a.RequestedMemoryGiB++
					case "inconsistent":
						a.Ready = true
					}
					return json.Marshal(a)
				}
				return base(ctx, input, program, args...)
			}
			name := c.newName()
			_, err := c.Create(context.Background(), name, image.ID, 42)
			if err == nil || len(f.ops) != 0 || len(f.vms) != 0 {
				t.Fatal("admission failure permitted VM side effects", err, f.ops)
			}
			var shortage *CapacityError
			if errors.As(err, &shortage) != (response == "deferred") {
				t.Fatal("resource deferral was confused with an invalid bridge response", err)
			}
			if _, err := os.Stat(c.path(name)); !os.IsNotExist(err) {
				t.Fatal("admission refusal left VM files", err)
			}
		})
	}
}

func TestHardwareChangeReusesImageAndPreservesExistingVM(t *testing.T) {
	c, f, image := fixture(t)
	inputs, parts, err := imageInputs(c.Config.ImageRecipe, c.Config.DiskGiB, guest)
	if err != nil {
		t.Fatal(err)
	}
	image.Inputs, image.InputHashes = inputs, parts
	manifest := filepath.Join(filepath.Dir(image.Path), "image.json")
	if err := atomicJSON(manifest, image); err != nil {
		t.Fatal(err)
	}
	c.Config.CPUs, c.Config.MemoryGiB = 4, 16
	old := c.newName()
	if _, err := c.Create(context.Background(), old, image.ID, 41); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	before := f.vms[old]
	c.Config.CPUs, c.Config.MemoryGiB = 2, 10
	f.ops = nil
	c.Download = func(context.Context, string, string) error {
		t.Fatal("hardware change downloaded a new image")
		return nil
	}
	if reused, err := c.PrepareImage(context.Background(), false); err != nil || reused.ID != image.ID || len(f.ops) != 0 {
		t.Fatal("hardware change rebuilt or modified the base image", reused, err, f.ops)
	}
	base := c.Command
	creates := 0
	c.Command = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if args[len(args)-1] == "Create" {
			var request struct {
				Settings config.HyperV `json:"settings"`
			}
			if err := json.Unmarshal(input, &request); err != nil || request.Settings.CPUs != 2 || request.Settings.MemoryGiB != 10 || request.Settings.DiskGiB != 20 {
				t.Fatal("new VM request lost the effective hardware settings", err)
			}
			creates++
		}
		return base(ctx, input, program, args...)
	}
	for range 2 {
		name := c.newName()
		if _, err := c.Create(context.Background(), name, image.ID, 42); err != nil {
			t.Fatal(err)
		}
		if err := c.Start(context.Background(), name); err != nil {
			t.Fatal(err)
		}
		if _, err := c.VerifyHardware(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	if creates != 2 || f.vms[old] != before {
		t.Fatal("existing VM changed or new clones were not provisioned")
	}
}

func TestIntegrationRejectsWrongNativeHardwareAndCleansUp(t *testing.T) {
	c, f, _ := fixture(t)
	base := c.Command
	c.Command = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		b, err := base(ctx, input, program, args...)
		if err == nil && args[len(args)-1] == "Inventory" {
			var items []VM
			if json.Unmarshal(b, &items) == nil {
				for i := range items {
					items[i].CPUs++
				}
				return json.Marshal(items)
			}
		}
		return b, err
	}
	err := c.Integration(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hardware verification failed") || len(f.vms) != 0 {
		t.Fatal("wrong native VM allocation was accepted or leaked", err)
	}
	b, err := os.ReadFile(filepath.Join(c.DataDir, "hyperv-validation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct{ Passed, HardwareVerified, CleanupPassed bool }
	if json.Unmarshal(b, &report) != nil || report.Passed || report.HardwareVerified || !report.CleanupPassed {
		t.Fatal("native hardware failure was absent from integration evidence", string(b))
	}
}

func TestNativeHardwareVerificationRejectsEachMismatch(t *testing.T) {
	for _, mismatch := range []string{"cpus", "startup-memory", "assigned-memory", "dynamic-memory", "missing-memory"} {
		t.Run(mismatch, func(t *testing.T) {
			c, f, image := fixture(t)
			name := c.newName()
			if _, err := c.Create(context.Background(), name, image.ID, 42); err != nil {
				t.Fatal(err)
			}
			if err := c.Start(context.Background(), name); err != nil {
				t.Fatal(err)
			}
			v := f.vms[name]
			switch mismatch {
			case "cpus":
				v.CPUs++
			case "startup-memory":
				v.MemoryStartupBytes++
			case "assigned-memory":
				v.MemoryAssignedBytes++
			case "dynamic-memory":
				enabled := true
				v.DynamicMemoryEnabled = &enabled
			case "missing-memory":
				v.DynamicMemoryEnabled = nil
			}
			f.vms[name] = v
			if _, err := c.VerifyHardware(context.Background(), name); err == nil {
				t.Fatal("incorrect native allocation was accepted", mismatch)
			}
			if f.vms[name] != v {
				t.Fatal("read-only hardware verification modified a VM")
			}
		})
	}
}
