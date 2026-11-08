package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/clcdpc/github-runners/internal/config"
	gh "github.com/google/go-github/v79/github"
)

type fakeScaleAPI struct {
	found                  *scaleset.RunnerScaleSet
	getErr, errorCreate    error
	creates, updates, gets int
}

func (f *fakeScaleAPI) GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error) {
	f.gets++
	return f.found, f.getErr
}
func (f *fakeScaleAPI) CreateRunnerScaleSet(_ context.Context, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.creates++
	s.ID = 42
	f.found = s
	return s, f.errorCreate
}
func (f *fakeScaleAPI) UpdateRunnerScaleSet(_ context.Context, _ int, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.updates++
	f.found = s
	return s, nil
}
func TestScaleSetDiscoveryAndReconciliation(t *testing.T) {
	f := &fakeScaleAPI{}
	ctx := context.Background()
	s, err := EnsureScaleSet(ctx, f, 7, "clc-linux")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != 42 || s.RunnerGroupID != 7 || !s.RunnerSetting.DisableUpdate || f.creates != 1 {
		t.Fatal("incorrect scale-set creation")
	}
	if _, err = EnsureScaleSet(ctx, f, 7, "clc-linux"); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || f.updates != 0 {
		t.Fatal("reconciliation not idempotent")
	}
	f.found.Labels = []scaleset.Label{{Name: "preserved", Type: "System"}}
	f.found.RunnerSetting.DisableUpdate = false
	if _, err = EnsureScaleSet(ctx, f, 7, "clc-linux"); err != nil {
		t.Fatal(err)
	}
	if f.updates != 1 || len(f.found.Labels) != 2 || f.found.Labels[0].Name != "preserved" {
		t.Fatal("reconciliation changed unrelated labels")
	}
	f.found.RunnerGroupID = 99
	if _, err = EnsureScaleSet(ctx, f, 7, "clc-linux"); err == nil {
		t.Fatal("foreign group was reconciled")
	}
}
func TestScaleSetLostCreateResponseAndDiscoveryFailure(t *testing.T) {
	f := &fakeScaleAPI{errorCreate: errors.New("response lost")}
	if _, err := EnsureScaleSet(context.Background(), f, 7, "clc-linux"); err != nil {
		t.Fatal("lost create response not recovered", err)
	}
	f = &fakeScaleAPI{getErr: errors.New("authentication failed")}
	if _, err := EnsureScaleSet(context.Background(), f, 7, "clc-linux"); err == nil || f.creates != 0 {
		t.Fatal("created a scale set after failed discovery")
	}
}
func restFixture(t *testing.T, handler http.HandlerFunc) *gh.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := gh.NewClient(server.Client())
	client.BaseURL, _ = url.Parse(server.URL + "/")
	return client
}
func testConfig(t *testing.T) config.Config {
	t.Helper()
	c, err := config.Load("../../config/controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestSetupLeavesExternalRepositoryPolicyAlone(t *testing.T) {
	mutations := 0
	client := restFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations++
			t.Errorf("unexpected policy mutation %s", r.URL.Path)
		}
		io.WriteString(w, `{"runner_groups":[{"id":7,"name":"CLC-Runners","visibility":"selected","allows_public_repositories":true}],"total_count":1}`)
	})
	group, err := SetupGroup(context.Background(), client, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if group.GetID() != 7 || mutations != 0 {
		t.Fatal("external group policy changed")
	}
}
func TestSetupCreatesSelectedGroupAndDoesNotChangeOthers(t *testing.T) {
	creates := 0
	client := restFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/clcdpc/actions/runner-groups" {
			t.Errorf("unexpected resource: %s", r.URL.Path)
		}
		if r.Method == "GET" {
			io.WriteString(w, `{"runner_groups":[{"id":1,"name":"Default","visibility":"all","default":true}],"total_count":1}`)
			return
		}
		if r.Method != "POST" {
			t.Errorf("unexpected mutation %s", r.Method)
		}
		var body gh.CreateRunnerGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if *body.Name != "CLC-Runners" || *body.Visibility != "selected" || *body.AllowsPublicRepositories {
			t.Error("group was not created with restricted visibility")
		}
		creates++
		w.WriteHeader(201)
		io.WriteString(w, `{"id":7,"name":"CLC-Runners","visibility":"selected"}`)
	})
	if _, err := SetupGroup(context.Background(), client, testConfig(t)); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatal("expected one group creation")
	}
}
func TestUnsafeExistingGroupFailsWithoutMutation(t *testing.T) {
	for _, field := range []string{`"visibility":"all"`, `"visibility":"selected","inherited":true`, `"visibility":"selected","default":true`} {
		client := restFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Error("unsafe group mutated")
			}
			io.WriteString(w, `{"runner_groups":[{"id":7,"name":"CLC-Runners",`+field+`}]}`)
		})
		if _, err := SetupGroup(context.Background(), client, testConfig(t)); err == nil {
			t.Fatal("unsafe existing group accepted")
		}
	}
}

