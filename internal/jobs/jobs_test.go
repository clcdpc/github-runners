package jobs

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/clcdpc/github-runners/internal/controller"
	"github.com/clcdpc/github-runners/internal/restart"
)

var testNow = time.Date(2026, 11, 1, 7, 12, 38, 0, time.UTC)

func testRuntime() ControllerStatus {
	started, updated := testNow.Add(-2*time.Hour), testNow
	return ControllerStatus{Status: "listening", ProcessAlive: true, Fresh: true, StartedAt: &started, UpdatedAt: &updated}
}

func TestRestartStateExtendsJobsJSONAndKeepsTableConcise(t *testing.T) {
	runtime := testRuntime()
	runtime.Restart = &restart.State{Version: 1, State: "Draining", Pending: true, Blocker: strings.Repeat("Accepted jobs still running; ", 12), ActiveAssignments: 2}
	report := Build(testState(), runtime, 3, testNow, time.Minute)
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err = json.Unmarshal(b, &output); err != nil {
		t.Fatal(err)
	}
	if output["schemaVersion"] != float64(1) || output["controller"].(map[string]any)["restart"].(map[string]any)["state"] != "Draining" {
		t.Fatal("restart changed Jobs schema compatibility")
	}
	var table bytes.Buffer
	if err = WriteTable(&table, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table.String(), "Controller restart draining") {
		t.Fatal("pending restart invisible")
	}
	for _, line := range strings.Split(table.String(), "\n") {
		if displayWidth(line) > 80 {
			t.Fatal("restart indicator exceeded terminal width", line)
		}
	}
}

func testRunner(suffix string, id int) *controller.Runner {
	return &controller.Runner{Name: "clc-linux-12345678-" + suffix, RunnerID: id, VMID: "vm-" + suffix, Phase: "busy",
		CreatedAt: testNow.Add(-time.Hour), ExecutionObservedAt: testNow.Add(-time.Second).Format(time.RFC3339Nano),
		Job: &controller.JobAssignment{Repository: "clcdpc/asap-pocketbase", DisplayName: "Full CI execution", WorkflowRunID: 123,
			AssignmentJobID: "opaque-guid-" + suffix, StartedAt: testNow.Add(-38 * time.Minute).Format(time.RFC3339Nano), StartSource: "runner-hook"}}
}

func testState(runners ...*controller.Runner) *controller.State {
	s := &controller.State{Version: 2, Runners: map[string]*controller.Runner{}}
	for _, r := range runners {
		s.Runners[r.Name] = r
	}
	return s
}

func TestBuildAndJSONSchemaDoNotMutateJournal(t *testing.T) {
	a, b := testRunner("fdd987dc2b5b", 398), testRunner("a1b2c3d4e5f6", 397)
	b.Job.DisplayName, b.Job.StartedAt = "Build and test", testNow.Add(-7*time.Minute).Format(time.RFC3339)
	s := testState(a, b)
	before, _ := json.Marshal(s)
	report := Build(s, testRuntime(), 3, testNow, time.Minute)
	if report.SchemaVersion != 1 || report.MaximumConcurrency != 3 || report.OccupiedCapacity != 2 || report.AvailableCapacity != 1 || len(report.Runners) != 2 {
		t.Fatalf("incorrect report: %+v", report)
	}
	if report.Runners[0].Name != b.Name || report.Runners[1].Name != a.Name || report.Runners[0].Repository != report.Runners[1].Repository ||
		*report.Runners[0].ElapsedSeconds != 420 || *report.Runners[1].ElapsedSeconds != 2280 || report.Runners[0].WorkflowJobID != 0 {
		t.Fatal("simultaneous jobs in one repository were mixed", report.Runners)
	}
	encoded, err := json.Marshal(report)
	if err != nil || !json.Valid(encoded) {
		t.Fatal("invalid JSON", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schemaVersion", "generatedAt", "controller", "maximumConcurrency", "occupiedCapacity", "availableCapacity", "runners"} {
		if _, ok := decoded[key]; !ok {
			t.Fatal("missing report property", key)
		}
	}
	row := decoded["runners"].([]any)[0].(map[string]any)
	for _, key := range []string{"name", "shortName", "runnerId", "vmId", "phase", "state", "executionState", "executionSource", "phaseSince", "repository", "workflowName", "workflowRef", "yamlJobId", "jobDisplayName", "workflowRunId", "workflowJobId", "assignmentJobId", "requestId", "runAttempt", "jobUrl", "assignedAt", "jobStartedAt", "elapsedSeconds", "elapsedSource", "metadataStatus", "lookupError"} {
		if _, ok := row[key]; !ok {
			t.Fatal("missing runner property", key)
		}
	}
	if row["name"] != b.Name || row["yamlJobId"] != "" || row["workflowName"] != "" || row["elapsedSeconds"] != float64(420) {
		t.Fatal("JSON lost identity or invented unavailable metadata", row)
	}
	after, _ := json.Marshal(s)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only view changed journal")
	}
}

