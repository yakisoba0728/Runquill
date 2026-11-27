package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type testOrphanHost struct {
	runners []runnerContainer
	removed []string
}

func (h *testOrphanHost) label() string { return "local" }

func (h *testOrphanHost) ownedRunners(context.Context) ([]runnerContainer, error) {
	return h.runners, nil
}

func (h *testOrphanHost) removeContainer(_ context.Context, id string) error {
	h.removed = append(h.removed, id)
	return nil
}

func TestRemoveOrphansOnlyRemovesProvablyIdleRunners(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo/actions/jobs/11":
			fmt.Fprint(w, `{"status":"in_progress"}`)
		case "GET /repos/owner/repo/actions/jobs/12", "GET /repos/owner/repo/actions/jobs/13":
			fmt.Fprint(w, `{"status":"completed"}`)
		case "GET /repos/owner/repo/actions/runners":
			name := r.URL.Query().Get("name")
			switch {
			case strings.HasPrefix(name, "runquill-12-"):
				fmt.Fprintf(w, `{"runners":[{"id":120,"name":%q,"status":"offline","busy":false}]}`, name)
			case strings.HasPrefix(name, "runquill-13-"):
				fmt.Fprintf(w, `{"runners":[{"id":130,"name":%q,"status":"online","busy":true}]}`, name)
			default:
				t.Errorf("unexpected runner lookup %s", name)
			}
		case "DELETE /repos/owner/repo/actions/runners/120":
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	q := testQueue(t)
	now := time.Now()
	for _, id := range []int64{10, 11, 12, 13} {
		recordTestJob(t, q, id, "runquill-1-1-ci", now)
	}
	if _, err := q.ExecContext(t.Context(), "UPDATE jobs SET runner_name = 'runquill-10-00000000000a', runner_host = 'local' WHERE job_id = 10"); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour)
	runner := func(name, id, state string, created time.Time) runnerContainer {
		return runnerContainer{name: name, id: id, state: state, owned: true, created: created}
	}
	host := &testOrphanHost{runners: []runnerContainer{
		runner("runquill-10-00000000000a", "tracked", "running", old),
		runner("runquill-11-00000000000b", "job-in-progress", "running", old),
		runner("runquill-12-00000000000c", "idle", "running", old),
		runner("runquill-13-00000000000d", "busy", "running", old),
		runner("runquill-14-00000000000e", "young", "exited", now.Add(-time.Minute)),
		runner("runquill-15-00000000000f", "unknown-running", "running", old),
		runner("runquill-16-000000000010", "unknown-exited", "exited", old),
		runner("runquill-17-000000000011", "worker", "exited", old),
		runner("not-a-runner", "foreign-name", "exited", old),
		{name: "runquill-18-000000000012", id: "unlabelled", state: "exited", created: old},
	}}
	host.runners[7].worker = "builder"
	var out bytes.Buffer
	err := removeOrphans(t.Context(), &out, q.DB, []orphanHost{host}, func() (*github, error) { return gh, nil },
		defaultOrphanGrace, now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(host.removed, []string{"idle", "unknown-exited"}) || !slices.Equal(deleted, []string{"/repos/owner/repo/actions/runners/120"}) {
		t.Fatalf("removed %v, deleted registrations %v\n%s", host.removed, deleted, out.String())
	}
	for _, want := range []string{
		"runquill-11-00000000000b: GitHub job 11 is in_progress",
		"runquill-13-00000000000d: GitHub reports the runner busy",
		"runquill-14-00000000000e: created 1m0s ago, inside the 10m0s grace period",
		"runquill-15-00000000000f: no queue record for job 15",
		"runquill-17-000000000011: belongs to outbound worker builder",
		"not-a-runner: name does not match a Runquill runner",
		"runquill-18-000000000012: missing the Runquill ownership label",
		"Removed 2 orphaned runners; skipped 7.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "runquill-10-") {
		t.Errorf("tracked runner was reported:\n%s", out.String())
	}
}

func TestGCOrphanRemovalRespectsStaleLeasesAndGrace(t *testing.T) {
	const name = "runquill-9-0123456789ab"
	for _, tt := range []struct {
		name   string
		leased bool
		owner  string
		age    time.Duration
		want   string
	}{
		{"old local lease", true, "local", time.Hour, "Removed 0 orphaned runners; skipped 0."},
		{"old legacy lease", true, "", time.Hour, "Removed 0 orphaned runners; skipped 0."},
		{"old lease on a different host", true, "other", time.Hour, "lease recorded on other"},
		{"unleased just inside grace", false, "", defaultOrphanGrace - time.Second, "inside the 10m0s grace period"},
		{"unleased at grace boundary", false, "", defaultOrphanGrace, "Removed local " + name},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := testQueue(t)
			ctx := t.Context()
			now := time.Unix(1_800_000_000, 0)
			if tt.leased {
				j := recordTestJob(t, q, 9, "runquill-1-1-ci", now.Add(-tt.age))
				if _, err := q.ExecContext(ctx, `UPDATE jobs SET phase = ?, runner_name = ?, runner_host = nullif(?, '')
					WHERE job_id = ?`, completed, name, tt.owner, j.id); err != nil {
					t.Fatal(err)
				}
			}
			host := &testOrphanHost{runners: []runnerContainer{{
				name: name, id: "container-id", state: "exited", owned: true, created: now.Add(-tt.age),
			}}}
			var out bytes.Buffer
			loadAPI := func() (*github, error) {
				t.Fatal("unexpected GitHub lookup")
				return nil, nil
			}
			if err := removeOrphans(ctx, &out, q.DB, []orphanHost{host}, loadAPI, defaultOrphanGrace, now); err != nil {
				t.Fatal(err)
			}
			var wantRemoved []string
			if !tt.leased && tt.age >= defaultOrphanGrace {
				wantRemoved = []string{"container-id"}
			}
			if !slices.Equal(host.removed, wantRemoved) || !strings.Contains(out.String(), tt.want) {
				t.Fatalf("removed %v, want %v and output containing %q:\n%s", host.removed, wantRemoved, tt.want, out.String())
			}
			leases, err := readRunnerLeases(ctx, q.DB)
			if owner, leased := leases[name]; err != nil || leased != tt.leased || owner != tt.owner {
				t.Fatalf("GC changed lease: owner %q, tracked %v, error %v", owner, leased, err)
			}
		})
	}
}

func TestParseRunnerInspect(t *testing.T) {
	id := strings.Repeat("ab", 32)
	runners, err := parseRunnerInspect("/runquill-1-0123456789ab|" + id + "|1||2026-11-19T00:01:02.123456789Z|exited\n")
	if err != nil || len(runners) != 1 || runners[0].name != "runquill-1-0123456789ab" || runners[0].id != id ||
		!runners[0].owned || runners[0].state != "exited" || runners[0].created.Unix() != 1795046462 {
		t.Fatalf("parsed %+v, %v", runners, err)
	}
	for _, invalid := range []string{"name|short|1||2026-11-19T00:01:02Z|exited", "/x|" + id + "|1||yesterday|exited", "/x|" + id + "|1"} {
		if _, err := parseRunnerInspect(invalid); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}
