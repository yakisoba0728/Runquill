package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	k3sWizardMarker     = ".runquill-setup.k3s"
	k3sWizardAnnotation = "runquill.io/setup-id"
)

type k3sWizardDeployment struct {
	Cluster string
	AppID   string
	ID      string
}

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
		return web.applyK3s(ctx, values["RUNQUILL_APP_ID"])
	}
	compose := []string{"compose", "-f", "compose.yaml", "-f", "compose.panel.yaml"}
	for host := range strings.SplitSeq(values["RUNQUILL_DOCKER_HOSTS"], ",") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(host), "ssh://"):
			if !slices.Contains(compose, "compose.ssh.yaml") {
				compose = append(compose, "-f", "compose.ssh.yaml")
			}
		case strings.HasPrefix(strings.TrimSpace(host), "agent://") && values["RUNQUILL_WORKER_AUTH_FILE"] != "":
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

func (web *setupWizard) applyK3s(ctx context.Context, appID string) error {
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
	marker := filepath.Join(web.dir, k3sWizardMarker)
	if err := prepareK3sNamespaces(marker, appID, makeCommand, func(manifest []byte) error {
		return run(bytes.NewReader(manifest), false, "create", "-f", "-")
	}); err != nil {
		return err
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
	if err := run(nil, true, "exec", "deployment/runquill", "--", "runquill", "doctor"); err != nil {
		return err
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("finish the K3s setup: %w", err)
	}
	return nil
}

func prepareK3sNamespaces(marker, appID string, command func(...string) *exec.Cmd, create func([]byte) error) error {
	raw, err := command("get", "namespace", "kube-system", "-o", "jsonpath={.metadata.uid}").Output()
	if err != nil {
		return fmt.Errorf("identify the cluster in the selected K3s context: %w", err)
	}
	cluster := strings.TrimSpace(string(raw))
	if cluster == "" {
		return errors.New("the selected K3s context did not report the kube-system namespace UID")
	}
	saved, err := os.ReadFile(marker)
	fresh := errors.Is(err, os.ErrNotExist)
	var state k3sWizardDeployment
	switch {
	case fresh:
		state = k3sWizardDeployment{Cluster: cluster, AppID: appID, ID: rand.Text()}
	case err != nil:
		return fmt.Errorf("read the K3s setup marker: %w", err)
	case json.Unmarshal(saved, &state) != nil || state.ID == "":
		return errors.New("the K3s setup marker is invalid")
	case state.Cluster != cluster || state.AppID != appID:
		return errors.New("resume K3s setup with the original cluster and GitHub App")
	}

	var missing []string
	for _, name := range []string{"runquill-system", "runquill-runners"} {
		raw, err := command("get", "namespace", name, "--ignore-not-found", "-o", "json").Output()
		if err != nil {
			return fmt.Errorf("check the selected K3s context and namespace permissions: %w", err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			missing = append(missing, name)
			continue
		}
		var ns core.Namespace
		if err := json.Unmarshal(raw, &ns); err != nil {
			return fmt.Errorf("read K3s namespace %s: %w", name, err)
		}
		if fresh || ns.Name != name || ns.Annotations[k3sWizardAnnotation] != state.ID || ns.DeletionTimestamp != nil {
			return fmt.Errorf("the selected context already has a Runquill namespace (%s); use the documented upgrade flow", name)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	resources, err := readK3sWizardNamespaces(filepath.Join(filepath.Dir(marker), "runquill-k3s.yaml"))
	if err != nil {
		return err
	}
	for _, name := range missing {
		if resources[name] == nil {
			return fmt.Errorf("runquill-k3s.yaml is missing namespace %s", name)
		}
	}
	if fresh {
		saved, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if err := writeSetupFile(marker, saved); err != nil {
			return fmt.Errorf("save the K3s setup marker: %w", err)
		}
	}

	// Create each namespace with its ownership marker in the same request. Create
	// refuses a namespace another installation obtained after the read above.
	for _, name := range missing {
		ns := resources[name]
		if ns.Annotations == nil {
			ns.Annotations = map[string]string{}
		}
		ns.Annotations[k3sWizardAnnotation] = state.ID
		raw, err := json.Marshal(ns)
		if err != nil {
			return err
		}
		if err := create(raw); err != nil {
			return fmt.Errorf("create K3s namespace %s: %w", name, err)
		}
	}
	return nil
}

func readK3sWizardNamespaces(path string) (map[string]*core.Namespace, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := kyaml.NewYAMLOrJSONDecoder(file, 4096)
	namespaces := map[string]*core.Namespace{}
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return namespaces, nil
			}
			return nil, fmt.Errorf("read runquill-k3s.yaml: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		var kind metav1.TypeMeta
		if err := json.Unmarshal(raw, &kind); err != nil {
			return nil, fmt.Errorf("read Kubernetes resource kind: %w", err)
		}
		if kind.Kind != "Namespace" {
			continue
		}
		var ns core.Namespace
		if err := json.Unmarshal(raw, &ns); err != nil {
			return nil, fmt.Errorf("read Kubernetes namespace: %w", err)
		}
		namespaces[ns.Name] = &ns
	}
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
