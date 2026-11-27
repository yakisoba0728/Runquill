package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const kubeHost = "kubernetes"

const kubeRunnerLabel = "app.kubernetes.io/managed-by=runquill"

// kubeForceDeleteAfter is how long a Runquill Pod may stay Terminating, for example
// on a NotReady node, before it is deleted without a grace period. Runner Pods have a
// 5-second grace period, so only a Pod whose kubelet is unreachable gets this far.
const kubeForceDeleteAfter = 5 * time.Minute

// errRunnerRemovalPending reports a removal the runtime accepted while the runner
// still exists and keeps its slot. The lease can be released; the snapshot keeps
// counting the runner until it is gone.
var errRunnerRemovalPending = errors.New("runner removal is still in progress")

type kubeRunners struct {
	pods    typed.PodInterface
	events  typed.EventInterface
	secrets typed.SecretInterface
	options kubeRunnerOptions
}

type kubeRunnerOptions struct {
	namespace, image, mode, pullSecret, mirror string
	nodeSelector                               map[string]string
	resources                                  core.ResourceRequirements
}

func readKubeRunnerOptions(image string) (kubeRunnerOptions, error) {
	opts := kubeRunnerOptions{
		namespace: os.Getenv("RUNQUILL_K8S_RUNNER_NAMESPACE"),
		image:     image, mode: env("RUNQUILL_K8S_RUNNER_MODE", "plain"),
		pullSecret: os.Getenv("RUNQUILL_K8S_IMAGE_PULL_SECRET"),
	}
	if len(validation.IsDNS1123Label(opts.namespace)) != 0 {
		return opts, errors.New("set RUNQUILL_K8S_RUNNER_NAMESPACE to a dedicated Kubernetes runner namespace")
	}
	if opts.mode != "plain" && opts.mode != "dind" {
		return opts, errors.New("RUNQUILL_K8S_RUNNER_MODE must be plain or dind")
	}
	var err error
	if opts.mirror, err = registryMirror(); err != nil {
		return opts, err
	}
	if opts.pullSecret != "" && len(validation.IsDNS1123Subdomain(opts.pullSecret)) != 0 {
		return opts, errors.New("RUNQUILL_K8S_IMAGE_PULL_SECRET must be a Kubernetes Secret name")
	}
	if raw := os.Getenv("RUNQUILL_K8S_NODE_SELECTOR"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &opts.nodeSelector); err != nil {
			return opts, errors.New("RUNQUILL_K8S_NODE_SELECTOR must be a JSON object of label names and values")
		}
		for key, value := range opts.nodeSelector {
			if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
				return opts, fmt.Errorf("invalid Kubernetes node selector %q", key)
			}
		}
	}
	limits := make(core.ResourceList)
	for _, setting := range []struct {
		env      string
		resource core.ResourceName
	}{
		{"RUNQUILL_K8S_JOB_CPU", core.ResourceCPU},
		{"RUNQUILL_K8S_JOB_MEMORY", core.ResourceMemory},
	} {
		if raw := os.Getenv(setting.env); raw != "" {
			q, err := resource.ParseQuantity(raw)
			if err != nil || q.Sign() <= 0 {
				return opts, fmt.Errorf("%s must be a positive Kubernetes quantity", setting.env)
			}
			limits[setting.resource] = q
		}
	}
	if len(limits) > 0 {
		opts.resources = core.ResourceRequirements{Limits: limits, Requests: limits.DeepCopy()}
	}
	return opts, nil
}

func newKubeRunners(image string) (*kubeRunners, error) {
	opts, err := readKubeRunnerOptions(image)
	if err != nil {
		return nil, err
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("Kubernetes mode requires an in-cluster ServiceAccount: %w", err)
	}
	// Never send the manager's ServiceAccount credential to a redirect target.
	client, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = 10 * time.Second
	api, err := typed.NewForConfigAndClient(cfg, client)
	if err != nil {
		return nil, err
	}
	return &kubeRunners{
		pods: api.Pods(opts.namespace), events: api.Events(opts.namespace),
		secrets: api.Secrets(opts.namespace), options: opts,
	}, nil
}

func (k *kubeRunners) check(ctx context.Context, report func(string, error)) {
	hosts, err := k.snapshot(ctx)
	if err == nil {
		err = hosts[0].Err
	}
	report("list Kubernetes runner Pods", err)
	opts := meta.CreateOptions{DryRun: []string{meta.DryRunAll}}
	name := "runquill-doctor-" + rand.String(12)
	_, err = k.pods.Create(ctx, k.runnerPod(name), opts)
	report("admit a runner Pod (dry run)", err)
	_, err = k.secrets.Create(ctx, &core.Secret{ObjectMeta: meta.ObjectMeta{Name: name}}, opts)
	report("create a runner credential Secret (dry run)", err)
}

