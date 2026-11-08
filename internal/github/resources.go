package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/clcdpc/github-runners/internal/config"
	gh "github.com/google/go-github/v79/github"
)

type ScaleAPI interface {
	GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(context.Context, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(context.Context, int, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
}

func EnsureScaleSet(ctx context.Context, api ScaleAPI, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	ss, err := api.GetRunnerScaleSet(ctx, groupID, name)
	if err != nil {
		return nil, err
	}
	if ss == nil {
		ss, err = api.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: name, RunnerGroupID: groupID, Labels: []scaleset.Label{{Name: name, Type: "System"}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}})
		// A lost create response or another setup may have created it already.
		if err != nil {
			existing, findErr := api.GetRunnerScaleSet(ctx, groupID, name)
			if findErr == nil && existing != nil {
				ss = existing
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if ss == nil || ss.ID <= 0 || ss.Name != name || ss.RunnerGroupID != groupID {
		return nil, errors.New("scale-set identity does not match configured runner group/name")
	}
	hasLabel := hasScaleSetLabel(ss, name)
	if !hasLabel || !ss.RunnerSetting.DisableUpdate {
		copy := *ss
		copy.Labels = append([]scaleset.Label{}, ss.Labels...)
		if !hasLabel {
			copy.Labels = append(copy.Labels, scaleset.Label{Name: name, Type: "System"})
		}
		copy.RunnerSetting.DisableUpdate = true
		return api.UpdateRunnerScaleSet(ctx, ss.ID, &copy)
	}
	return ss, nil
}

func REST(token string) *gh.Client {
	return gh.NewClient(&http.Client{Timeout: 60 * time.Second}).WithAuthToken(token)
}

// CheckAuthentication exercises organization discovery and runner write access
// without registering a runner or changing group/scale-set policy. The short
// lived registration token is discarded and must never be printed or persisted.
func CheckAuthentication(ctx context.Context, client *gh.Client, c config.Config) error {
	if _, err := FindGroup(ctx, client, c); err != nil {
		return authenticationError(err)
	}
	token, _, err := client.Actions.CreateOrganizationRegistrationToken(ctx, c.Organization())
	if err != nil {
		return authenticationError(err)
	}
	if token == nil || token.GetToken() == "" {
		return errors.New("GitHub returned no organization registration token; runner write permission could not be validated")
	}
	token.Token = nil
	return nil
}

func authenticationError(err error) error {
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		switch response.Response.StatusCode {
		case http.StatusUnauthorized:
			return errors.New("GitHub HTTP 401: the saved PAT is invalid, expired or revoked. Replace it with runners.ps1 SetToken using the same -Config, then rerun Activate. The existing PAT was not overwritten")
		case http.StatusForbidden:
			return errors.New("GitHub HTTP 403: organization access denied. Use a fine-grained PAT owned by the configured organization with Administration read and Self-hosted runners read/write; check organization approval, SSO and rate limits. Replace it with runners.ps1 SetToken using the same -Config if needed, then rerun Activate. The existing PAT was not overwritten")
		case http.StatusNotFound:
			return errors.New("GitHub HTTP 404: the organization is missing or hidden from this PAT. Check the configured organization, PAT resource owner and organization approval; then rerun Activate")
		}
	}
	return fmt.Errorf("GitHub authentication/permission check failed: %w", err)
}

func scaleSetCurrent(ss *scaleset.RunnerScaleSet, groupID int, name string) bool {
	if ss == nil || ss.ID <= 0 || ss.Name != name || ss.RunnerGroupID != groupID || !ss.RunnerSetting.DisableUpdate {
		return false
	}
	return hasScaleSetLabel(ss, name)
}

func hasScaleSetLabel(ss *scaleset.RunnerScaleSet, name string) bool {
	for _, label := range ss.Labels {
		if strings.EqualFold(label.Name, name) {
			return true
		}
	}
	return false
}

// InfrastructureCurrent is read-only and may run while the listener owns the
// journal lock. A previously initialized identity is never silently replaced.
func InfrastructureCurrent(ctx context.Context, client *gh.Client, api ScaleAPI, c config.Config, savedID int) (bool, error) {
	group, err := FindGroup(ctx, client, c)
	if err != nil {
		return false, err
	}
	if group == nil {
		if savedID != 0 {
			return false, errors.New("saved scale-set ID has no configured runner group in GitHub; restore the original resources/configuration before activating")
		}
		return false, nil
	}
	ss, err := api.GetRunnerScaleSet(ctx, int(group.GetID()), c.GitHub.ScaleSet)
	if err != nil {
		return false, err
	}
	if savedID != 0 && (ss == nil || ss.ID != savedID) {
		return false, errors.New("saved scale-set ID differs from GitHub; investigate before adopting a replacement")
	}
	if savedID == 0 || !scaleSetCurrent(ss, int(group.GetID()), c.GitHub.ScaleSet) {
		return false, nil
	}
	if len(c.GitHub.Repositories) == 0 {
		return true, nil
	}
	if group.GetAllowsPublicRepositories() != c.GitHub.AllowPublicRepositories {
		return false, nil
	}
	grants := map[string]bool{}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		repositories, response, err := client.Actions.ListRepositoryAccessRunnerGroup(ctx, c.Organization(), group.GetID(), opts)
		if err != nil {
			return false, err
		}
		for _, repo := range repositories.Repositories {
			grants[strings.ToLower(repo.GetName())] = true
		}
		if response.NextPage == 0 {
			break
		}
		opts.Page = response.NextPage
	}
	for _, name := range c.GitHub.Repositories {
		if !grants[strings.ToLower(name)] {
			return false, nil
		}
	}
	return true, nil
}

func FindGroup(ctx context.Context, client *gh.Client, c config.Config) (*gh.RunnerGroup, error) {
	var match *gh.RunnerGroup
	opts := &gh.ListOrgRunnerGroupOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		groups, response, err := client.Actions.ListOrganizationRunnerGroups(ctx, c.Organization(), opts)
		if err != nil {
			return nil, fmt.Errorf("list organization runner groups (PAT requires Administration read and Self-hosted runners write): %w", err)
		}
		for _, group := range groups.RunnerGroups {
			if strings.EqualFold(group.GetName(), c.GitHub.RunnerGroup) {
				if match != nil {
					return nil, errors.New("ambiguous runner group name")
				}
				match = group
			}
		}
		if response.NextPage == 0 {
			break
		}
		opts.Page = response.NextPage
	}
	if match != nil && (match.GetInherited() || match.GetDefault() || match.GetVisibility() != "selected") {
		return nil, errors.New("configured runner group must be a non-default, non-inherited group with selected repository visibility; no settings changed")
	}
	return match, nil
}

