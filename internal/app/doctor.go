package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const doctorTroubleshooting = "https://github.com/yakisoba0728/Runquill/blob/main/docs/operations.md#troubleshooting"

type doctorCheck struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Level string `json:"level,omitempty"`
}

func doctor(ctx context.Context, out io.Writer, repo string, asJSON bool) error {
	var failures []error
	checks := []doctorCheck{}
	jsonOut := out
	if asJSON {
		// JSON mode prints only the final document.
		out = io.Discard
	}
	notice := func(level, name string) {
		fmt.Fprintf(out, "%s %s\n", strings.ToUpper(level), name)
		checks = append(checks, doctorCheck{Name: name, OK: true, Level: level})
	}
	info := func(name string) { notice("info", name) }
	report := func(name string, err error) {
		if errors.Is(err, errInvalidRegistryMirror) {
			return // The dedicated mirror check emits the warning.
		}
		if _, invalid := errors.AsType[invalidWarmPoolError](err); invalid {
			notice("warn", name+": "+err.Error())
			return
		}
		check := doctorCheck{Name: name, OK: err == nil}
		if err != nil {
			check.Error = err.Error()
			failures = append(failures, err)
			fmt.Fprintf(out, "FAIL %s: %s\n", name, err)
		} else {
			fmt.Fprintf(out, "OK   %s\n", name)
		}
		checks = append(checks, check)
	}
	cfg, err := loadConfig()
	report("configuration", err)
	if cfg.allowPublic {
		report("public repositories allowed ("+allowPublicEnv+"); workflow runs from forks are refused", nil)
	}
	mirror, mirrorErr := registryMirror()
	switch {
	case mirrorErr != nil:
		notice("warn", mirrorErr.Error())
	case mirror == "":
		info("registry mirror (RUNQUILL_REGISTRY_MIRROR): unset")
	default:
		info("registry mirror (RUNQUILL_REGISTRY_MIRROR): configured and valid")
	}
	saved, settingsErr := doctorOperationSettings(ctx, cfg)
	if settingsErr != nil {
		report("saved operation settings", settingsErr)
	} else if saved != nil {
		report("saved operation settings", nil)
		for _, change := range saved.adjusted {
			// The manager starts with these changes; saving Settings in the panel records them.
			report("saved operation settings fitted to this deployment: "+change, nil)
		}
		switch fleet := saved.runners.runtime.(type) {
		case *dockerFleet:
			reportDoctorPools(saved.runners.pools, info)
			checkConfiguredDockerFleet(ctx, fleet, report, info)
		case *kubeRunners:
			fleet.check(ctx, report)
		}
	} else if env("RUNQUILL_BACKEND", "docker") == "docker" {
		checkDockerFleet(ctx, cfg.runnerImage, mirror, report, info)
	} else {
		runners, err := configureRunnerRuntime(cfg.runnerImage)
		report("runner backend", err)
		if err == nil {
			runners.runtime.(*kubeRunners).check(ctx, report)
		}
	}
	if policy, policyErr := loadAssetPolicy(os.Getenv); policyErr != nil {
		report("runner image and cache volume cleanup", policyErr)
	} else {
		report("runner image and cache volume cleanup: "+policy.String(), nil)
	}
	checkRunnerImageCatalogue(ctx, saved, cfg.runnerImage, report)
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
	if asJSON {
		enc := json.NewEncoder(jsonOut)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Checks []doctorCheck `json:"checks"`
			Passed bool          `json:"passed"`
		}{checks, len(failures) == 0}); err != nil {
			failures = append(failures, err)
		}
	} else if len(failures) == 0 {
		fmt.Fprintln(out, "Preflight passed. Run your first workflow to verify job discovery, runner assignment, and cleanup.")
	} else {
		fmt.Fprintf(out, "Next: fix the failed checks above. See %s\n", doctorTroubleshooting)
	}
	return errors.Join(failures...)
}

