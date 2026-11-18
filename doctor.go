package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
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
	if env("RUNQUILL_BACKEND", "docker") == "docker" {
		checkDockerFleet(ctx, cfg.runnerImage, report)
	} else {
		runners, err := configureRunnerRuntime(cfg.runnerImage)
		report("runner backend", err)
		if err == nil {
			runners.runtime.(*kubeRunners).check(ctx, report)
		}
	}
	if err == nil {
		api, err := cfg.github()
		report("GitHub App private key", err)
		if err == nil {
			api.checkInstallation(ctx, repo, cfg.secret != "", report)
		}
	}
	if addr := os.Getenv("RUNQUILL_PANEL_ADDR"); addr != "" {
		cfg, err := loadPanelOAuthConfig()
		report("panel OAuth configuration", err)
		if err == nil {
			fmt.Fprintf(out, "Expected GitHub OAuth authorization callback URL: %s\n", cfg.callbackURL())
			report("panel sign-in listener", checkPanelListener(ctx, addr, cfg))
			fmt.Fprintf(out, "Open %s/ and sign in to verify the GitHub callback registration and OAuth credentials.\n", cfg.origin)
		}
	}
	if len(failures) == 0 {
		fmt.Fprintln(out, "Preflight passed. Run your first workflow to verify job discovery, runner assignment, and cleanup.")
	}
	return errors.Join(failures...)
}

func checkPanelListener(ctx context.Context, addr string, cfg panelOAuthConfig) error {
	host, port, err := net.SplitHostPort(addr)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 {
		return errors.New("set RUNQUILL_PANEL_ADDR to a host and fixed port, such as 127.0.0.1:8081")
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	endpoint := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/api/me"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return errors.New("could not build the panel probe; review RUNQUILL_PANEL_ADDR")
	}
	// Probe the local listener without creating an OAuth state or sending credentials.
	req.Host = cfg.host
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("start the panel listener at %s and rerun doctor: %w", addr, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("expected the panel at %s to require sign-in (HTTP 401), got HTTP %d; check the listener and RUNQUILL_PANEL_URL", addr, res.StatusCode)
	}
	return nil
}

func checkDockerFleet(ctx context.Context, image string, report func(string, error)) {
	fleet, dockerErr := newDockerFleet(image, env("RUNQUILL_DOCKER_HOSTS", "local"))
	if dockerErr != nil {
		report("Docker hosts", dockerErr)
	} else {
		_, limitErr := fleet.limits(os.Getenv("RUNQUILL_DOCKER_HOST_LIMITS"))
		report("Docker host runner limits", limitErr)
		_, poolErr := fleet.configurePools(os.Getenv("RUNQUILL_WORKER_POOLS"))
		report("Docker worker pools", poolErr)
		for _, target := range fleet.hosts {
			targetReport := report
			if len(fleet.hosts) > 1 || target.host != "" {
				targetReport = func(name string, err error) { report(target.label()+" "+name, err) }
			}
			checkDocker(ctx, target, targetReport)
		}
		if len(fleet.hosts) > 1 && poolErr == nil {
			report("distinct Docker hosts with runner images", fleet.prepare(ctx))
		}
	}
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
