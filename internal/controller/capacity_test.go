package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/clcdpc/github-runners/internal/hyperv"
)

func busyLifecycle() hyperv.Lifecycle {
	return hyperv.Lifecycle{Ready: true, Configured: true, Listening: true, Started: true}
}

func TestThreeSlotsProvisionWhileExistingRunnersAreBusyAndRecoverAfterCompletion(t *testing.T) {
	f := newFixture(t)
	f.c.Config.Runners.Maximum = 3
	for active := 0; active <= 3; active++ {
		f.api.assigned = active
		f.scale(t, active)
		for name := range f.c.State.Runners {
			f.ws.life[name] = busyLifecycle()
		}
		if err := f.c.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		d := f.c.Capacity()
		if d.Maximum != 3 || d.Minimum != 0 || d.CurrentRunners != active || d.BusyRunners != active || d.AvailableSlots != 3-active || d.DesiredRunners != active {
			t.Fatalf("active=%d: %+v", active, d)
		}
	}
	f.api.assigned = 4
	f.scale(t, 4)
	if f.c.Count() != 3 || f.api.jitCalls != 3 || f.ws.stopped != 0 {
		t.Fatal("fourth demand exceeded three slots or disturbed active VMs")
	}
	name := f.name()
	f.exit(name)
	f.api.assigned = 3 // Two running jobs and the next job waiting for a runner.
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 3 || f.api.jitCalls != 4 || f.ws.removed != 1 || f.c.State.Runners[name] != nil || f.api.runners[name] != nil {
		t.Fatal("completed runner was not retired and replaced in the same session")
	}
	f.api.assigned = 0
	for name := range f.ws.VMs {
		f.exit(name)
	}
	if err := f.c.Tick(context.Background()); err != nil || f.c.Count() != 0 || len(f.api.runners) != 0 {
		t.Fatal("zero-idle cleanup failed", err)
	}
}

func TestResourceDeferralPreservesListenerAndAvoidsRegistrationChurn(t *testing.T) {
	f := newFixture(t)
	f.c.Config.Runners.Maximum = 3
	f.ws.shortage = true
	f.api.assigned = 3
	var log bytes.Buffer
	f.c.Logger = slog.New(slog.NewJSONHandler(&log, nil))
	f.scale(t, 3)
	if f.c.Count() != 0 || f.api.jitCalls != 0 || f.ws.admissionCalls != 1 {
		t.Fatal("resource deferral allocated or retried before its deadline")
	}
	d := f.c.Capacity()
	if !d.ResourceBlocked || !d.ListenerConnected || d.LastAdmission.Ready || d.LastProvisioningIssue.Decision != "Deferred" || !d.NextProvisionAttempt.Equal(f.now.Add(30*time.Second)) {
		t.Fatal("deferral is not inspectable", d)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.Split(log.Bytes(), []byte("\n"))[1], &entry); err != nil {
		t.Fatal(err)
	}
	if entry["decision"] != "Deferred" || entry["runnerLimit"] != float64(3) || entry["requestedMemoryGiB"] != float64(10) || entry["reserveMemoryGiB"] != float64(8) {
		t.Fatal("admission log omitted the decision or measurements", log.String())
	}
	f.now = f.now.Add(30 * time.Second)
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ws.admissionCalls != 2 || !f.c.Capacity().NextProvisionAttempt.Equal(f.now.Add(time.Minute)) {
		t.Fatal("repeated shortage did not back off")
	}
	f.now = f.now.Add(time.Minute)
	f.ws.shortage = false
	if err := f.c.Tick(context.Background()); err != nil || f.c.Count() != 3 || f.api.jitCalls != 3 || f.c.Capacity().ResourceBlocked {
		t.Fatal("resources did not recover without a listener restart", err)
	}
	if f.c.Snapshot().LastProvisioningIssue == nil {
		t.Fatal("successful recovery erased the most recent issue")
	}
}

func TestFreedRunnerRechecksAdmissionWithoutWaitingForBackoff(t *testing.T) {
	f := newFixture(t)
	f.c.Config.Runners.Maximum = 3
	f.scale(t, 2)
	for name := range f.c.State.Runners {
		f.ws.life[name] = busyLifecycle()
	}
	f.ws.shortage = true
	f.api.assigned = 3
	f.scale(t, 3)
	f.exit(f.name())
	f.ws.shortage = false
	if err := f.c.Tick(context.Background()); err != nil || f.c.Count() != 3 {
		t.Fatal("freed resources remained blocked by the old backoff", err)
	}
}