// Setup changes only the exact named group. Repository grants are additive;
// unrelated grants and workflow restrictions on an existing group are preserved.
func SetupGroup(ctx context.Context, client *gh.Client, c config.Config) (*gh.RunnerGroup, error) {
	group, err := FindGroup(ctx, client, c)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, name := range c.GitHub.Repositories {
		repo, _, err := client.Repositories.Get(ctx, c.Organization(), name)
		if err != nil {
			return nil, fmt.Errorf("resolve selected repository %s: %w", name, err)
		}
		if !repo.GetPrivate() && !c.GitHub.AllowPublicRepositories {
			return nil, errors.New("selected public repositories require allowPublicRepositories=true")
		}
		ids = append(ids, repo.GetID())
	}
	if group == nil {
		group, _, err = client.Actions.CreateOrganizationRunnerGroup(ctx, c.Organization(), gh.CreateRunnerGroupRequest{Name: gh.Ptr(c.GitHub.RunnerGroup), Visibility: gh.Ptr("selected"), SelectedRepositoryIDs: ids, AllowsPublicRepositories: gh.Ptr(c.GitHub.AllowPublicRepositories)})
		if err != nil {
			return nil, err
		}
	} else if len(c.GitHub.Repositories) > 0 && group.GetAllowsPublicRepositories() != c.GitHub.AllowPublicRepositories {
		group, _, err = client.Actions.UpdateOrganizationRunnerGroup(ctx, c.Organization(), group.GetID(), gh.UpdateRunnerGroupRequest{AllowsPublicRepositories: gh.Ptr(c.GitHub.AllowPublicRepositories)})
		if err != nil {
			return nil, err
		}
	}
	for _, id := range ids {
		if _, err = client.Actions.AddRepositoryAccessRunnerGroup(ctx, c.Organization(), group.GetID(), id); err != nil {
			return nil, err
		}
	}
	return group, nil
}

type RunnerAPI struct{ Client *scaleset.Client }

func (a RunnerAPI) JIT(ctx context.Context, name string, scaleID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return a.Client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, scaleID)
}
func (a RunnerAPI) Find(ctx context.Context, name string) (*scaleset.RunnerReference, error) {
	return a.Client.GetRunnerByName(ctx, name)
}
func (a RunnerAPI) Remove(ctx context.Context, id int) error {
	err := a.Client.RemoveRunner(ctx, int64(id))
	if errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError) {
		return nil
	}
	return err
}
func (a RunnerAPI) Statistics(ctx context.Context, id int) (*scaleset.RunnerScaleSetStatistic, error) {
	ss, err := a.Client.GetRunnerScaleSetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if ss == nil {
		return nil, errors.New("scale set not found while refreshing statistics")
	}
	return ss.Statistics, nil
}
