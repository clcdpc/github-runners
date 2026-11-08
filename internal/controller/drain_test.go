package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/clcdpc/github-runners/internal/hyperv"
)

func TestDrainDeclinesOffersButAcknowledgesAndReportsZeroCapacity(t *testing.T) {
	f := newFixture(t)
	f.api.assigned = 0
	f.c.BeginDrain()
	f.session.available = []*scaleset.RunnerScaleSetMessage{{MessageID: 31, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 123}}}}}
	l, err := listener.New(f.c.ObserveSession(f.session), listener.Config{ScaleSetID: 42, MaxRunners: 3})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Run(context.Background(), f.c)
	if len(f.session.acquired) != 0 || f.api.jitCalls != 0 || len(f.session.ack) != 1 {
		t.Fatal("draining claimed new work or failed to ACK declined offers")
	}
	for _, n := range f.session.capacities {
		if n != 0 {
			t.Fatal("draining advertised capacity", n)
		}
	}
	if !f.c.CheckQuiescence(context.Background()).Ready {
		t.Fatal("empty drain did not become ready")
	}
}

func TestDrainPreservesThreeJobsUntilDifferentCompletionOrdersAndCleanup(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}, {1, 2, 0}} {
		f := newFixture(t)
		f.c.Config.Runners.Maximum = 3
		f.api.assigned = 3
		f.scale(t, 3)
		names := []string{}
		for name := range f.c.State.Runners {
			names = append(names, name)
			f.ws.life[name] = busyLifecycle()
		}
		if err := f.c.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.c.BeginDrain()
		for _, index := range order {
			if f.c.CheckQuiescence(context.Background()).Ready {
				t.Fatal("ready with owned jobs")
			}
			if err := f.c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.ws.stopped != 0 {
				t.Fatal("drain terminated active job")
			}
			f.exit(names[index])
			f.api.assigned--
			if err := f.c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if !f.c.CheckQuiescence(context.Background()).Ready || f.c.Count() != 0 || len(f.api.runners) != 0 {
			t.Fatal("final cleanup did not establish quiescence")
		}
	}
}

func TestDrainFinishesAcceptedWorkWithoutRunnerAndPreservesGuestBoot(t *testing.T) {
	f := newFixture(t)
	f.ws.shortage = true
	if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 14}}}, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	f.c.BeginDrain()
	f.ws.shortage = false
	f.now = f.now.Add(time.Minute)
	if err := f.c.Tick(context.Background()); err != nil || f.c.Count() != 1 {
		t.Fatal("accepted work lost its ability to provision", err)
	}
	name := f.name()
	f.ws.life[name] = hyperv.Lifecycle{Ready: true}
	if err := f.c.Tick(context.Background()); err != nil || f.ws.stopped != 0 || f.ws.removed != 0 {
		t.Fatal("drain retired an accepted job's booting VM", err)
	}
	f.ws.life[name] = hyperv.Lifecycle{Ready: true, Configured: true, Listening: true}
	if err := f.c.Tick(context.Background()); err != nil || f.ws.stopped != 0 {
		t.Fatal("assigned but not started runner retired", err)
	}
	if f.c.CheckQuiescence(context.Background()).Ready {
		t.Fatal("runner/assignment is not quiescent")
	}
	f.api.assigned = 0
	f.exit(name)
	if err := f.c.RefreshDrainDemand(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	if err := f.c.RefreshDrainDemand(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Tick(context.Background()); err != nil || !f.c.CheckQuiescence(context.Background()).Ready {
		t.Fatal("accepted work/cancellation did not clean up", err)
	}
}

func TestDrainRemoteAndCleanupFailuresFailClosed(t *testing.T) {
	for _, failure := range []string{"archive", "remove", "list", "github", "statistics"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			f.scale(t, 1)
			f.c.BeginDrain()
			f.api.assigned = 0
			f.exit(f.name())
			switch failure {
			case "github":
				f.api.findError = true
			case "statistics":
				f.api.statsError = true
			default:
				f.ws.failure = failure
			}
			_ = f.c.Tick(context.Background())
			if q := f.c.CheckQuiescence(context.Background()); q.Ready || q.Blocker == "" {
				t.Fatal("failure permitted restart", q)
			}
			f.ws.failure = ""
			f.api.findError = false
			f.api.statsError = false
			if err := f.c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !f.c.CheckQuiescence(context.Background()).Ready {
				t.Fatal("drain did not recover")
			}
		})
	}
}