func TestOfficialListenerReportsTotalMaximumThreeAtEveryOccupancy(t *testing.T) {
	for active := 0; active <= 3; active++ {
		f := newFixture(t)
		f.c.Config.Runners.Maximum = 3
		f.api.assigned = active
		f.scale(t, active)
		f.session.initial = active
		f.session.available = []*scaleset.RunnerScaleSetMessage{{MessageID: 20, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: active}}}
		l, err := listener.New(f.c.ObserveSession(f.session), listener.Config{ScaleSetID: 42, MaxRunners: 3})
		if err != nil {
			t.Fatal(err)
		}
		_ = l.Run(context.Background(), f.c)
		for _, capacity := range f.session.capacities {
			if capacity != 3 {
				t.Fatalf("active=%d advertised %d instead of total maximum 3", active, capacity)
			}
		}
		d := f.c.Capacity()
		if !d.Advertising || d.AdvertisedMaximum != 3 || d.AvailableSlots != 3-active {
			t.Fatal("capacity reporting diagnostics incorrect", d)
		}
		f.c.Session(nil)
		if f.c.Capacity().Advertising {
			t.Fatal("disconnected listener claimed live advertising")
		}
	}
}

func TestListenerAcknowledgesDeferredDemandAndTimerRetriesIt(t *testing.T) {
	f := newFixture(t)
	f.ws.shortage = true
	f.session.available = []*scaleset.RunnerScaleSetMessage{{MessageID: 17, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}}
	l, err := listener.New(f.session, listener.Config{ScaleSetID: 42, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Run(context.Background(), f.c)
	if len(f.session.ack) != 1 || f.session.ack[0] != 17 || f.api.jitCalls != 0 {
		t.Fatal("resource deferral discarded the message or registered a runner")
	}
	f.ws.shortage = false
	f.now = f.now.Add(30 * time.Second)
	if err := f.c.Tick(context.Background()); err != nil || f.c.Count() != 1 {
		t.Fatal("acknowledged deferred demand was lost", err)
	}
}

func TestAdmissionRaceCleansRegistrationAndKeepsRetryDeadline(t *testing.T) {
	f := newFixture(t)
	f.ws.createShortage = true
	f.scale(t, 1)
	if f.c.Count() != 0 || len(f.api.runners) != 0 || f.api.jitCalls != 1 || f.api.removed != 1 {
		t.Fatal("race after JIT leaked its registration or retried immediately")
	}
	if f.c.Capacity().NextProvisionAttempt.IsZero() {
		t.Fatal("cleanup of failed create erased the backoff")
	}
	f.ws.createShortage = false
	f.now = f.now.Add(30 * time.Second)
	if err := f.c.Tick(context.Background()); err != nil || f.c.Count() != 1 {
		t.Fatal("post-JIT admission race did not recover", err)
	}
}

func TestAdmissionMeasurementFailureIsVisibleAndHasNoJITSideEffects(t *testing.T) {
	f := newFixture(t)
	f.ws.failure = "admission"
	if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err == nil {
		t.Fatal("host measurement failure was silently accepted")
	}
	if err := f.c.Tick(context.Background()); err != nil || f.api.jitCalls != 0 || f.ws.admissionCalls != 1 || f.c.Capacity().LastProvisioningIssue.Decision != "Failed" {
		t.Fatal("measurement failure did not preserve diagnostics/backoff", err)
	}
}

func TestRestartWithNewHardwarePreservesBusyRunnerAndOwnership(t *testing.T) {
	f := newFixture(t)
	f.c.Config.HyperV.CPUs, f.c.Config.HyperV.MemoryGiB = 4, 16
	f.scale(t, 1)
	name := f.name()
	f.ws.life[name] = busyLifecycle()
	owner := f.c.State.Owner
	f.c.Config.HyperV.CPUs, f.c.Config.HyperV.MemoryGiB, f.c.Config.Runners.Maximum = 2, 10, 3
	restored, err := LoadState(f.path, f.c.Config)
	if err != nil || restored.Owner != owner {
		t.Fatal("hardware change invalidated deployment identity", err)
	}
	f.c.State, f.c.pendingJIT = restored, nil
	f.c.Session(nil)
	if err := f.c.Tick(context.Background()); err != nil || f.ws.removed != 0 || f.ws.stopped != 0 || f.api.removed != 0 {
		t.Fatal("restart modified an existing busy VM/registration", err)
	}
	f.c.Session(f.session)
	f.api.assigned = 3
	f.scale(t, 3)
	if f.c.Count() != 3 || f.c.State.Runners[name].Phase != "busy" {
		t.Fatal("new controller could not fill the two remaining slots")
	}
}