func TestDefaultTableAndEmptyState(t *testing.T) {
	r := testRunner("fdd987dc2b5b", 398)
	report := Build(testState(r), testRuntime(), 3, testNow, time.Minute)
	var output bytes.Buffer
	if err := WriteTable(&output, report); err != nil {
		t.Fatal(err)
	}
	want := "CLC GitHub Runners - Active Jobs\n\n" +
		"Runner       State        Repository             Job                  Elapsed\n" +
		"fdd987dc2b5b Running      clcdpc/asap-pocketbase Full CI execution    38m\n\n" +
		"1 running | 1/3 capacity\n"
	if output.String() != want {
		t.Fatalf("table changed:\n%q\nwant:\n%q", output.String(), want)
	}
	for _, state := range []*controller.State{nil, testState()} {
		empty := Build(state, ControllerStatus{Status: "uninitialized"}, 3, testNow, time.Minute)
		output.Reset()
		if err := WriteTable(&output, empty); err != nil || !strings.Contains(output.String(), "No active controller-managed runners.") || !strings.Contains(output.String(), "0/3 capacity") {
			t.Fatal("uninformative empty state", output.String(), err)
		}
		data, _ := json.Marshal(empty)
		if !strings.Contains(string(data), `"runners":[]`) {
			t.Fatal("empty collection must remain an array", string(data))
		}
	}
}

func TestLifecycleRowsAndCompletionHideAssignments(t *testing.T) {
	for _, tc := range []struct{ phase, state string }{{"provisioning", "Provisioning"}, {"starting", "Provisioning"}, {"online", "Idle"}, {"completed", "Completing"}, {"cleaning", "Cleaning up"}, {"failed", "Failed"}, {"recovering", "Unknown"}} {
		t.Run(tc.phase, func(t *testing.T) {
			r := testRunner("fdd987dc2b5b", 398)
			r.Phase, r.PhaseSince = tc.phase, testNow.Add(-18*time.Second).Format(time.RFC3339Nano)
			report := Build(testState(r), testRuntime(), 3, testNow, time.Minute)
			row := report.Runners[0]
			if row.State != tc.state || row.Phase != tc.phase || row.Repository != "" || row.JobDisplayName != "" || row.JobStartedAt != nil || report.OccupiedCapacity != 1 {
				t.Fatal("terminal/unassigned row retained an assignment or lost occupied capacity", row)
			}
			if tc.state == "Provisioning" || tc.state == "Completing" || tc.state == "Cleaning up" || tc.state == "Failed" {
				if row.ElapsedSeconds == nil || *row.ElapsedSeconds != 18 || row.ElapsedSource != "phase" {
					t.Fatal("phase duration was confused with job duration", row)
				}
			}
		})
	}
}

func TestMissingDelayedAndMalformedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*controller.Runner)
	}{
		{"no metadata", func(r *controller.Runner) { r.Job = nil }},
		{"no start", func(r *controller.Runner) { r.Job.StartedAt = "" }},
		{"bad start", func(r *controller.Runner) { r.Job.StartedAt = "not-a-timestamp" }},
		{"future start", func(r *controller.Runner) { r.Job.StartedAt = testNow.Add(time.Hour).Format(time.RFC3339) }},
		{"zero start", func(r *controller.Runner) { r.Job.StartedAt = time.Time{}.Format(time.RFC3339) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testRunner("fdd987dc2b5b", 398)
			tc.change(r)
			row := Build(testState(r), testRuntime(), 3, testNow, time.Minute).Runners[0]
			if row.State != "Running" || row.ElapsedSeconds != nil {
				t.Fatal("missing timestamp was replaced with VM creation/assignment time", row)
			}
		})
	}
}

