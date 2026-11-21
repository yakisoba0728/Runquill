package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// stuckPodAPI is a Kubernetes API whose Pods never finish terminating, like Pods on a NotReady node.
type stuckPodAPI struct {
	mu     sync.Mutex
	pods   map[string]*core.Pod
	forced []string
}

func newStuckPodAPI(t *testing.T, pods ...core.Pod) (*stuckPodAPI, *kubeRunners) {
	t.Helper()
	fake := &stuckPodAPI{pods: make(map[string]*core.Pod)}
	for i := range pods {
		fake.pods[pods[i].Name] = &pods[i]
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	api, err := typed.NewForConfig(&rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	return fake, &kubeRunners{pods: api.Pods("runner-fixture"), events: api.Events("runner-fixture"), options: kubeRunnerOptions{namespace: "runner-fixture"}}
}

func (f *stuckPodAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	const prefix = "/api/v1/namespaces/runner-fixture/"
	path := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case r.Method == http.MethodGet && path == "pods":
		list := core.PodList{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "PodList"}}
		for _, pod := range f.pods {
			list.Items = append(list.Items, *pod)
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodGet && path == "events":
		_ = json.NewEncoder(w).Encode(core.EventList{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "EventList"}})
	case strings.HasPrefix(path, "pods/"):
		pod := f.pods[strings.TrimPrefix(path, "pods/")]
		if pod == nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
			return
		}
		if r.Method == http.MethodDelete {
			var options meta.DeleteOptions
			if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
				http.Error(w, "unreadable delete options", http.StatusBadRequest)
				return
			}
			if options.GracePeriodSeconds != nil && *options.GracePeriodSeconds == 0 {
				f.forced = append(f.forced, pod.Name)
				delete(f.pods, pod.Name)
			} else if pod.DeletionTimestamp == nil {
				pod.DeletionTimestamp = &meta.Time{Time: time.Now()}
			}
		}
		_ = json.NewEncoder(w).Encode(pod)
	default:
		http.Error(w, "unexpected call", http.StatusNotFound)
	}
}

func (f *stuckPodAPI) terminatingSince(name string, since time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[name].DeletionTimestamp = &meta.Time{Time: since}
}

func (f *stuckPodAPI) forcedDeletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forced...)
}

func stuckRunnerPod(name string, terminating *time.Time) core.Pod {
	pod := core.Pod{
		TypeMeta:   meta.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: meta.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: map[string]string{"app.kubernetes.io/managed-by": "runquill"}},
		Spec:       core.PodSpec{NodeName: "not-ready"},
		Status:     core.PodStatus{Phase: core.PodRunning},
	}
	if terminating != nil {
		pod.DeletionTimestamp = &meta.Time{Time: *terminating}
	}
	return pod
}

