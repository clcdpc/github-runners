package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Admission describes one additional VM, using current host measurements. Running
// guest memory is already reflected in Windows' available memory; do not charge
// it a second time. Hyper-V's own available memory already includes its reserve.
type Admission struct {
	Ready                      bool      `json:"ready"`
	Decision                   string    `json:"decision"`
	Reason                     string    `json:"reason,omitempty"`
	CheckedAt                  time.Time `json:"checkedAt"`
	RequestedCPUs              int       `json:"requestedCPUs"`
	AssignedCPUs               int       `json:"assignedCPUs"`
	LogicalProcessors          int       `json:"logicalProcessors"`
	RequestedMemoryGiB         int       `json:"requestedMemoryGiB"`
	ReserveMemoryGiB           int       `json:"reserveMemoryGiB"`
	AvailableMemoryBytes       int64     `json:"availableMemoryBytes"`
	HyperVAvailableMemoryBytes *int64    `json:"hypervAvailableMemoryBytes,omitempty"`
	HyperVMemoryError          string    `json:"hypervMemoryError,omitempty"`
	AvailableDiskBytes         int64     `json:"availableDiskBytes"`
	RequiredDiskBytes          int64     `json:"requiredDiskBytes"`
}

type CapacityError struct{ Admission Admission }

func (e *CapacityError) Error() string { return "VM admission deferred: " + e.Admission.Reason }

// Capacity is read-only and can be called before registering a runner with GitHub.
func (c *Client) Capacity(ctx context.Context) (Admission, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	records, err := c.records()
	if err != nil {
		return Admission{}, err
	}
	return c.admission(ctx, records)
}

func (c *Client) admission(ctx context.Context, records []Record) (Admission, error) {
	b, err := c.operation(ctx, "Admission", records)
	if err != nil {
		return Admission{}, err
	}
	var a Admission
	if json.Unmarshal(b, &a) != nil || a.CheckedAt.IsZero() ||
		(a.Decision != "Accepted" && a.Decision != "Deferred") ||
		a.Ready != (a.Decision == "Accepted") || (!a.Ready && a.Reason == "") ||
		a.RequestedCPUs != c.Config.CPUs || a.RequestedMemoryGiB != c.Config.MemoryGiB || a.ReserveMemoryGiB != c.Config.ReserveMemoryGiB {
		return Admission{}, errors.New("invalid Hyper-V admission response; no VM allocated")
	}
	return a, nil
}

// VerifyHardware consumes native Get-VMProcessor/Get-VMMemory inventory. It never
// adjusts a VM: older runners retain the allocation with which they were created.
func (c *Client) VerifyHardware(ctx context.Context, name string) (VM, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	records, err := c.records()
	if err != nil {
		return VM{}, err
	}
	items, err := c.inventory(ctx, records)
	if err != nil {
		return VM{}, err
	}
	for _, v := range items {
		if v.Name != name {
			continue
		}
		memory := int64(c.Config.MemoryGiB) << 30
		if v.CPUs != c.Config.CPUs || v.MemoryStartupBytes != memory || v.DynamicMemoryEnabled == nil || *v.DynamicMemoryEnabled || (v.Running && v.MemoryAssignedBytes != memory) {
			dynamic := "unknown"
			if v.DynamicMemoryEnabled != nil {
				dynamic = fmt.Sprint(*v.DynamicMemoryEnabled)
			}
			return v, fmt.Errorf("VM hardware verification failed for %s: CPUs=%d startupBytes=%d assignedBytes=%d dynamicMemory=%s", name, v.CPUs, v.MemoryStartupBytes, v.MemoryAssignedBytes, dynamic)
		}
		return v, nil
	}
	return VM{}, errors.New("VM missing during hardware verification")
}