func TestUnconfirmedRestartOfflineAndOldObservationsAreUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*controller.Runner, *ControllerStatus)
	}{
		{"controller unavailable", func(_ *controller.Runner, rt *ControllerStatus) { rt.ProcessAlive = false; rt.Fresh = false }},
		{"stale heartbeat", func(_ *controller.Runner, rt *ControllerStatus) { rt.Fresh = false }},
		{"legacy journal", func(r *controller.Runner, _ *ControllerStatus) { r.ExecutionObservedAt = "" }},
		{"old observation", func(r *controller.Runner, _ *ControllerStatus) {
			r.ExecutionObservedAt = testNow.Add(-2 * time.Minute).Format(time.RFC3339)
		}},
		{"restart", func(r *controller.Runner, rt *ControllerStatus) { start := testNow; rt.StartedAt = &start }},
		{"future clock", func(r *controller.Runner, _ *ControllerStatus) {
			r.ExecutionObservedAt = testNow.Add(time.Hour).Format(time.RFC3339)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, rt := testRunner("fdd987dc2b5b", 398), testRuntime()
			tc.change(r, &rt)
			row := Build(testState(r), rt, 3, testNow, time.Minute).Runners[0]
			if row.State != "Unknown" || row.ExecutionState != "unknown" || row.ElapsedSeconds != nil || row.Repository == "" {
				t.Fatal("stale persisted assignment reported running or discarded known metadata", row)
			}
		})
	}
}

func TestUTCElapsedAcrossDSTAndAdvancingTime(t *testing.T) {
	r := testRunner("fdd987dc2b5b", 398)
	// Both 01:30 local timestamps exist on the fall-back date.
	r.Job.StartedAt = "2026-11-01T01:30:00-04:00"
	r.ExecutionObservedAt = "2026-11-01T01:30:00-05:00"
	now, _ := time.Parse(time.RFC3339, "2026-11-01T01:30:00-05:00")
	rt := testRuntime()
	started := now.Add(-2 * time.Hour)
	rt.StartedAt = &started
	first := Build(testState(r), rt, 3, now, time.Minute).Runners[0]
	second := Build(testState(r), rt, 3, now.Add(5*time.Second), time.Minute).Runners[0]
	if *first.ElapsedSeconds != 3600 || *second.ElapsedSeconds != 3605 || first.JobStartedAt.Location() != time.UTC {
		t.Fatal("DST/UTC elapsed computation is incorrect", first, second)
	}
}

func TestDurationsLongNamesAndTerminalSafety(t *testing.T) {
	for seconds, want := range map[int64]string{-10: "0s", 18: "18s", 240: "4m", 2280: "38m", 4320: "1h 12m"} {
		if got := Duration(seconds); got != want {
			t.Fatalf("Duration(%d)=%s want %s", seconds, got, want)
		}
	}
	r := testRunner("fdd987dc2b5b", 398)
	r.Job.Repository = "clcdpc/very-long-repository-name-for-a-narrow-terminal"
	r.Job.DisplayName = strings.Repeat("Build and test ", 20) + "\x1b[31m\nline2 github_pat_sensitive_fixture"
	report := Build(testState(r), testRuntime(), 3, testNow, time.Minute)
	var output bytes.Buffer
	if err := WriteTable(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if utf8.RuneCountInString(line) > 80 || strings.ContainsRune(line, '\x1b') {
			t.Fatal("table exceeded terminal width or emitted an escape", line)
		}
	}
	if !strings.Contains(output.String(), "...") || report.Runners[0].Repository != r.Job.Repository || len(report.Runners[0].JobDisplayName) < 100 || strings.Contains(report.Runners[0].JobDisplayName, "sensitive_fixture") {
		t.Fatal("long metadata lost, unredacted, or not truncated in table", report.Runners[0], output.String())
	}
}

func TestLookupTargetsDoNotSearchForUnknownAssignments(t *testing.T) {
	r := testRunner("fdd987dc2b5b", 398)
	full := Build(testState(r), testRuntime(), 3, testNow, time.Minute)
	if !full.LookupTargets() {
		t.Fatal("known workflow run cannot be enriched")
	}
	r.Job = nil
	missing := Build(testState(r), testRuntime(), 3, testNow, time.Minute)
	if missing.LookupTargets() || reflect.DeepEqual(full, missing) {
		t.Fatal("unknown assignment caused workflow discovery")
	}
}

func TestUnicodeConsoleCellsHaveStableWidths(t *testing.T) {
	for _, text := range []string{"Build", "テストとビルドを実行するための長い名前", "编译并运行测试用例", "🧪 Build and test workflow", "Cafe\u0301"} {
		if got := cell(text, 20); displayWidth(got) != 20 || !utf8.ValidString(got) {
			t.Fatalf("invalid terminal cell width for %q: %q", text, got)
		}
	}
}
