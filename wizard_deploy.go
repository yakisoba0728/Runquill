package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

func (web *setupWizard) apply(ctx context.Context, checkOnly bool) error {
	data, err := os.ReadFile(filepath.Join(web.dir, setupEnvFile))
	if err != nil {
		return err
	}
	values, err := setupValues(data)
	if err != nil {
		return err
	}
	web.progress.reset(values)
	if web.options.kubernetes {
		return web.applyK3s(ctx)
	}
	compose := []string{"compose", "-f", "compose.yaml", "-f", "compose.panel.yaml"}
	for host := range strings.SplitSeq(values["RUNQUILL_DOCKER_HOSTS"], ",") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(host), "ssh://"):
			if !slices.Contains(compose, "compose.ssh.yaml") {
				compose = append(compose, "-f", "compose.ssh.yaml")
			}
		case strings.HasPrefix(strings.TrimSpace(host), "agent://"):
			if !slices.Contains(compose, "compose.worker.yaml") {
				compose = append(compose, "-f", "compose.worker.yaml")
			}
		}
	}
	docker := func(args ...string) error {
		if err := web.deployCommand(ctx, nil, "docker", args...); err != nil {
			return fmt.Errorf("Docker Compose deployment: %w", err)
		}
		return nil
	}
	if !checkOnly {
		up := []string{"up", "-d", "--build"}
		if setupImage.MatchString(values["RUNQUILL_RUNNER_IMAGE"]) && setupImage.MatchString(values["RUNQUILL_MANAGER_IMAGE"]) {
			up = []string{"up", "-d", "--no-build", "--pull", "missing"}
		} else {
			web.progress.mark("images")
			if err := docker("build", "-t", "runquill-runner:local", "runner"); err != nil {
				return err
			}
		}
		web.progress.mark("manager")
		if err := docker(append(compose, up...)...); err != nil {
			return err
		}
		if web.network.serve || web.network.funnel {
			web.progress.mark("network")
			serveFailed, funnelFailed := web.network.activate(ctx, &web.progress)
			web.mu.Lock()
			web.serveFailed, web.funnelFailed = serveFailed, funnelFailed
			web.mu.Unlock()
		}
	}
	web.progress.mark("check")
	return docker(append(compose, "exec", "-T", "manager", "runquill", "doctor")...)
}

func (web *setupWizard) applyK3s(ctx context.Context) error {
	web.progress.mark("resources")
	base := []string{"--context", web.kubeContext}
	makeCommand := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "kubectl", append(base, args...)...)
		cmd.Dir = web.dir
		return cmd
	}
	run := func(input io.Reader, namespace bool, args ...string) error {
		if namespace {
			args = append([]string{"-n", "runquill-system"}, args...)
		}
		return web.deployCommand(ctx, input, "kubectl", append(base, args...)...)
	}
	if !web.kubeAccepted {
		exists, err := makeCommand("get", "namespace", "runquill-system", "runquill-runners", "--ignore-not-found", "-o", "name").Output()
		if err != nil {
			return fmt.Errorf("check the selected K3s context and namespace permissions: %w", err)
		}
		if strings.TrimSpace(string(exists)) != "" {
			return fmt.Errorf("the selected context already has a Runquill namespace; use the documented upgrade flow")
		}
		web.kubeAccepted = true
	}
	if err := run(nil, false, "apply", "-f", "runquill-k3s.yaml"); err != nil {
		return err
	}
	for _, secret := range []struct{ name, source string }{
		{"runquill-config", "--from-env-file=.env"}, {"runquill-github-key", "--from-file=github-app.pem"},
	} {
		cmd := makeCommand("-n", "runquill-system", "create", "secret", "generic", secret.name, secret.source, "--dry-run=client", "-o", "json")
		cmd.Stderr = web.log
		manifest, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("prepare %s Secret: %w", secret.name, err)
		}
		if err := run(bytes.NewReader(manifest), true, "apply", "--server-side", "--field-manager=runquill-wizard", "-f", "-"); err != nil {
			return err
		}
	}
	web.progress.mark("manager")
	if err := run(nil, true, "rollout", "status", "deployment/runquill", "--timeout=150s"); err != nil {
		return err
	}
	web.progress.mark("check")
	return run(nil, true, "exec", "deployment/runquill", "--", "runquill", "doctor")
}

func (web *setupWizard) deployCommand(ctx context.Context, input io.Reader, binary string, args ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Stdin = web.dir, input
	fmt.Fprintf(&web.progress, "$ %s %s\n", binary, strings.Join(args, " "))
	output := web.log
	// An admission error can include part of a Kubernetes Secret sent on stdin.
	if input == nil {
		output = io.MultiWriter(web.log, &web.progress)
	}
	cmd.Stdout, cmd.Stderr = output, output
	defer fmt.Fprintln(&web.progress)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s command failed: %w", binary, err)
	}
	return nil
}