type blockedAcquirer struct {
	entered, release chan struct{}
	acquired         []int64
}

func (a *blockedAcquirer) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	close(a.entered)
	<-a.release
	a.acquired = append(a.acquired, ids...)
	return ids, nil
}

func TestDrainSerializesWithAcquisitionAndProvisioning(t *testing.T) {
	f := newFixture(t)
	a := &blockedAcquirer{entered: make(chan struct{}), release: make(chan struct{})}
	f.c.Session(a)
	scaled := make(chan error, 1)
	go func() {
		scaled <- f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 77}}}, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}})
	}()
	<-a.entered
	drained := make(chan struct{})
	go func() { f.c.BeginDrain(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("drain bypassed an in-flight acquisition")
	case <-time.After(20 * time.Millisecond):
	}
	close(a.release)
	if err := <-scaled; err != nil {
		t.Fatal(err)
	}
	<-drained
	if f.c.Count() != 1 || !f.c.State.Acquisitions[77] {
		t.Fatal("in-flight accepted work was lost")
	}
	if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 78}}}}); err != nil {
		t.Fatal(err)
	}
	if len(a.acquired) != 1 {
		t.Fatal("offer acquired after drain fence")
	}
}

func TestLostAcquireReplyAndCrashRecoveryRemainOwnedUntilFreshRemoteZero(t *testing.T) {
	f := newFixture(t)
	f.session.acquireError = true
	err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 81}}}})
	if err == nil {
		t.Fatal("expected failed acquisition")
	}
	state, err := LoadState(f.path, f.c.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := state.Acquisitions[81]; !exists {
		t.Fatal("uncertain acquisition disappeared on crash")
	}
	f.c.State = state
	f.c.BeginDrain()
	f.api.statsError = true
	if f.c.CheckQuiescence(context.Background()).Ready {
		t.Fatal("unknown remote ownership treated as idle")
	}
	f.api.statsError = false
	f.api.assigned = 0
	if f.c.CheckQuiescence(context.Background()).Ready {
		t.Fatal("one lagging zero discarded an acquisition intent")
	}
	f.now = f.now.Add(time.Minute)
	if !f.c.CheckQuiescence(context.Background()).Ready {
		t.Fatal("fresh remote cancellation/zero did not release uncertain ownership")
	}
}

func TestDrainRegistrationFailureRetainsIntentAndRetriesOwnedDemand(t *testing.T) {
	f := newFixture(t)
	f.api.failJIT = true
	f.c.BeginDrain()
	f.c.State.Acquisitions = map[int64]bool{9: true}
	if err := f.c.RefreshDrainDemand(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Tick(context.Background()); err == nil {
		t.Fatal("expected registration failure")
	}
	if f.c.CheckQuiescence(context.Background()).Ready {
		t.Fatal("failed registration erased owned job")
	}
	f.api.failJIT = false
	f.now = f.now.Add(time.Minute)
	if err := f.c.Tick(context.Background()); err != nil || f.c.Count() != 1 {
		t.Fatal("owned assignment did not retry provisioning", err)
	}
}

func TestAcquisitionIntentSaveFailureHasNoRemoteEffects(t *testing.T) {
	f := newFixture(t)
	f.c.Save = func(*State) error { return errors.New("journal unavailable") }
	_ = f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 9}}}})
	if len(f.session.acquired) > 0 {
		t.Fatal("acquired before durable ownership intent")
	}
}
