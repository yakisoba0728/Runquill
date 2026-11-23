package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

func TestKubernetesReportsSafePodStartupCauses(t *testing.T) {
	t.Run("placement", func(t *testing.T) {
		for raw, code := range map[string]string{
			"1 Insufficient cpu: private-node-annotated-data":                        "pod_resources",
			"1 node(s) didn't match Pod's node affinity/selector: tenant-node-a":     "pod_node_constraints",
			"1 Insufficient cpu, 1 node(s) had untolerated taint {internal-node: x}": "pod_unschedulable",
		} {
			pod := core.Pod{Status: core.PodStatus{Phase: core.PodPending, Conditions: []core.PodCondition{{
				Type: core.PodScheduled, Status: core.ConditionFalse, Reason: "Unschedulable", Message: raw,
			}}}}
			detail := kubePodDiagnostic(pod)
			if detail.Code != code || strings.Contains(detail.Message, raw) {
				t.Errorf("diagnostic = %+v, want %q without raw scheduler data", detail, code)
			}
		}
	})
	t.Run("containers", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			state core.ContainerState
			code  string
		}{
			{"prepare-jit", core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ErrImagePull", Message: "secret-marker"}}, "pod_image_pull"},
			{"runner", core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "CreateContainerConfigError"}}, "pod_start_failed"},
			{"prepare-jit", core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1}}, "pod_init_failed"},
			{"runner", core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}, "pod_memory_limit"},
		} {
			status := core.PodStatus{Phase: core.PodPending}
			states := []core.ContainerStatus{{Name: tc.name, State: tc.state}}
			if tc.name == "prepare-jit" {
				status.InitContainerStatuses = states
			} else {
				status.ContainerStatuses = states
			}
			detail := kubePodDiagnostic(core.Pod{Status: status})
			if detail.Code != tc.code || strings.Contains(detail.Message, "secret-marker") {
				t.Errorf("diagnostic = %+v, want %q", detail, tc.code)
			}
		}
	})
}

func TestKubernetesSnapshotReportsOnlyCurrentMountWarnings(t *testing.T) {
	for _, permitEvents := range []bool{true, false} {
		pods := []core.Pod{
			{ObjectMeta: meta.ObjectMeta{Name: "runquill-real", UID: types.UID("current")}},
			{ObjectMeta: meta.ObjectMeta{Name: "runquill-stale", UID: types.UID("stale")}},
			{ObjectMeta: meta.ObjectMeta{Name: "runquill-other", UID: types.UID("other")}},
			{ObjectMeta: meta.ObjectMeta{Name: "runquill-initialized", UID: types.UID("initialized")}},
		}
		for i := range pods {
			pods[i].Spec.NodeName = "fixture-node"
			pods[i].Status.Phase = core.PodPending
		}
		pods[3].Status.Conditions = []core.PodCondition{{Type: core.PodReadyToStartContainers, Status: core.ConditionTrue}}
		warning := func(name, uid string, when time.Time) core.Event {
			return core.Event{InvolvedObject: core.ObjectReference{Kind: "Pod", Name: name, UID: types.UID(uid)},
				Reason: "FailedMount", Type: core.EventTypeWarning,
				LastTimestamp: meta.NewTime(when), Message: "secret-marker"}
		}
		now := time.Now()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/namespaces/runner-fixture/pods":
				if r.URL.Query().Get("labelSelector") != kubeRunnerLabel {
					t.Errorf("unexpected pod selector: %s", r.URL.RawQuery)
				}
				_ = json.NewEncoder(w).Encode(core.PodList{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: pods})
			case "/api/v1/namespaces/runner-fixture/events":
				if r.URL.Query().Get("fieldSelector") != "involvedObject.kind=Pod,type=Warning" || r.URL.Query().Get("limit") != "100" {
					t.Errorf("unexpected event query: %s", r.URL.RawQuery)
				}
				if !permitEvents {
					http.Error(w, "events unsupported", http.StatusForbidden)
					return
				}
				_ = json.NewEncoder(w).Encode(core.EventList{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "EventList"}, Items: []core.Event{
					warning("runquill-real", "current", now),
					warning("runquill-stale", "stale", now.Add(-5*time.Minute)),
					warning("runquill-other", "old-uid", now),
					warning("runquill-initialized", "initialized", now),
				}})
			default:
				http.Error(w, "unexpected call", http.StatusNotFound)
			}
		}))
		t.Cleanup(server.Close)
		api, err := typed.NewForConfig(&rest.Config{Host: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		runtime := &kubeRunners{pods: api.Pods("runner-fixture"), events: api.Events("runner-fixture")}
		hosts, err := runtime.snapshot(t.Context())
		if err != nil || len(hosts) != 1 || hosts[0].Err != nil || len(hosts[0].Containers) != 4 {
			t.Fatalf("snapshot failed with permitEvents=%v: hosts=%+v err=%v", permitEvents, hosts, err)
		}
		expected := map[string]string{
			"runquill-real": "pod_preparing", "runquill-stale": "pod_preparing",
			"runquill-other": "pod_preparing", "runquill-initialized": "pod_initializing",
		}
		if permitEvents {
			expected["runquill-real"] = "pod_mount_failed"
		}
		for name, code := range expected {
			report := hosts[0].RunnerDiagnostics[name]
			if report.Code != code || strings.Contains(report.Message, "secret-marker") {
				t.Errorf("%s: diagnostic = %+v, want %q", name, report, code)
			}
		}
	}
}

// Exited runner Pods report whether Kubernetes killed them for memory; running ones stay unknown.
func TestKubernetesSnapshotReportsMemoryKills(t *testing.T) {
	terminated := func(name string, phase core.PodPhase, code int32, reason string) core.Pod {
		return core.Pod{ObjectMeta: meta.ObjectMeta{Name: name}, Status: core.PodStatus{Phase: phase, ContainerStatuses: []core.ContainerStatus{{
			Name: "runner", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: code, Reason: reason}}}}}}
	}
	pods := []core.Pod{
		terminated("runquill-oom", core.PodFailed, 137, "OOMKilled"),
		terminated("runquill-done", core.PodSucceeded, 0, "Completed"),
		{ObjectMeta: meta.ObjectMeta{Name: "runquill-busy"}, Status: core.PodStatus{Phase: core.PodRunning}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(core.PodList{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: pods})
	}))
	t.Cleanup(server.Close)
	api, err := typed.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &kubeRunners{pods: api.Pods("runner-fixture"), events: api.Events("runner-fixture")}
	hosts, err := runtime.snapshot(t.Context())
	if err != nil || len(hosts) != 1 {
		t.Fatalf("snapshot: %+v, %v", hosts, err)
	}
	if got := hosts[0].OOMKilled; len(got) != 2 || !got["runquill-oom"] || got["runquill-done"] {
		t.Fatalf("OOMKilled = %v", got)
	}
}