func (k *kubeRunners) snapshot(ctx context.Context) ([]dockerHostSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	host := dockerHostSnapshot{Name: kubeHost}
	list, err := k.pods.List(ctx, meta.ListOptions{LabelSelector: kubeRunnerLabel})
	if err != nil {
		host.Err = fmt.Errorf("list Runquill Pods in %s: %w", k.options.namespace, err)
		return []dockerHostSnapshot{host}, nil
	}
	host.Containers = make(map[string]string, len(list.Items))
	host.RunnerDiagnostics = make(map[string]jobDiagnostic)
	host.OOMKilled = make(map[string]bool)
	mountCandidates := make(map[string]string)
	if ctx.Value(reapStuckKey{}) != nil {
		k.forceDeleteStuck(ctx, list.Items, time.Now())
	}
	for _, pod := range list.Items {
		host.Containers[pod.Name] = kubePodState(pod)
		detail := kubePodDiagnostic(pod)
		if detail.Code != "" {
			host.RunnerDiagnostics[pod.Name] = detail
		}
		if host.Containers[pod.Name] == "exited" {
			host.OOMKilled[pod.Name] = detail.Code == "pod_memory_limit"
		}
		if detail.Code == "pod_preparing" && pod.UID != "" {
			mountCandidates[string(pod.UID)] = pod.Name
		}
	}
	k.addMountDiagnostics(ctx, mountCandidates, host.RunnerDiagnostics)
	return []dockerHostSnapshot{host}, nil
}

func kubePodState(pod core.Pod) string {
	if pod.DeletionTimestamp != nil {
		return "pending"
	}
	switch pod.Status.Phase {
	case core.PodSucceeded, core.PodFailed:
		return "exited"
	case core.PodRunning:
		for _, s := range pod.Status.ContainerStatuses {
			if s.Name == "runner" {
				if s.State.Terminated != nil {
					return "exited"
				}
				if s.State.Running != nil {
					return "running"
				}
			}
		}
	}
	return "pending"
}

func (k *kubeRunners) startOn(ctx context.Context, host, name, jit string) error {
	if host != kubeHost {
		return fmt.Errorf("unknown Kubernetes runner host %q", host)
	}
	spec := k.runnerPod(name)
	if image := runnerImageFrom(ctx); image != "" {
		// A job's catalogue image replaces the configured one; the kubelet pulls it.
		spec.Spec.InitContainers[0].Image, spec.Spec.Containers[0].Image = image, image
	}
	pod, err := k.pods.Create(ctx, spec, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create runner Pod: %w", err)
	}
	credential := &core.Secret{
		ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "runquill"},
			OwnerReferences: []meta.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}},
		Type: core.SecretTypeOpaque,
		Data: map[string][]byte{"config": []byte(jit)},
	}
	if _, err := k.secrets.Create(ctx, credential, meta.CreateOptions{}); err != nil {
		// Kubernetes may include invalid values in error details. Do not surface Secret data.
		var status apierrors.APIStatus
		if !errors.As(err, &status) {
			return errors.New("create runner credential Secret: request outcome unknown")
		}
		secretErr := fmt.Errorf("create runner credential Secret: %s", apierrors.ReasonForError(err))
		code := status.Status().Code
		// A lost response can leave a valid Secret and an active runner for the scheduler to reconcile.
		if code < 400 || code >= 500 || code == http.StatusRequestTimeout || code == http.StatusConflict {
			return secretErr
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := k.deletePod(cleanup, pod); err != nil {
			return errors.Join(secretErr, fmt.Errorf("clean up runner Pod: %w", err))
		}
		return secretErr
	}
	return nil
}

func (k *kubeRunners) runnerPod(name string) *core.Pod {
	no, yes := false, true
	uid := int64(1001)
	mode := int32(0440)
	grace := int64(5)
	plain := &core.SecurityContext{
		RunAsUser: &uid, RunAsGroup: &uid, RunAsNonRoot: &yes, AllowPrivilegeEscalation: &no,
		Capabilities:   &core.Capabilities{Drop: []core.Capability{"ALL"}},
		SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
	}
	runnerSecurity := plain
	if k.options.mode == "dind" {
		root := int64(0)
		runnerSecurity = &core.SecurityContext{RunAsUser: &root, Privileged: &yes}
	}
	initSecurity := *plain
	initSecurity.ReadOnlyRootFilesystem = &yes
	runnerEnv := []core.EnvVar{{Name: "RUNQUILL_RUNNER_MODE", Value: k.options.mode}, {Name: "RUNQUILL_JIT_FILE", Value: "/runquill/config"}}
	if k.options.mode == "dind" && k.options.mirror != "" {
		runnerEnv = append(runnerEnv, core.EnvVar{Name: "RUNQUILL_REGISTRY_MIRROR", Value: k.options.mirror})
	}
	writable := core.VolumeMount{Name: "jit", MountPath: "/runquill"}
	var pullSecrets []core.LocalObjectReference
	if k.options.pullSecret != "" {
		pullSecrets = []core.LocalObjectReference{{Name: k.options.pullSecret}}
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "runquill"}},
		Spec: core.PodSpec{
			AutomountServiceAccountToken: &no, ServiceAccountName: "runquill-job",
			RestartPolicy: core.RestartPolicyNever, TerminationGracePeriodSeconds: &grace, EnableServiceLinks: &no, NodeSelector: k.options.nodeSelector,
			ImagePullSecrets: pullSecrets,
			SecurityContext:  &core.PodSecurityContext{FSGroup: &uid},
			InitContainers: []core.Container{{
				Name: "prepare-jit", Image: k.options.image, SecurityContext: &initSecurity, Resources: k.options.resources,
				Command:      []string{"/bin/sh", "-ec", "umask 077; cp /runquill-secret/config /runquill/config"},
				VolumeMounts: []core.VolumeMount{writable, {Name: "credential", MountPath: "/runquill-secret", ReadOnly: true}},
			}},
			Containers: []core.Container{{
				Name: "runner", Image: k.options.image, SecurityContext: runnerSecurity, Resources: k.options.resources,
				Env:          runnerEnv,
				VolumeMounts: []core.VolumeMount{writable},
			}},
			Volumes: []core.Volume{
				{Name: "credential", VolumeSource: core.VolumeSource{Secret: &core.SecretVolumeSource{SecretName: name, DefaultMode: &mode}}},
				{Name: "jit", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{Medium: core.StorageMediumMemory}}},
			},
		},
	}
}