func TestAuthenticationChecksReadAndWriteWithoutResourceChanges(t *testing.T) {
	requests := []string{}
	client := restFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/orgs/clcdpc/actions/runner-groups":
			io.WriteString(w, `{"runner_groups":[]}`)
		case "/orgs/clcdpc/actions/runners/registration-token":
			if r.Method != "POST" {
				t.Error("write permission was not exercised")
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"token":"short-lived-secret"}`)
		default:
			t.Error("unexpected resource modification", r.URL.Path)
		}
	})
	if err := CheckAuthentication(context.Background(), client, testConfig(t)); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0] != "GET /orgs/clcdpc/actions/runner-groups" {
		t.Fatal("authentication did not check organization discovery first", requests)
	}
}

func TestAuthenticationFailuresHaveSafeActionableGuidance(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		for _, failWrite := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/write=%t", status, failWrite), func(t *testing.T) {
				calls := 0
				client := restFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					if failWrite && calls == 1 {
						io.WriteString(w, `{"runner_groups":[]}`)
						return
					}
					w.WriteHeader(status)
					io.WriteString(w, `{"message":"github_pat_MUST_NEVER_APPEAR secret response body"}`)
				})
				err := CheckAuthentication(context.Background(), client, testConfig(t))
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || !strings.Contains(err.Error(), "rerun Activate") || strings.Contains(err.Error(), "MUST_NEVER_APPEAR") {
					t.Fatal("unsafe or unhelpful authentication error", err)
				}
				if !failWrite && calls != 1 {
					t.Fatal("authentication continued after failed read access")
				}
			})
		}
	}
}

func TestReadOnlyInfrastructureReusesPermanentResources(t *testing.T) {
	mutations := 0
	client := restFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations++
		}
		io.WriteString(w, `{"runner_groups":[{"id":7,"name":"CLC-Runners","visibility":"selected","allows_public_repositories":true}]}`)
	})
	api := &fakeScaleAPI{found: &scaleset.RunnerScaleSet{ID: 42, Name: "clc-linux", RunnerGroupID: 7, Labels: []scaleset.Label{{Name: "clc-linux"}, {Name: "preserved"}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}}}
	current, err := InfrastructureCurrent(context.Background(), client, api, testConfig(t), 42)
	if err != nil || !current || mutations != 0 || api.creates != 0 || api.updates != 0 {
		t.Fatal("read-only probe modified resources or external repository policy", current, err)
	}
	if current, err = InfrastructureCurrent(context.Background(), client, api, testConfig(t), 0); err != nil || current {
		t.Fatal("unjournaled scale-set identity was treated as initialized")
	}
	if _, err = InfrastructureCurrent(context.Background(), client, api, testConfig(t), 99); err == nil {
		t.Fatal("saved scale-set replacement was accepted")
	}
	api.found.RunnerSetting.DisableUpdate = false
	if current, err = InfrastructureCurrent(context.Background(), client, api, testConfig(t), 42); err != nil || current {
		t.Fatal("required scale-set repair was missed")
	}
}

func TestInfrastructureCheckMissingGroupPreservesSavedIdentity(t *testing.T) {
	client := restFixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"runner_groups":[]}`) })
	api := &fakeScaleAPI{}
	if current, err := InfrastructureCurrent(context.Background(), client, api, testConfig(t), 0); current || err != nil {
		t.Fatal("fresh infrastructure check failed", err)
	}
	if _, err := InfrastructureCurrent(context.Background(), client, api, testConfig(t), 42); err == nil || api.creates != 0 {
		t.Fatal("missing permanent identity was silently replaced")
	}
}

func TestInfrastructureCheckRepositoryGrantsAreAdditiveAndReadOnly(t *testing.T) {
	c := testConfig(t)
	c.GitHub.Repositories = []string{"desired"}
	granted := true
	client := restFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("probe modified repository grants")
		}
		if strings.HasSuffix(r.URL.Path, "/repositories") {
			if granted {
				io.WriteString(w, `{"repositories":[{"name":"unrelated"},{"name":"desired"}]}`)
			} else {
				io.WriteString(w, `{"repositories":[{"name":"unrelated"}]}`)
			}
			return
		}
		io.WriteString(w, `{"runner_groups":[{"id":7,"name":"CLC-Runners","visibility":"selected"}]}`)
	})
	api := &fakeScaleAPI{found: &scaleset.RunnerScaleSet{ID: 42, Name: "clc-linux", RunnerGroupID: 7, Labels: []scaleset.Label{{Name: "clc-linux"}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}}}
	if current, err := InfrastructureCurrent(context.Background(), client, api, c, 42); err != nil || !current {
		t.Fatal("existing selected grant was not reused", err)
	}
	granted = false
	if current, err := InfrastructureCurrent(context.Background(), client, api, c, 42); err != nil || current {
		t.Fatal("missing selected grant was not detected", err)
	}
}
