package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestKubernetesPendingRunnersReserveCapacity(t *testing.T) {
	for _, pod := range []core.Pod{
		{Status: core.PodStatus{Phase: core.PodPending}},
		{Status: core.PodStatus{Phase: core.PodRunning}},
		{ObjectMeta: meta.ObjectMeta{DeletionTimestamp: &meta.Time{Time: time.Now()}}, Status: core.PodStatus{Phase: core.PodFailed}},
	} {
		if got := kubePodState(pod); got != "pending" || !runnerOccupiesSlot(got) {
			t.Fatalf("phase %s must reserve a slot, got %q", pod.Status.Phase, got)
		}
	}
}

func TestKubernetesRunnerLifecycle(t *testing.T) {
	image := os.Getenv("RUNQUILL_TEST_K8S_IMAGE")
	if image == "" {
		t.Skip("set RUNQUILL_TEST_K8S_IMAGE in a dedicated cluster to use a runner fixture")
	}
	runtime, err := newKubeRunners(image)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	runtime.check(ctx, func(check string, err error) {
		if err != nil {
			t.Fatalf("%s: %v", check, err)
		}
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var generated, deleted atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"fixture-token"}`)
		case "GET /repos/owner/fixture":
			fmt.Fprint(w, `{"id":2,"private":true,"owner":{"login":"owner"}}`)
		case "GET /repos/owner/fixture/actions/jobs/7001", "GET /repos/owner/fixture/actions/jobs/7002":
			fmt.Fprint(w, `{"status":"queued"}`)
		case "POST /repos/owner/fixture/actions/runners/generate-jitconfig":
			fmt.Fprintf(w, `{"runner":{"id":%d},"encoded_jit_config":"integration-fixture"}`, generated.Add(1))
		case "DELETE /repos/owner/fixture/actions/runners/1", "DELETE /repos/owner/fixture/actions/runners/2":
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected fixture API call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	s := &scheduler{queue: q, github: &github{base: api.URL, client: api.Client(), key: key, appID: "123", owner: "owner"}, runtime: runtime, limit: 1}
	now := time.Now()
	for _, id := range []int64{7001, 7002} {
		j := job{id: id, repository: "owner/fixture", repositoryID: 2, installationID: 1, route: fmt.Sprintf("runquill-17-1-ci-%d", id)}
		for range 2 {
			if _, err := q.record(ctx, j, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	for i, id := range []int64{7001, 7002} {
		list, err := runtime.pods.List(ctx, meta.ListOptions{LabelSelector: kubeRunnerLabel})
		if err != nil || len(list.Items) != 1 || !strings.HasPrefix(list.Items[0].Name, fmt.Sprintf("runquill-%d-", id)) {
			t.Fatalf("expected one Pod for FIFO job %d: %v", id, err)
		}
		name := list.Items[0].Name
		if generated.Load() != int32(i+1) {
			t.Fatalf("unexpected number of JIT requests at job %d: %d", id, generated.Load())
		}
		if err := wait.PollUntilContextCancel(ctx, 300*time.Millisecond, true, func(ctx context.Context) (bool, error) {
			pod, err := runtime.pods.Get(ctx, name, meta.GetOptions{})
			if err != nil {
				return false, err
			}
			if pod.Status.Phase == core.PodFailed {
				return false, fmt.Errorf("fixture Pod %s failed", name)
			}
			return pod.Status.Phase == core.PodSucceeded, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := q.advance(ctx, id, completed, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.tick(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	pods, err := runtime.pods.List(ctx, meta.ListOptions{LabelSelector: kubeRunnerLabel})
	if err != nil || len(pods.Items) != 0 || generated.Load() != 2 || deleted.Load() != 2 {
		t.Fatalf("cleanup incomplete: JIT %d, deletions %d, error %v", generated.Load(), deleted.Load(), err)
	}
}
