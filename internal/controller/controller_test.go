package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/clcdpc/github-runners/internal/config"
	"github.com/clcdpc/github-runners/internal/hyperv"
	"github.com/clcdpc/github-runners/internal/secure"
	"github.com/google/uuid"
)

type fakeAPI struct {
	runners                            map[string]*scaleset.RunnerReference
	jitCalls, removed                  int
	failJIT, lostJIT, busy, statsError bool
	findError                          bool
	assigned                           int
}

func (f *fakeAPI) JIT(_ context.Context, name string, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.jitCalls++
	if f.failJIT {
		return nil, errors.New("JIT temporarily unavailable")
	}
	r := &scaleset.RunnerReference{ID: 100 + f.jitCalls, Name: name, RunnerScaleSetID: id}
	f.runners[name] = r
	if f.lostJIT {
		return nil, errors.New("JIT response lost")
	}
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: r, EncodedJITConfig: "synthetic-jit-" + strings.Repeat("x", 120)}, nil
}
func (f *fakeAPI) Find(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	if f.findError {
		return nil, errors.New("runner lookup unavailable")
	}
	return f.runners[name], nil
}
func (f *fakeAPI) Remove(_ context.Context, id int) error {
	if f.busy {
		return scaleset.JobStillRunningError
	}
	for name, r := range f.runners {
		if r.ID == id {
			delete(f.runners, name)
			f.removed++
		}
	}
	return nil
}
func (f *fakeAPI) Statistics(context.Context, int) (*scaleset.RunnerScaleSetStatistic, error) {
	if f.statsError {
		return nil, errors.New("statistics unavailable")
	}
	return &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: f.assigned}, nil
}

type fakeHyperV struct {
	VMs                        map[string]hyperv.VM
	life                       map[string]hyperv.Lifecycle
	failure                    string
	lostCreate                 bool
	archived, removed, stopped int
	admissionCalls             int
	shortage                   bool
	createShortage             bool
}

func (f *fakeHyperV) Capacity(context.Context) (hyperv.Admission, error) {
	f.admissionCalls++
	if f.failure == "admission" {
		return hyperv.Admission{}, errors.New("host measurement unavailable")
	}
	a := hyperv.Admission{Ready: !f.shortage, Decision: "Accepted", CheckedAt: time.Now(), RequestedCPUs: 2, RequestedMemoryGiB: 10, ReserveMemoryGiB: 8, AvailableMemoryBytes: 18 << 30}
	if f.shortage {
		a.Decision, a.Reason = "Deferred", "Insufficient available memory"
	}
	return a, nil
}

func (f *fakeHyperV) List(context.Context) ([]hyperv.VM, error) {
	if f.failure == "list" {
		return nil, errors.New("HyperV unavailable")
	}
	items := []hyperv.VM{}
	for _, v := range f.VMs {
		items = append(items, v)
	}
	return items, nil
}
func (f *fakeHyperV) Create(_ context.Context, name, _ string, id int) (string, error) {
	if f.createShortage {
		return "", &hyperv.CapacityError{Admission: hyperv.Admission{Decision: "Deferred", Reason: "Host memory changed after admission", CheckedAt: time.Now()}}
	}
	if f.failure == "create" && !f.lostCreate {
		return "", errors.New("create failed")
	}
	v := hyperv.VM{ID: "VM-" + name, Name: name, Created: time.Now()}
	v.RunnerID = id
	v.Status = "created"
	f.VMs[name] = v
	if f.lostCreate {
		return "", errors.New("create response lost")
	}
	return v.ID, nil
}

