package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v79/github"
)

func apiJob(name string, runnerID, jobID int64) *gh.WorkflowJob {
	return &gh.WorkflowJob{ID: gh.Ptr(jobID), RunID: gh.Ptr(int64(123)), RunnerID: gh.Ptr(runnerID), RunnerName: gh.Ptr(name),
		Status: gh.Ptr("in_progress"), Name: gh.Ptr("Build and test"), WorkflowName: gh.Ptr("Full CI"), RunAttempt: gh.Ptr(int64(2)),
		StartedAt: &gh.Timestamp{Time: testNow.Add(-4 * time.Minute)}, HTMLURL: gh.Ptr("https://github.com/clcdpc/asap-pocketbase/actions/runs/123/job/" + strconv.FormatInt(jobID, 10))}
}

func serverAPI(t *testing.T, handler http.HandlerFunc) *gh.ActionsService {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := gh.NewClient(server.Client())
	client.BaseURL, _ = url.Parse(server.URL + "/")
	return client.Actions
}

func TestGitHubMatchesConcurrentSameRepositoryJobsByRunnerIdentity(t *testing.T) {
	a, b := testRunner("fdd987dc2b5b", 398), testRunner("a1b2c3d4e5f6", 397)
	var requests atomic.Int32
	api := serverAPI(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.URL.Path != "/repos/clcdpc/asap-pocketbase/actions/runs/123/jobs" || r.URL.Query().Get("filter") != "all" || r.URL.Query().Get("per_page") != "100" {
			t.Error("unexpected mutation or discovery request", r.Method, r.URL)
		}
		one, two := apiJob(a.Name, 398, 10001), apiJob(b.Name, 397, 10002)
		one.Name, two.Name = gh.Ptr("Same display name"), gh.Ptr("Same display name")
		wrong := apiJob(a.Name, 999, 10003)
		_ = json.NewEncoder(w).Encode(gh.Jobs{Jobs: []*gh.WorkflowJob{wrong, two, one}})
	})
	report := Build(testState(a, b), ControllerStatus{Status: "not-running"}, 3, testNow, time.Minute)
	Enrich(context.Background(), &report, api)
	if requests.Load() != 1 {
		t.Fatal("same run queried more than once", requests.Load())
	}
	for _, r := range report.Runners {
		wantID := int64(10001)
		if r.RunnerID == 397 {
			wantID = 10002
		}
		if r.WorkflowJobID != wantID || r.RunAttempt != 2 || r.WorkflowName != "Full CI" || r.JobDisplayName != "Same display name" || r.State != "Running" ||
			r.ElapsedSource != "github-api" || r.ExecutionSource != "github-api" || *r.ElapsedSeconds != 240 || r.YAMLJobID != "" || r.JobURL == "" || r.LookupError != "" {
			t.Fatal("incorrect stable association or enrichment", r)
		}
	}
	if a.Job.DisplayName != "Full CI execution" || a.Job.StartSource != "runner-hook" {
		t.Fatal("lookup wrote to journal")
	}
}

func TestGitHubCompletionClearsOnlyItsOwnView(t *testing.T) {
	a, b := testRunner("fdd987dc2b5b", 398), testRunner("a1b2c3d4e5f6", 397)
	api := serverAPI(t, func(w http.ResponseWriter, r *http.Request) {
		one := apiJob(a.Name, 398, 10001)
		one.Status, one.Conclusion = gh.Ptr("completed"), gh.Ptr("cancelled")
		_ = json.NewEncoder(w).Encode(gh.Jobs{Jobs: []*gh.WorkflowJob{one, apiJob(b.Name, 397, 10002)}})
	})
	report := Build(testState(a, b), testRuntime(), 3, testNow, time.Minute)
	Enrich(context.Background(), &report, api)
	if report.OccupiedCapacity != 2 || len(report.Runners) != 2 {
		t.Fatal("premature capacity release")
	}
	for _, r := range report.Runners {
		if r.Name == a.Name && (r.State != "Completing" || r.Repository != "" || r.JobDisplayName != "" || r.ElapsedSeconds != nil) {
			t.Fatal("completed assignment remained active", r)
		}
		if r.Name == b.Name && (r.State != "Running" || r.WorkflowJobID != 10002 || r.Repository == "") {
			t.Fatal("completion modified other runner", r)
		}
	}
}

func TestGitHubFailuresRetainLocalMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"permission", 403, `{"message":"token=github_pat_secret_must_not_appear"}`},
		{"unavailable", 503, `{"message":"server unavailable"}`},
		{"malformed", 200, `{broken`},
		{"bad timestamp", 200, `{"jobs":[{"id":10001,"started_at":"invalid"}]}`},
		{"missing job", 200, `{"jobs":[]}`},
		{"unassigned runner", 200, `{"jobs":[{"id":10001,"run_id":123,"runner_id":398,"runner_name":"another-runner"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := serverAPI(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.code); _, _ = w.Write([]byte(tc.body)) })
			report := Build(testState(testRunner("fdd987dc2b5b", 398)), testRuntime(), 3, testNow, time.Minute)
			Enrich(context.Background(), &report, api)
			row := report.Runners[0]
			if row.LookupError == "" || strings.Contains(row.LookupError, "secret_must_not_appear") || row.Repository != "clcdpc/asap-pocketbase" || row.JobDisplayName != "Full CI execution" || row.State != "Running" || *row.ElapsedSeconds != 2280 {
				t.Fatal("failure discarded local data or leaked server text", row)
			}
		})
	}
}

type blockingAPI struct {
	mu              sync.Mutex
	active, maximum int
	entered         chan struct{}
}

func (b *blockingAPI) ListWorkflowJobs(ctx context.Context, _, _ string, _ int64, _ *gh.ListWorkflowJobsOptions) (*gh.Jobs, *gh.Response, error) {
	b.mu.Lock()
	b.active++
	b.maximum = max(b.active, b.maximum)
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-ctx.Done()
	b.mu.Lock()
	b.active--
	b.mu.Unlock()
	return nil, nil, ctx.Err()
}

func TestGitHubTimeoutAndBoundedConcurrency(t *testing.T) {
	api := &blockingAPI{entered: make(chan struct{}, 12)}
	report := Build(nil, testRuntime(), 12, testNow, time.Minute)
	for i := range 12 {
		r := testRunner("fdd987dc2b5b", i+1)
		r.Job.WorkflowRunID = int64(i + 1)
		report.Runners = append(report.Runners, Build(testState(r), testRuntime(), 12, testNow, time.Minute).Runners[0])
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { Enrich(ctx, &report, api); close(done) }()
	for range 3 {
		select {
		case <-api.entered:
		case <-time.After(time.Second):
			t.Fatal("lookups did not start concurrently")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lookups ignored cancellation")
	}
	if api.maximum != 3 || api.active != 0 {
		t.Fatal("unbounded requests", api.maximum, api.active)
	}
	for _, r := range report.Runners {
		if !strings.Contains(r.LookupError, "timed out or was cancelled") || r.Repository == "" {
			t.Fatal("timeout discarded local data", r)
		}
	}
	if !strings.Contains(lookupError(context.DeadlineExceeded), "timed out") || strings.Contains(lookupError(errors.New("token=secret")), "secret") {
		t.Fatal("unsafe error")
	}
}

func TestGitHubPaginationAndAmbiguousAssignments(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(strconv.FormatBool(ambiguous), func(t *testing.T) {
			var count atomic.Int32
			api := serverAPI(t, func(w http.ResponseWriter, req *http.Request) {
				count.Add(1)
				var items []*gh.WorkflowJob
				if req.URL.Query().Get("page") == "1" {
					w.Header().Set("Link", "<http://"+req.Host+req.URL.Path+"?page=2>; rel=\"next\"")
					if ambiguous {
						items = append(items, apiJob("clc-linux-12345678-fdd987dc2b5b", 398, 10001))
					}
				} else {
					items = append(items, apiJob("clc-linux-12345678-fdd987dc2b5b", 398, 10002))
				}
				_ = json.NewEncoder(w).Encode(gh.Jobs{Jobs: items})
			})
			report := Build(testState(testRunner("fdd987dc2b5b", 398)), testRuntime(), 3, testNow, time.Minute)
			Enrich(context.Background(), &report, api)
			if count.Load() != 2 || (report.Runners[0].LookupError != "") != ambiguous {
				t.Fatal("pagination/match uniqueness lost", count.Load(), report.Runners)
			}
		})
	}
	var count atomic.Int32
	api := serverAPI(t, func(w http.ResponseWriter, req *http.Request) {
		n := count.Add(1)
		w.Header().Set("Link", "<http://"+req.Host+req.URL.Path+"?page="+strconv.Itoa(int(n+1))+">; rel=\"next\"")
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	})
	report := Build(testState(testRunner("fdd987dc2b5b", 398)), testRuntime(), 3, testNow, time.Minute)
	Enrich(context.Background(), &report, api)
	if count.Load() != 5 || report.Runners[0].LookupError == "" {
		t.Fatal("unbounded pagination", count.Load(), report.Runners)
	}
}

func TestGitHubDeadlineRetainsLocalState(t *testing.T) {
	api := &blockingAPI{entered: make(chan struct{}, 1)}
	report := Build(testState(testRunner("fdd987dc2b5b", 398)), testRuntime(), 3, testNow, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	Enrich(ctx, &report, api)
	if !strings.Contains(report.Runners[0].LookupError, "timed out") || report.Runners[0].State != "Running" {
		t.Fatal("deadline removed local execution evidence", report.Runners[0])
	}
}
