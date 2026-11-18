package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkPoll100Repositories(b *testing.B) {
	q, err := openQueue(filepath.Join(b.TempDir(), "queue.sqlite3"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(q.close)
	const active = "owner/repo-100"
	repos := make([]repository, 0, 100)
	for i := range 100 {
		repo := repository{ID: int64(i + 1), FullName: fmt.Sprintf("owner/repo-%03d", i+1), Private: true}
		repo.Owner.Login = "owner"
		repos = append(repos, repo)
	}
	repoPage, err := json.Marshal(map[string][]repository{"repositories": repos})
	if err != nil {
		b.Fatal(err)
	}
	var calls, firstJobRequest, firstJobResponse, jobID atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := calls.Add(1)
		// Make sequential request latency visible while keeping the benchmark short.
		time.Sleep(time.Millisecond)
		path := r.URL.Path
		switch {
		case path == "/installation/repositories":
			w.Write(repoPage)
		case path == "/repos/"+active+"/actions/runs/9/jobs":
			firstJobRequest.CompareAndSwap(0, request)
			firstJobResponse.CompareAndSwap(0, time.Now().UnixNano())
			fmt.Fprintf(w, `{"jobs":[{"id":%d,"run_id":9,"labels":["runquill-9-1-build"],"status":"queued"}]}`, jobID.Add(1))
		case path == "/repos/"+active+"/actions/runs" && r.URL.Query().Get("status") == "in_progress":
			fmt.Fprint(w, `{"workflow_runs":[{"id":9}]}`)
		case strings.HasSuffix(path, "/actions/runs"):
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	b.Cleanup(server.Close)
	p := &poller{
		github: &github{client: server.Client(), base: server.URL, owner: "owner"}, queue: q,
		token:          installationToken{Token: "fixture", ExpiresAt: time.Now().Add(time.Hour)},
		installationID: 1, cache: make(map[string]pollPage),
	}
	if err := p.sweep(b.Context()); err != nil {
		b.Fatal(err)
	}
	var totalRequests, totalFirstJobRequests int64
	var totalDiscovery time.Duration
	b.ResetTimer()
	for b.Loop() {
		calls.Store(0)
		firstJobRequest.Store(0)
		firstJobResponse.Store(0)
		started := time.Now()
		if err := p.sweep(b.Context()); err != nil {
			b.Fatal(err)
		}
		totalRequests += calls.Load()
		ordinal := firstJobRequest.Load()
		if ordinal == 0 {
			b.Fatal("sweep did not inspect the active run")
		}
		totalFirstJobRequests += ordinal
		totalDiscovery += time.Duration(firstJobResponse.Load() - started.UnixNano())
	}
	b.ReportMetric(float64(totalRequests)/float64(b.N), "requests/sweep")
	b.ReportMetric(float64(totalFirstJobRequests)/float64(b.N), "requests-to-job/sweep")
	b.ReportMetric(float64(totalDiscovery.Microseconds())/float64(b.N)/1000, "job-response-ms/sweep")
}