func (f *fakeHyperV) Start(_ context.Context, name string) error {
	if f.failure == "start" {
		return errors.New("start failed")
	}
	v := f.VMs[name]
	v.Running = true
	v.Status = "running"
	f.VMs[name] = v
	f.life[name] = hyperv.Lifecycle{Ready: true}
	return nil
}
func (f *fakeHyperV) Stop(_ context.Context, name string) error {
	f.stopped++
	v := f.VMs[name]
	v.Running = false
	v.Status = "exited"
	f.VMs[name] = v
	return nil
}
func (f *fakeHyperV) Deliver(_ context.Context, name, _ string) error {
	if f.failure == "deliver" {
		return errors.New("delivery failed")
	}
	f.life[name] = hyperv.Lifecycle{Ready: true, Configured: true, Listening: true}
	return nil
}
func (f *fakeHyperV) Lifecycle(_ context.Context, name string) (hyperv.Lifecycle, error) {
	if f.failure == "probe" {
		return hyperv.Lifecycle{}, errors.New("probe failed")
	}
	return f.life[name], nil
}
func (f *fakeHyperV) Archive(context.Context, string, bool) error {
	if f.failure == "archive" {
		return errors.New("disk full")
	}
	f.archived++
	return nil
}
func (f *fakeHyperV) Remove(_ context.Context, name string) error {
	if f.failure == "remove" {
		return errors.New("remove failed")
	}
	f.removed++
	delete(f.VMs, name)
	return nil
}
func (f *fakeHyperV) ForgetDiagnostics(string) error { return nil }

type fakeSession struct {
	available              []*scaleset.RunnerScaleSetMessage
	ack                    []int
	acquired               []int64
	capacities, lastIDs    []int
	acquireError, ackError bool
	initial                int
}

func (f *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	if f.acquireError {
		return nil, errors.New("acquire failed")
	}
	f.acquired = append(f.acquired, ids...)
	return ids, nil
}
func (f *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: f.initial}}
}
func (f *fakeSession) GetMessage(_ context.Context, last, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	f.capacities = append(f.capacities, capacity)
	f.lastIDs = append(f.lastIDs, last)
	if len(f.available) == 0 {
		return nil, context.Canceled
	}
	m := f.available[0]
	f.available = f.available[1:]
	return m, nil
}
func (f *fakeSession) DeleteMessage(_ context.Context, id int) error {
	if f.ackError {
		return errors.New("ack failed")
	}
	f.ack = append(f.ack, id)
	return nil
}