func TestKubernetesTerminatingPodReleasesLeaseButKeepsSlot(t *testing.T) {
	const runner = "runquill-9-0123456789ab"
	var deleted atomic.Int32
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo/actions/jobs/9":
			fmt.Fprint(w, `{"status":"queued"}`)
		case "DELETE /repos/owner/repo/actions/runners/55":
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			// A launch here would place a second runner in the slot the terminating Pod still holds.
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	since := time.Now().Add(-time.Minute)
	fake, runtime := newStuckPodAPI(t, stuckRunnerPod(runner, &since))
	q := testQueue(t)
	now := time.Now()
	recordTestJob(t, q, 9, "runquill-1-1-ci", now.Add(-10*time.Minute))
	recordTestJob(t, q, 10, "runquill-1-1-other", now.Add(-10*time.Minute))
	if _, err := q.ExecContext(t.Context(), `UPDATE jobs SET runner_name = ?, runner_host = ?, runner_id = 55, launched_at = ?
        WHERE job_id = 9`, runner, kubeHost, now.Add(-10*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 1}

	started := time.Now()
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("a terminating Pod delayed the tick by %s", elapsed)
	}
	if j := readJob(t, q, 9); j.runnerName != "" || j.runnerHost != "" || j.phase != queued || deleted.Load() != 1 {
		t.Fatalf("terminating Pod must release the lease and GitHub registration: %+v, deletes=%d", j, deleted.Load())
	}
	if j := readJob(t, q, 10); j.runnerName != "" {
		t.Fatalf("job launched into the slot the terminating Pod holds: %+v", j)
	}

	// Both jobs are due now, but the untracked Pod still fills the only slot.
	later := now.Add(retryDelay + time.Second)
	if err := s.tick(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{9, 10} {
		if j := readJob(t, q, id); j.runnerName != "" {
			t.Fatalf("job %d launched while the terminating Pod holds the slot: %+v", id, j)
		}
	}
	states, err := readHostStates(t.Context(), q.DB)
	if err != nil || states[kubeHost].untrackedSlots.Int64 != 1 {
		t.Fatalf("terminating Pod must count as an untracked slot: %+v, %v", states, err)
	}
	if forced := fake.forcedDeletes(); len(forced) != 0 {
		t.Fatalf("Pod force deleted before %s: %v", kubeForceDeleteAfter, forced)
	}

	fake.terminatingSince(runner, time.Now().Add(-kubeForceDeleteAfter-time.Second))
	hosts, err := runtime.snapshot(t.Context())
	if err != nil || hosts[0].Err != nil {
		t.Fatalf("snapshot: %+v, %v", hosts, err)
	}
	if forced := fake.forcedDeletes(); len(forced) != 1 || forced[0] != runner {
		t.Fatalf("Pod stuck terminating was not force deleted: %v", forced)
	}
	if hosts[0].Containers[runner] != "pending" {
		t.Fatalf("the force deleted Pod keeps its slot until a snapshot no longer lists it: %+v", hosts[0].Containers)
	}
	if hosts, err = runtime.snapshot(t.Context()); err != nil || len(hosts[0].Containers) != 0 {
		t.Fatalf("force deleted Pod still listed: %+v, %v", hosts, err)
	}
}

func TestKubernetesForceDeletesStuckPodsAfterRestart(t *testing.T) {
	old, recent := time.Now().Add(-kubeForceDeleteAfter-time.Second), time.Now().Add(-time.Minute)
	foreign := stuckRunnerPod("builder-0123456789ab", &old)
	unlabelled := stuckRunnerPod("runquill-4-0123456789ab", &old)
	unlabelled.Labels = nil
	// A fresh runtime has not removed any of these Pods, as after a manager restart.
	fake, runtime := newStuckPodAPI(t, stuckRunnerPod("runquill-1-0123456789ab", &old), stuckRunnerPod("runquill-2-0123456789ab", &recent),
		stuckRunnerPod("runquill-3-0123456789ab", nil), foreign, unlabelled)
	if _, err := runtime.snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if forced := fake.forcedDeletes(); len(forced) != 1 || forced[0] != "runquill-1-0123456789ab" {
		t.Fatalf("force deleted %v, want only the Runquill Pod Terminating for %s", forced, kubeForceDeleteAfter)
	}
}

func TestKubernetesRemoveReportsPodThatOutlivesItsGracePeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the removal timeout")
	}
	const runner = "runquill-2-0123456789ab"
	fake, runtime := newStuckPodAPI(t, stuckRunnerPod(runner, nil))
	err := runtime.removeOn(t.Context(), kubeHost, runner)
	if !errors.Is(err, errRunnerRemovalPending) {
		t.Fatalf("removeOn = %v, want a pending removal", err)
	}
	fake.terminatingSince(runner, time.Now().Add(-kubeForceDeleteAfter))
	if _, err := runtime.snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if forced := fake.forcedDeletes(); len(forced) != 1 {
		t.Fatalf("removed Pod stuck terminating was not force deleted: %v", forced)
	}
	if err := runtime.removeOn(t.Context(), kubeHost, runner); err != nil {
		t.Fatalf("removing a deleted Pod: %v", err)
	}
}
