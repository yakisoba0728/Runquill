package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	ownerTypeEnv                 = "RUNQUILL_OWNER_TYPE"
	runnerGroupEnv               = "RUNQUILL_RUNNER_GROUP_ID"
	organizationOwnerType        = "organization"
	organizationRunnerPermission = "organization_self_hosted_runners"
)

func parseGitHubTarget(ownerType, group string) (bool, int64, error) {
	organization := ownerType == organizationOwnerType
	if ownerType != "" && ownerType != "user" && !organization {
		return false, 0, fmt.Errorf("set %s to user or organization", ownerTypeEnv)
	}
	if group == "" {
		return organization, 0, nil
	}
	id, err := strconv.ParseInt(group, 10, 64)
	if err != nil || id <= 0 {
		return false, 0, fmt.Errorf("%s must be a positive integer", runnerGroupEnv)
	}
	if !organization {
		return false, 0, fmt.Errorf("%s requires %s=organization", runnerGroupEnv, ownerTypeEnv)
	}
	return true, id, nil
}

func (g *github) installationURL(id int64) string {
	path := "/settings/installations/"
	if g.organization {
		path = "/organizations/" + url.PathEscape(g.owner) + path
	}
	return "https://github.com" + path + strconv.FormatInt(id, 10)
}

func (g *github) runnersPath(j job) string {
	if g.runnerGroupID > 0 {
		return "/orgs/" + url.PathEscape(g.owner) + "/actions/runners"
	}
	return "/repos/" + j.repository + "/actions/runners"
}

func (g *github) organizationJobToken(ctx context.Context, j job, provision bool) (string, error) {
	jwt, err := g.jwt(time.Now())
	if err != nil {
		return "", err
	}
	inst, err := g.installation(ctx, jwt)
	if err != nil {
		return "", err
	}
	if inst.ID != j.installationID {
		return "", errRepositoryOutsideInstallation
	}
	if provision {
		if err := inst.checkRepositoryScope(g.repositoryScope); err != nil {
			return "", err
		}
	}
	token, err := g.mintCached(ctx, jwt, inst.ID, map[string]any{
		"permissions": map[string]string{organizationRunnerPermission: "write"},
	})
	if err != nil {
		return "", err
	}
	if provision {
		repositories, err := g.runnerGroupRepositories(ctx, token)
		if err != nil {
			return "", err
		}
		if err := g.checkRunnerGroupRepository(repositories, j.repositoryID, j.repository); err != nil {
			return "", err
		}
	}
	return token.Token, nil
}

func (g *github) inspectRunnerGroup(ctx context.Context, jwt string, installationID int64) (map[int64]string, error) {
	token, err := g.mintToken(ctx, jwt, installationID, map[string]any{
		"permissions": map[string]string{organizationRunnerPermission: "read"},
	})
	if err != nil {
		return nil, err
	}
	return g.runnerGroupRepositories(ctx, token)
}

func (g *github) checkRunnerGroupRepository(repositories map[int64]string, id int64, name string) error {
	if repositories[id] != name {
		return fmt.Errorf("runner group %d does not allow %s", g.runnerGroupID, name)
	}
	return nil
}

func (g *github) runnerGroupRepositories(ctx context.Context, token installationToken) (map[int64]string, error) {
	path := fmt.Sprintf("/orgs/%s/actions/runner-groups/%d", url.PathEscape(g.owner), g.runnerGroupID)
	var group struct {
		ID                       int64
		Visibility               string
		AllowsPublicRepositories *bool `json:"allows_public_repositories"`
		Inherited                bool
		RestrictedToWorkflows    bool `json:"restricted_to_workflows"`
	}
	if err := g.request(ctx, http.MethodGet, path, token.Token, nil, &group); err != nil {
		return nil, fmt.Errorf("read organization runner group %d: %w", g.runnerGroupID, err)
	}
	allowsPublic := group.AllowsPublicRepositories != nil && *group.AllowsPublicRepositories
	if group.ID != g.runnerGroupID || group.Visibility != "selected" || group.Inherited ||
		!g.allowPublic && (group.AllowsPublicRepositories == nil || allowsPublic) {
		if g.allowPublic {
			return nil, fmt.Errorf("runner group %d must belong to the organization and use selected repositories", g.runnerGroupID)
		}
		return nil, fmt.Errorf("runner group %d must belong to the organization, use selected repositories, and disallow public repositories", g.runnerGroupID)
	}
	if group.RestrictedToWorkflows {
		return nil, fmt.Errorf("runner group %d restricts workflows; choose a dedicated group without workflow restrictions", g.runnerGroupID)
	}
	pages := &poller{github: g, token: token, cache: make(map[string]pollPage)}
	members, err := polledPages[repository](ctx, pages, path+"/repositories?per_page=100", "repositories")
	if err != nil {
		return nil, fmt.Errorf("list runner group %d repositories: %w", g.runnerGroupID, err)
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("add a private repository to runner group %d", g.runnerGroupID)
	}
	repositories := make(map[int64]string, len(members))
	for _, member := range members {
		name, valid := ownedRepositoryPath(member.FullName, g.owner)
		public := g.allowPublic && member.Visibility == "public" && member.publicFor(g.owner)
		if member.ID <= 0 || !valid || !public && (!member.privateFor(g.owner) || member.Visibility != "private") {
			if g.allowPublic {
				return nil, fmt.Errorf("runner group %d must contain only private or public repositories owned by %s", g.runnerGroupID, g.owner)
			}
			return nil, fmt.Errorf("runner group %d must contain only private repositories owned by %s", g.runnerGroupID, g.owner)
		}
		if public && !allowsPublic {
			// GitHub never assigns a public repository's job to a group that disallows public repositories.
			return nil, fmt.Errorf("runner group %d disallows public repositories but contains the public repository %s; allow public repositories in the group or remove it", g.runnerGroupID, name)
		}
		repositories[member.ID] = name
	}
	if g.repositoryScope == scopeSelected {
		installed, err := polledPages[repository](ctx, pages, "/installation/repositories?per_page=100", "repositories")
		if err != nil {
			return nil, fmt.Errorf("verify runner group %d repository permissions: %w", g.runnerGroupID, err)
		}
		approved := make(map[int64]string, len(installed))
		for _, item := range installed {
			name, valid := ownedRepositoryPath(item.FullName, g.owner)
			if valid && item.acceptedFor(g.owner, g.allowPublic) {
				approved[item.ID] = name
			}
		}
		for id, name := range repositories {
			if approved[id] != name {
				return nil, errors.New("runner group includes repositories outside the selected GitHub App installation")
			}
		}
	}
	return repositories, nil
}