type fixture struct {
	c       *Controller
	api     *fakeAPI
	ws      *fakeHyperV
	session *fakeSession
	now     time.Time
	path    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	conf, err := config.Load("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	conf.Runners.IdleGraceSeconds = 30
	conf.Runners.CompletionGraceSeconds = 30
	conf.Runners.Maximum = 1 // Existing single-slot lifecycle scenarios; three-slot cases override this.
	f := &fixture{now: time.Now(), path: filepath.Join(t.TempDir(), "state.json"), api: &fakeAPI{runners: map[string]*scaleset.RunnerReference{}, assigned: 1}, ws: &fakeHyperV{VMs: map[string]hyperv.VM{}, life: map[string]hyperv.Lifecycle{}}, session: &fakeSession{}}
	s := &State{Version: 2, Owner: uuid.NewString(), Scope: Scope(conf), ScaleSetID: 42, Runners: map[string]*Runner{}}
	f.c = &Controller{Config: conf, State: s, API: f.api, HyperV: f.ws, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Redactor: &secure.Redactor{}, Save: func(s *State) error { return SaveState(f.path, s) }, Now: func() time.Time { return f.now }}
	f.c.Session(f.session)
	return f
}
func (f *fixture) scale(t *testing.T, n int) {
	t.Helper()
	if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: n}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.c.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
func (f *fixture) name() string {
	for name := range f.c.State.Runners {
		return name
	}
	return ""
}
func (f *fixture) exit(name string) {
	v := f.ws.VMs[name]
	v.Running = false
	v.Status = "exited"
	f.ws.VMs[name] = v
}

func TestDesiredCountUsesStatistics(t *testing.T) {
	for _, test := range []struct{ min, max, assigned, want int }{{0, 1, 0, 0}, {0, 1, 50, 1}, {2, 5, 2, 4}, {2, 5, -10, 2}, {0, 1, int(^uint(0) >> 1), 1}} {
		if got := Desired(test.min, test.max, &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: test.assigned}); got != test.want {
			t.Fatalf("desired=%d want=%d", got, test.want)
		}
	}
	f := newFixture(t)
	msg := &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0}, JobAssignedMessages: make([]*scaleset.JobAssigned, 50)}
	if err := f.c.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 0 {
		t.Fatal("counted job events instead of statistics")
	}
}
func TestProvisionAndMaximumConcurrency(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 20)
	f.scale(t, 20)
	if f.c.Count() != 1 || len(f.ws.VMs) != 1 || f.api.jitCalls != 1 {
		t.Fatal("maximum not enforced")
	}
	b, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "synthetic-jit") {
		t.Fatal("JIT persisted in journal")
	}
}
func TestProvisioningFailuresAndRetries(t *testing.T) {
	for _, failure := range []string{"jit", "lost-jit", "create", "lost-create", "start", "deliver"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			switch failure {
			case "jit":
				f.api.failJIT = true
			case "lost-jit":
				f.api.lostJIT = true
			case "lost-create":
				f.ws.lostCreate = true
			default:
				f.ws.failure = failure
			}
			err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}})
			if failure == "deliver" && err == nil {
				err = f.c.Tick(context.Background())
			}
			if err == nil {
				t.Fatal("expected provisioning failure")
			}
			f.api.failJIT = false
			f.api.lostJIT = false
			f.ws.failure = ""
			f.ws.lostCreate = false
			f.now = f.now.Add(31 * time.Second)
			if err = f.c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.c.Count() != 1 || len(f.ws.VMs) != 1 || len(f.api.runners) != 1 {
				t.Fatal("retry leaked or duplicated resources")
			}
		})
	}
}
func TestCompletionAndCancellationCleanup(t *testing.T) {
	for _, result := range []string{"success", "failure", "canceled"} {
		t.Run(result, func(t *testing.T) {
			f := newFixture(t)
			f.scale(t, 1)
			name := f.name()
			msg := &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{}, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: name, Result: result}}}
			if err := f.c.Scale(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
			if f.ws.removed != 0 {
				t.Fatal("completion notification killed a runner before final logs")
			}
			f.exit(name)
			f.api.assigned = 0
			if err := f.c.Scale(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
			if f.c.Count() != 0 || f.ws.archived != 1 || len(f.api.runners) != 0 {
				t.Fatal("completed runner leaked")
			}
			if err := f.c.Scale(context.Background(), msg); err != nil {
				t.Fatal("duplicate completion must be harmless", err)
			}
			if err := f.c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.ws.removed != 1 {
				t.Fatal("cleanup not idempotent")
			}
		})
	}
}
func TestQueuedCancellationRetiresIdleRunner(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	f.scale(t, 0)
	f.now = f.now.Add(31 * time.Second)
	f.api.assigned = 0
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 0 {
		t.Fatal("canceled queued job left idle runner")
	}
}
func TestBusyRunnerProtectedAndServerFencesRace(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	f.ws.life[name] = hyperv.Lifecycle{Ready: true, Started: true}
	f.scale(t, 0)
	f.now = f.now.Add(time.Hour)
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ws.removed != 0 || f.api.removed != 0 {
		t.Fatal("busy runner was scaled down")
	}
	f.c.Drain()
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 1 {
		t.Fatal("shutdown killed job")
	}
	// A job may start after the idle probe but before deletion. The API fences it.
	g := newFixture(t)
	g.scale(t, 1)
	g.scale(t, 0)
	g.now = g.now.Add(31 * time.Second)
	g.api.busy = true
	if err := g.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.ws.removed != 0 || g.ws.stopped != 0 {
		t.Fatal("server busy fence was ignored")
	}
}
func TestUnexpectedTerminationAndRestart(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	f.ws.life[name] = hyperv.Lifecycle{Ready: true, Started: true}
	s, err := LoadState(f.path, f.c.Config)
	if err != nil {
		t.Fatal(err)
	}
	f.c.State = s
	f.c.Session(nil)
	if err = f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.State.Runners[name].Phase != "busy" {
		t.Fatal("restart did not adopt running job")
	}
	f.exit(name)
	f.c.Session(f.session)
	if err = f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 1 || f.name() == name || len(f.ws.VMs) != 1 {
		t.Fatal("unexpected termination was not replaced with fresh runner")
	}
}
func TestRecoverUnjournaledOwnedVM(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	delete(f.c.State.Runners, name)
	f.c.Session(nil)
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.State.Runners[name] == nil || f.api.jitCalls != 1 {
		t.Fatal("orphan was not adopted idempotently")
	}
	f.c.Drain()
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 0 {
		t.Fatal("orphan cleanup failed")
	}
}
func TestCleanupFailuresDoNotLoseJournal(t *testing.T) {
	for _, failure := range []string{"list", "archive", "remove"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			f.scale(t, 1)
			f.exit(f.name())
			f.ws.failure = failure
			f.api.assigned = 0
			if err := f.c.Tick(context.Background()); err == nil {
				t.Fatal("expected cleanup failure")
			}
			if f.c.Count() != 1 {
				t.Fatal("journal discarded before cleanup")
			}
			f.ws.failure = ""
			if err := f.c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.c.Count() != 0 {
				t.Fatal("cleanup retry failed")
			}
		})
	}
}
func TestUnknownProbeAndIdentityFailClosed(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	f.ws.failure = "probe"
	f.c.Drain()
	if err := f.c.Tick(context.Background()); err == nil {
		t.Fatal("probe failure ignored")
	}
	if f.ws.removed != 0 {
		t.Fatal("probe failure deleted a runner")
	}
	f.ws.failure = ""
	f.api.runners[f.name()].RunnerScaleSetID = 99
	f.exit(f.name())
	if err := f.c.Tick(context.Background()); err == nil {
		t.Fatal("foreign scale-set registration removed")
	}
}
func TestNoSideEffectsBeforeDurableIntent(t *testing.T) {
	f := newFixture(t)
	f.c.Save = func(*State) error { return errors.New("disk unavailable") }
	if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err == nil {
		t.Fatal("expected journal failure")
	}
	if f.api.jitCalls != 0 || len(f.ws.VMs) != 0 {
		t.Fatal("side effects without durable intent")
	}
}
func TestOfficialListenerAcknowledgementAndReplay(t *testing.T) {
	f := newFixture(t)
	message := &scaleset.RunnerScaleSetMessage{MessageID: 17, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 123}}}}
	f.session.available = []*scaleset.RunnerScaleSetMessage{message}
	f.session.ackError = true
	l, err := listener.New(f.session, listener.Config{ScaleSetID: 42, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Run(context.Background(), f.c); err == nil {
		t.Fatal("expected ack failure")
	}
	if f.api.jitCalls != 1 || len(f.session.acquired) != 1 {
		t.Fatal("job was not acquired/provisioned")
	}
	f.session.ackError = false
	f.session.initial = 1
	f.session.available = []*scaleset.RunnerScaleSetMessage{message}
	_ = l.Run(context.Background(), f.c)
	if f.api.jitCalls != 1 || len(f.session.ack) != 1 || f.session.ack[0] != 17 {
		t.Fatal("redelivery duplicated runner or was not acknowledged")
	}
	for _, cap := range f.session.capacities {
		if cap != 1 {
			t.Fatal("maximum capacity not advertised")
		}
	}
	if f.session.lastIDs[len(f.session.lastIDs)-1] != 17 {
		t.Fatal("message ID not tracked")
	}
}
func TestOfficialListenerDoesNotAckFailedProvisioning(t *testing.T) {
	f := newFixture(t)
	f.ws.failure = "create"
	f.session.available = []*scaleset.RunnerScaleSetMessage{{MessageID: 9, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}}
	l, err := listener.New(f.session, listener.Config{ScaleSetID: 42, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Run(context.Background(), f.c); err == nil || len(f.session.ack) != 0 {
		t.Fatal("failed provisioning was acknowledged")
	}
}

func TestStatisticsRefreshRetriesAndDisconnectedSessionsDoNotProvision(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	f.exit(f.name())
	f.api.statsError = true
	if err := f.c.Tick(context.Background()); err == nil {
		t.Fatal("statistics failure ignored")
	}
	if f.c.Count() != 0 {
		t.Fatal("terminated runner not cleaned up")
	}
	f.api.statsError = false
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 1 {
		t.Fatal("statistics refresh was not retried")
	}
	f.c.Session(nil)
	f.exit(f.name())
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 0 {
		t.Fatal("disconnected listener provisioned from stale statistics")
	}
}
func TestCompletedProcessGraceAndStaleStatistics(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	f.ws.life[name] = hyperv.Lifecycle{Ready: true, Started: true, Completed: true}
	f.api.assigned = 0
	f.scale(t, 0)
	f.now = f.now.Add(31 * time.Second)
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 0 || f.ws.stopped != 1 {
		t.Fatal("finished process did not retire after completion grace")
	}
	g := newFixture(t)
	g.scale(t, 0)
	g.api.assigned = 1
	g.now = g.now.Add(4 * time.Minute)
	if err := g.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.c.Count() != 1 {
		t.Fatal("expired statistics were never refreshed")
	}
}
func TestConcurrentReconciliationRespectsCapacity(t *testing.T) {
	f := newFixture(t)
	var workers sync.WaitGroup
	for range 24 {
		workers.Go(func() {
			if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 20}}); err != nil {
				t.Error(err)
			}
			_ = f.c.Snapshot()
			if err := f.c.Tick(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if f.api.jitCalls != 1 || len(f.ws.VMs) != 1 {
		t.Fatal("concurrent callbacks exceeded maximum capacity")
	}
}

func TestRestartDuringGuestBootPreservesVMUntilDeadline(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	name := f.name()
	f.ws.life[name] = hyperv.Lifecycle{}
	f.c.pendingJIT = nil // Memory is lost in a process restart; no JIT was persisted.
	f.c.Session(nil)
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ws.stopped != 0 || f.c.Count() != 1 {
		t.Fatal("restart prematurely killed a booting VM")
	}
	f.now = f.now.Add(time.Duration(f.c.Config.Runners.StartupTimeoutSeconds+1) * time.Second)
	f.ws.failure = "probe"
	f.api.busy = true
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ws.stopped != 0 || f.c.Count() != 1 {
		t.Fatal("startup timeout bypassed server busy fence")
	}
}

func TestNetworkLossRecoveryPreservesBusyJobAndCleansAfterRemoteCompletion(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	name := f.name()
	f.c.State.Runners[name].Phase = "busy"
	f.api.busy = true
	f.ws.failure = "probe"
	f.now = f.now.Add(2 * time.Hour)
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ws.stopped != 0 || f.ws.removed != 0 {
		t.Fatal("unreachable active job was killed")
	}
	f.api.busy = false
	f.api.assigned = 0 // Completion event was lost with the old listener.
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.c.Count() != 0 || f.ws.removed != 1 {
		t.Fatal("unreachable completed guest leaked")
	}
}

func TestFailedCloudInitNeverReceivesJIT(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	f.ws.life[f.name()] = hyperv.Lifecycle{Ready: true, Failed: true}
	f.ws.failure = "deliver"
	f.c.Session(nil)
	if err := f.c.Tick(context.Background()); err != nil {
		t.Fatal("failed guest was given JIT before retirement", err)
	}
	if f.c.Count() != 0 || f.ws.removed != 1 {
		t.Fatal("failed initialization was not cleaned up")
	}
}
