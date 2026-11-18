package main

import (
	"context"
	"slices"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func kubePodDiagnostic(pod core.Pod) jobDiagnostic {
	if pod.DeletionTimestamp != nil {
		return jobDiagnostic{"pod_terminating", "Kubernetes is removing the previous runner Pod."}
	}
	if pod.Status.Reason == "Evicted" && pod.Status.Phase == core.PodFailed {
		return jobDiagnostic{"pod_evicted", "Kubernetes evicted the runner Pod. Check node pressure and Pod events."}
	}
	if pod.Status.Phase == core.PodSucceeded {
		return jobDiagnostic{}
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == core.PodScheduled && condition.Status == core.ConditionFalse && condition.Reason == "Unschedulable" {
			return kubeSchedulingDiagnostic(condition.Message)
		}
	}

	for _, containers := range [][]core.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, container := range containers {
			if container.Name != "prepare-jit" && container.Name != "runner" {
				continue
			}
			if stopped := container.State.Terminated; stopped != nil && stopped.ExitCode != 0 {
				if stopped.Reason == "OOMKilled" {
					return jobDiagnostic{"pod_memory_limit", "Kubernetes reported an out-of-memory kill. Check the job memory limit and node capacity."}
				}
				if container.Name == "prepare-jit" {
					return jobDiagnostic{"pod_init_failed", "The runner initialization container exited with an error. Check the Pod events and init container logs."}
				}
				return jobDiagnostic{"pod_runner_exited", "The Kubernetes runner container exited with an error. Check the runner Pod logs."}
			}
			if waiting := container.State.Waiting; waiting != nil {
				switch waiting.Reason {
				case "ErrImagePull", "ImagePullBackOff", "ErrImageNeverPull", "InvalidImageName":
					return jobDiagnostic{"pod_image_pull", "Kubernetes could not pull the runner image. Check the image reference and registry pull credentials."}
				case "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "CrashLoopBackOff":
					return jobDiagnostic{"pod_start_failed", "Kubernetes could not start the runner container. Check the Pod events and container configuration."}
				}
			}
		}
	}
	switch pod.Status.Phase {
	case core.PodFailed:
		return jobDiagnostic{"pod_failed", "Kubernetes reported a failed runner Pod. Check the Pod events."}
	case core.PodPending:
		if pod.Spec.NodeName == "" {
			return jobDiagnostic{"pod_scheduling", "Waiting for Kubernetes to assign a node to the runner Pod."}
		}
		if slices.ContainsFunc(pod.Status.Conditions, func(c core.PodCondition) bool {
			return c.Type == core.PodReadyToStartContainers && c.Status == core.ConditionTrue
		}) {
			return jobDiagnostic{"pod_initializing", "Kubernetes is initializing the runner containers."}
		}
		return jobDiagnostic{"pod_preparing", "Kubernetes is preparing the runner Pod and its volumes."}
	}
	return jobDiagnostic{}
}

func kubeSchedulingDiagnostic(message string) jobDiagnostic {
	message = strings.ToLower(message)
	resources := strings.Contains(message, "insufficient ") || strings.Contains(message, "too many pods")
	placement := strings.Contains(message, "node affinity/selector") || strings.Contains(message, "untolerated taint")
	switch {
	case resources && !placement:
		return jobDiagnostic{"pod_resources", "Kubernetes reported insufficient node resources. Check runner resource requests and available node capacity."}
	case placement && !resources:
		return jobDiagnostic{"pod_node_constraints", "Kubernetes reported a node placement constraint. Check the runner node selector and node taints."}
	default:
		return jobDiagnostic{"pod_unschedulable", "Kubernetes could not schedule the runner Pod. Check Pod events for resource or node constraints."}
	}
}

// Event messages can contain paths and plugin output. Only classify structured,
// recent events attached to the pending Pod's UID.
func (k *kubeRunners) addMountDiagnostics(ctx context.Context, candidates map[string]string, diagnostics map[string]jobDiagnostic) {
	if len(candidates) == 0 || k.events == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	events, err := k.events.List(ctx, meta.ListOptions{FieldSelector: "involvedObject.kind=Pod,type=Warning", Limit: 100})
	if err != nil {
		return
	}
	since := time.Now().Add(-2 * time.Minute)
	for _, event := range events.Items {
		name, ok := candidates[string(event.InvolvedObject.UID)]
		if !ok || event.InvolvedObject.Name != name || event.InvolvedObject.Kind != "Pod" || event.Type != core.EventTypeWarning {
			continue
		}
		switch event.Reason {
		case "FailedMount", "FailedAttachVolume":
			seen := event.LastTimestamp.Time
			if event.Series != nil && event.Series.LastObservedTime.Time.After(seen) {
				seen = event.Series.LastObservedTime.Time
			}
			if seen.IsZero() {
				seen = event.EventTime.Time
			}
			if seen.IsZero() {
				seen = event.CreationTimestamp.Time
			}
			if seen.After(since) {
				diagnostics[name] = jobDiagnostic{"pod_mount_failed", "Kubernetes reported a volume mount or attachment failure. Check the runner Pod events and required Secrets."}
			}
		}
	}
}
