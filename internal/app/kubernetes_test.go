package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
)

type failingRunnerSecrets struct {
	typed.SecretInterface
	cancel context.CancelFunc
	err    error
}

func (s failingRunnerSecrets) Create(context.Context, *core.Secret, meta.CreateOptions) (*core.Secret, error) {
	s.cancel()
	return nil, s.err
}

type rollbackRunnerPods struct {
	typed.PodInterface
	deleteErr error
	deleted   bool
}

const rollbackRunnerUID types.UID = "fixture-pod-uid"

func (p *rollbackRunnerPods) Create(_ context.Context, pod *core.Pod, _ meta.CreateOptions) (*core.Pod, error) {
	pod.UID = rollbackRunnerUID
	return pod, nil
}

func (p *rollbackRunnerPods) Delete(ctx context.Context, name string, opts meta.DeleteOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok || name != "runquill-1-test" || opts.Preconditions == nil ||
		opts.Preconditions.UID == nil || *opts.Preconditions.UID != rollbackRunnerUID {
		return errors.New("expected bounded cleanup of the created Pod UID")
	}
	p.deleted = true
	return p.deleteErr
}

func TestKubernetesRollsBackPodWhenCredentialCreationIsCanceled(t *testing.T) {
	for name, deleteErr := range map[string]error{"removed": nil, "delete failure": errors.New("delete failed")} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pods := &rollbackRunnerPods{deleteErr: deleteErr}
			k := &kubeRunners{pods: pods, secrets: failingRunnerSecrets{cancel: cancel, err: apierrors.NewBadRequest("fixture-credential")}}
			err := k.startOn(ctx, kubeHost, "runquill-1-test", "fixture-credential")
			if !pods.deleted || err == nil || !strings.Contains(err.Error(), "BadRequest") ||
				strings.Contains(err.Error(), "fixture-credential") || (deleteErr != nil && !errors.Is(err, deleteErr)) {
				t.Fatalf("cleanup attempted=%t, error=%v", pods.deleted, err)
			}
		})
	}
}

func TestKubernetesLostCredentialReplyKeepsPod(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pods := &rollbackRunnerPods{}
	k := &kubeRunners{pods: pods, secrets: failingRunnerSecrets{cancel: cancel, err: context.Canceled}}
	err := k.startOn(ctx, kubeHost, "runquill-1-test", "fixture-credential")
	if pods.deleted || err == nil {
		t.Fatalf("ambiguous credential creation: deleted=%t, error=%v", pods.deleted, err)
	}
}

func TestKubernetesCredentialInitContainerIsConstrained(t *testing.T) {
	t.Setenv("RUNQUILL_K8S_RUNNER_NAMESPACE", "runner-fixture")
	t.Setenv("RUNQUILL_K8S_JOB_CPU", "2")
	t.Setenv("RUNQUILL_K8S_JOB_MEMORY", "4Gi")
	for _, mode := range []string{"plain", "dind"} {
		t.Setenv("RUNQUILL_K8S_RUNNER_MODE", mode)
		opts, err := readKubeRunnerOptions("runner:fixture")
		if err != nil {
			t.Fatal(err)
		}
		pod := (&kubeRunners{options: opts}).runnerPod("runquill-1-test")
		init, runner := pod.Spec.InitContainers[0], pod.Spec.Containers[0]
		if !reflect.DeepEqual(init.Resources, opts.resources) || !reflect.DeepEqual(runner.Resources, opts.resources) {
			t.Fatalf("%s: both containers must use configured resources, got init=%v runner=%v", mode, init.Resources, runner.Resources)
		}
		if init.SecurityContext.ReadOnlyRootFilesystem == nil || !*init.SecurityContext.ReadOnlyRootFilesystem ||
			runner.SecurityContext.ReadOnlyRootFilesystem != nil {
			t.Fatalf("%s: expected read-only root only for the credential init container", mode)
		}
	}
}

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
	if err := migrateUsage(ctx, q.DB); err != nil {
		t.Fatal(err)
	}
	panel := newPanel(config{})
	access := panelAccessForTest(t, q)
	handler := panelHandler(panel, access, true)
	admin, err := access.startSession(ctx, panelIdentity{GitHubID: 101, Login: "fixture-admin"})
	if err != nil {
		t.Fatal(err)
	}
	s := &scheduler{queue: q, github: &github{base: api.URL, client: api.Client(), key: key, appID: "123", owner: "owner"}, runtime: runtime, limit: 1, panel: panel}
	tick := func() {
		t.Helper()
		if err := s.tick(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	cordon := func(paused bool) {
		t.Helper()
		body := fmt.Sprintf(`{"host":%q,"cordoned":%t}`, kubeHost, paused)
		req := httptest.NewRequest(http.MethodPost, access.cfg.origin+"/api/hosts/cordon", strings.NewReader(body))
		req.Header.Set("Origin", access.cfg.origin)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: access.cookieName("session"), Value: admin})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("set host pause to %t: HTTP %d: %s", paused, rec.Code, rec.Body)
		}
	}
	assertPaused := func(wantJIT int32, waiting ...int64) {
		t.Helper()
		pods, err := runtime.pods.List(ctx, meta.ListOptions{LabelSelector: kubeRunnerLabel})
		if err != nil || len(pods.Items) != 0 || generated.Load() != wantJIT {
			t.Fatalf("unexpected provisioning while paused: JIT %d, Pod list error %v", generated.Load(), err)
		}
		snapshot := panel.snapshot.Fleet
		if snapshot == nil || len(snapshot.Hosts) != 1 || !snapshot.Hosts[0].Cordoned || snapshot.Runners == nil ||
			snapshot.Runners.OccupiedSlots != 0 || len(snapshot.Jobs) != len(waiting) {
			t.Fatalf("unexpected fleet while paused: %+v", snapshot)
		}
		for i, id := range waiting {
			job := snapshot.Jobs[i]
			if job.ID != id || job.Status != "WAITING" || job.QueuePosition != i+1 {
				t.Fatalf("job %d should be waiting in position %d, got %+v", id, i+1, job)
			}
		}
	}
	ids := []int64{7001, 7002}
	tick()
	cordon(true)
	for _, id := range ids {
		j := job{id: id, repository: "owner/fixture", repositoryID: 2, installationID: 1, route: fmt.Sprintf("runquill-17-1-ci-%d", id)}
		for range 2 {
			if _, err := q.record(ctx, j, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	tick()
	assertPaused(0, ids...)
	cordon(false)
	tick()
	for i, id := range ids {
		list, err := runtime.pods.List(ctx, meta.ListOptions{LabelSelector: kubeRunnerLabel})
		if err != nil || len(list.Items) != 1 || !strings.HasPrefix(list.Items[0].Name, fmt.Sprintf("runquill-%d-", id)) {
			t.Fatalf("expected one Pod for FIFO job %d: %v", id, err)
		}
		name := list.Items[0].Name
		if generated.Load() != int32(i+1) {
			t.Fatalf("unexpected number of JIT requests at job %d: %d", id, generated.Load())
		}
		cordon(true)
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
		tick()
		assertPaused(int32(i+1), ids[i+1:]...)
		cordon(false)
		tick()
	}
	pods, err := runtime.pods.List(ctx, meta.ListOptions{LabelSelector: kubeRunnerLabel})
	if err != nil || len(pods.Items) != 0 || generated.Load() != 2 || deleted.Load() != 2 {
		t.Fatalf("cleanup incomplete: JIT %d, deletions %d, error %v", generated.Load(), deleted.Load(), err)
	}
}