func doctorOperationSettings(ctx context.Context, cfg config) (*operationState, error) {
	path := env("RUNQUILL_DB", "runquill.sqlite3")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	db, err := openQueueDB(path, "ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, saved, err := readOperationPolicy(ctx, db, operationPolicy{}); err != nil || !saved {
		return nil, err
	}
	return appliedRunnerSettings(ctx, db, cfg.runnerImage, cfg.limit)
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

func checkDockerFleet(ctx context.Context, image, mirror string, report func(string, error), info func(string)) {
	fleet, dockerErr := loadDockerFleet(image, env("RUNQUILL_DOCKER_HOSTS", "local"), mirror)
	if dockerErr != nil {
		report("Docker hosts", dockerErr)
		return
	}
	_, limitErr := fleet.limits(os.Getenv("RUNQUILL_DOCKER_HOST_LIMITS"))
	report("Docker host runner limits", limitErr)
	pools, poolErr := fleet.configurePools(os.Getenv("RUNQUILL_WORKER_POOLS"))
	report("Docker worker pools", poolErr)
	if poolErr == nil {
		reportDoctorPools(pools, info)
	}
	fleet.recordPrivilegeRequests(pools)
	checkConfiguredDockerFleet(ctx, fleet, report, info)
}

func reportDoctorPools(pools workerPools, info func(string)) {
	if len(pools) == 0 {
		pools = workerPools{defaultWorkerPool: {}}
	}
	for _, name := range slices.Sorted(maps.Keys(pools)) {
		pool := pools[name]
		info(fmt.Sprintf("worker pool %q: cache=%t, warm=%d per host", name, pool.Cache, pool.Warm))
	}
}

func checkConfiguredDockerFleet(ctx context.Context, fleet *dockerFleet, report func(string, error), info func(string)) {
	for _, target := range fleet.hosts {
		targetReport := report
		if len(fleet.hosts) > 1 || target.host != "" {
			targetReport = func(name string, err error) { report(target.label()+" "+name, err) }
		}
		if target.agentName() != "" {
			targetReport("worker connection and runner image", fleet.workers.snapshot(ctx, target).Err)
			if target.cache {
				info(fmt.Sprintf("worker pool %q cache volumes on %s: inventory unavailable", target.pool, target.label()))
			}
		} else {
			checkDocker(ctx, target, fleet.images, targetReport)
		}
	}
	if len(fleet.hosts) > 1 {
		report("distinct Docker hosts with runner images", fleet.prepare(ctx))
	}
	reportRunnerAssets(ctx, fleet, report, info)
}

func checkDocker(ctx context.Context, target docker, images *imageReadiness, report func(string, error)) {
	host, err := target.probe(ctx)
	if err != nil {
		report("container engine", err)
		return
	}
	mode := "rootful"
	if host.Rootless {
		mode = "rootless"
	}
	summary := fmt.Sprintf("%s %s (%s/%s, cgroup v%s, %s)", host.Engine, host.Version, host.OS, host.Arch, host.Cgroup, mode)
	switch {
	case host.OS != "linux" || host.Cgroup != "2" || (host.Arch != "amd64" && host.Arch != "arm64"):
		report("container engine "+summary, fmt.Errorf("requires Linux %s on amd64 or arm64 with cgroup v2", host.Engine))
	case host.Rootless && target.requirePrivileged && !target.unprivileged:
		report("container engine "+summary, target.rootlessPrivilegedError(host.Engine))
	case host.Rootless && !target.unprivileged:
		report("container engine "+summary+"; runners start unprivileged without Docker-in-Docker", nil)
	default:
		report("container engine "+summary, nil)
	}
	imagePlatform, err := target.command(ctx, nil, "image", "inspect", target.image, "--format", "{{.Os}}/{{.Architecture}}")
	if err == nil && strings.TrimSpace(imagePlatform) != host.OS+"/"+host.Arch {
		err = fmt.Errorf("image platform %s differs from %s platform %s/%s", strings.TrimSpace(imagePlatform), host.Engine, host.OS, host.Arch)
	}
	if err != nil && images.enabled() && imageMissing(err) {
		report(fmt.Sprintf("runner image %s is not on this host yet; the manager pulls it (RUNQUILL_IMAGE_PULL=%s)", target.image, images.policy), nil)
		return
	}
	if err == nil && images.enabled() {
		report(fmt.Sprintf("runner image %s (RUNQUILL_IMAGE_PULL=%s)", target.image, images.policy), nil)
		return
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
	scopeErr, permissionErr := inst.accessErrors(g.repositoryScope, g.runnerGroupID > 0)
	report("GitHub App repository access ("+repositoryScopeLabel(inst.RepositorySelection)+")", scopeErr)
	report("GitHub App permissions", permissionErr)
	var group map[int64]string
	if g.runnerGroupID > 0 && scopeErr == nil && permissionErr == nil {
		group, err = g.inspectRunnerGroup(ctx, jwt, inst.ID)
		report(fmt.Sprintf("GitHub organization runner group %d", g.runnerGroupID), err)
	}
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
		_, err := g.repositoryInstallation(ctx, jwt, name, inst.ID)
		if errors.Is(err, errRepositoryOutsideInstallation) && inst.RepositorySelection == scopeSelected {
			err = fmt.Errorf("%w. Add %s at %s", err, name, g.installationURL(inst.ID))
		}
		report("GitHub App covers "+name, err)
		if err == nil && group != nil {
			groupErr := fmt.Errorf("runner group %d does not allow %s", g.runnerGroupID, name)
			for _, allowed := range group {
				if allowed == name {
					groupErr = nil
					break
				}
			}
			report("GitHub runner group covers "+name, groupErr)
		}
	}
}
