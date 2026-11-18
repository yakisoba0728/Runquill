package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

func doctor(ctx context.Context, out io.Writer, repo string) error {
	var failures []error
	report := func(name string, err error) {
		if err != nil {
			failures = append(failures, err)
			fmt.Fprintf(out, "FAIL %s: %s\n", name, err)
			return
		}
		fmt.Fprintf(out, "OK   %s\n", name)
	}
	cfg, err := loadConfig()
	report("configuration", err)
	fleet, dockerErr := newDockerFleet(cfg.runnerImage, env("RUNQUILL_DOCKER_HOSTS", "local"))
	if dockerErr != nil {
		report("Docker hosts", dockerErr)
	} else {
		_, limitErr := fleet.limits(os.Getenv("RUNQUILL_DOCKER_HOST_LIMITS"))
		report("Docker host runner limits", limitErr)
		for _, target := range fleet.hosts {
			targetReport := report
			if len(fleet.hosts) > 1 || target.host != "" {
				targetReport = func(name string, err error) { report(target.label()+" "+name, err) }
			}
			checkDocker(ctx, target, targetReport)
		}
		if len(fleet.hosts) > 1 {
			report("distinct Docker hosts with runner images", fleet.prepare(ctx))
		}
	}
	if err == nil {
		api, err := cfg.github()
		report("GitHub App private key", err)
		if err == nil {
			api.checkInstallation(ctx, repo, cfg.secret != "", report)
		}
	}
	if len(failures) == 0 {
		fmt.Fprintln(out, "Preflight passed. Run your first workflow to verify job discovery, runner assignment, and cleanup.")
	}
	return errors.Join(failures...)
}

func checkDocker(ctx context.Context, target docker, report func(string, error)) {
	raw, err := target.command(ctx, nil, "info", "--format",
		`{"OS":{{json .OSType}},"Arch":{{json .Architecture}},"Cgroup":{{json .CgroupVersion}},"Security":{{json .SecurityOptions}}}`)
	if err != nil {
		report("Docker Engine", err)
		return
	}
	var host struct {
		OS, Arch, Cgroup string
		Security         []string
	}
	if err := json.Unmarshal([]byte(raw), &host); err != nil {
		report("Docker Engine", fmt.Errorf("read Docker host properties: %w", err))
		return
	}
	arch := host.Arch
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	rootless := slices.ContainsFunc(host.Security, func(option string) bool {
		name, _, _ := strings.Cut(option, ",")
		return name == "name=rootless"
	})
	if host.OS != "linux" || host.Cgroup != "2" || rootless || (arch != "amd64" && arch != "arm64") {
		report("Docker Engine", fmt.Errorf("requires rootful Linux Docker on amd64 or arm64 with cgroup v2; found %s/%s, cgroup %s, rootless=%t",
			host.OS, arch, host.Cgroup, rootless))
	} else {
		report("Docker Engine ("+host.OS+"/"+arch+", cgroup v2)", nil)
	}
	imagePlatform, err := target.command(ctx, nil, "image", "inspect", target.image, "--format", "{{.Os}}/{{.Architecture}}")
	if err == nil && strings.TrimSpace(imagePlatform) != host.OS+"/"+arch {
		err = fmt.Errorf("image platform %s differs from Docker platform %s/%s", strings.TrimSpace(imagePlatform), host.OS, arch)
	}
	report("runner image "+target.image, err)
}

func (g *github) checkInstallation(ctx context.Context, repo string, webhooks bool, report func(string, error)) {
	jwt, err := g.jwt(time.Now())
	if err != nil {
		report("GitHub App JWT", err)
		return
	}
	inst, err := g.installation(ctx, jwt)
	report("GitHub App installation", err)
	if err != nil {
		return
	}
	scopeErr, permissionErr := inst.accessErrors()
	report("GitHub App repository access", scopeErr)
	report("GitHub App permissions", permissionErr)
	if webhooks {
		var eventErr error
		if !slices.Contains(inst.Events, "workflow_job") {
			eventErr = errors.New("subscribe to the Workflow job event")
		}
		report("GitHub App event subscription", eventErr)

		var hook struct {
			URL         string
			ContentType string `json:"content_type"`
			InsecureSSL string `json:"insecure_ssl"`
		}
		err = g.request(ctx, http.MethodGet, "/app/hook/config", jwt, nil, &hook)
		if err == nil {
			endpoint, parseErr := url.Parse(hook.URL)
			if parseErr != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" ||
				hook.ContentType != "json" || hook.InsecureSSL != "0" {
				err = errors.New("configure an HTTPS webhook with JSON content and TLS verification enabled")
			}
		}
		report("GitHub webhook configuration", err)
	}
	if repo != "" {
		name, valid := ownedRepositoryPath(repo, g.owner)
		if !valid {
			report("test repository", errors.New("use OWNER/REPO for a repository owned by RUNQUILL_OWNER"))
			return
		}
		var target struct{ ID int64 }
		err := g.request(ctx, http.MethodGet, "/repos/"+name+"/installation", jwt, nil, &target)
		if err == nil && target.ID != inst.ID {
			err = errors.New("the repository does not belong to the expected GitHub App installation")
		}
		report("GitHub App covers "+name, err)
	}
}