func kubeOwned(pod *core.Pod) bool {
	return pod.Labels["app.kubernetes.io/managed-by"] == "runquill" && strings.HasPrefix(pod.Name, "runquill-")
}

func (k *kubeRunners) deletePod(ctx context.Context, pod *core.Pod) error {
	if !kubeOwned(pod) {
		return errors.New("refusing to remove a Pod without Runquill ownership")
	}
	err := k.pods.Delete(ctx, pod.Name, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &pod.UID}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (k *kubeRunners) removeOn(ctx context.Context, host, name string) error {
	if host != kubeHost {
		return fmt.Errorf("unknown Kubernetes runner host %q", host)
	}
	pod, err := k.pods.Get(ctx, name, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !kubeOwned(pod) {
		return errors.New("refusing to remove a Pod without Runquill ownership")
	}
	if pod.DeletionTimestamp != nil {
		// A Pod on a NotReady node can stay Terminating indefinitely. Do not wait for it again.
		return fmt.Errorf("Pod %s is terminating: %w", name, errRunnerRemovalPending)
	}
	if err := k.deletePod(ctx, pod); err != nil {
		return err
	}
	// Each check uses the caller's context: near the poll deadline the client rate
	// limiter would refuse to wait and turn a slow removal into a failure.
	err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 8*time.Second, true,
		func(context.Context) (bool, error) {
			current, err := k.pods.Get(ctx, name, meta.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			return current.UID != pod.UID, nil
		})
	if wait.Interrupted(err) && ctx.Err() == nil {
		return fmt.Errorf("Pod %s is still terminating: %w", name, errRunnerRemovalPending)
	}
	return err
}

type reapStuckKey struct{}

// reapingStuckRunners marks the scheduler's snapshot, the only one that force deletes.
// The snapshots of status, doctor and gc --dry-run must change nothing.
func reapingStuckRunners(ctx context.Context) context.Context {
	return context.WithValue(ctx, reapStuckKey{}, true)
}

// stuckPods returns the Runquill Pods that have been Terminating for at least kubeForceDeleteAfter.
// It does not ask who began the deletion: the runner namespace is dedicated to Runquill, a lease
// released while its Pod terminates leaves no record that survives a manager restart, and an
// operator or a node drain that deletes a runner Pod also means it to go.
func stuckPods(pods []core.Pod, now time.Time) []core.Pod {
	var stuck []core.Pod
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil && now.Sub(pod.DeletionTimestamp.Time) >= kubeForceDeleteAfter && kubeOwned(&pod) {
			stuck = append(stuck, pod)
		}
	}
	return stuck
}

// forceDeleteStuck deletes, without a grace period, the Runquill Pods that have been
// Terminating for longer than kubeForceDeleteAfter. Kubernetes then removes each
// runner credential Secret through its owner reference.
func (k *kubeRunners) forceDeleteStuck(ctx context.Context, pods []core.Pod, now time.Time) {
	for _, pod := range stuckPods(pods, now) {
		grace := int64(0)
		err := k.pods.Delete(ctx, pod.Name, meta.DeleteOptions{GracePeriodSeconds: &grace, Preconditions: &meta.Preconditions{UID: &pod.UID}})
		if err != nil && !apierrors.IsNotFound(err) {
			if ctx.Err() == nil {
				slog.Warn("force delete terminating runner Pod", "runner", pod.Name, "error", err)
			}
			continue
		}
		slog.Warn("force deleted runner Pod stuck terminating", "runner", pod.Name, "node", pod.Spec.NodeName,
			"terminating_since", pod.DeletionTimestamp.Time)
	}
}
