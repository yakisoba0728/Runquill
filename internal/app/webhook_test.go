package app

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookRejectsInvalidRequestsAndIgnoresUnknownEvents(t *testing.T) {
	const body = `{"action":"queued","installation":{"id":1},
		"repository":{"id":2,"full_name":"owner/repo","private":true,"owner":{"login":"owner"}},
		"workflow_job":{"id":9,"run_id":17,"labels":["runquill-17-1-ci"]}}`
	q := testQueue(t)
	secret := []byte("fixture-secret")
	handler := webhook(q, &github{owner: "owner"}, secret, nil)
	for _, tt := range []struct {
		name, event, payload, signature string
		status                          int
	}{
		{"missing signature", "workflow_job", body, "", http.StatusUnauthorized},
		{"legacy signature only", "workflow_job", body, "sha1", http.StatusUnauthorized},
		{"wrong signature", "workflow_job", body, "wrong", http.StatusUnauthorized},
		{"oversize body", "workflow_job", body + strings.Repeat(" ", maxWebhookBytes+1-len(body)), "sha256", http.StatusRequestEntityTooLarge},
		{"malformed JSON", "workflow_job", body[:len(body)-1], "sha256", http.StatusBadRequest},
		{"unknown event", "unknown", body, "sha256", http.StatusAccepted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(tt.payload))
			req.Header.Set("X-GitHub-Event", tt.event)
			switch tt.signature {
			case "sha1":
				mac := hmac.New(sha1.New, secret)
				mac.Write([]byte(tt.payload))
				req.Header.Set("X-Hub-Signature", "sha1="+hex.EncodeToString(mac.Sum(nil)))
			case "sha256":
				signWebhookFixture(req, secret, tt.payload)
			case "wrong":
				signWebhookFixture(req, []byte("wrong-key"), tt.payload)
			}
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tt.status {
				t.Fatalf("status %d, want %d", res.Code, tt.status)
			}
			jobs, err := q.jobs(t.Context(), "")
			if err != nil || len(jobs) != 0 {
				t.Fatalf("unexpected jobs: %+v, error %v", jobs, err)
			}
		})
	}
}

func TestWebhookAndPollerForkRunsNeverRegisterRunners(t *testing.T) {
	const fork = `{"id":17,"repository":{"id":2},"head_repository":{"id":99,"full_name":"fork/repo"}}`
	const candidate = `{"id":9,"run_id":17,"status":"queued","labels":["runquill-17-1-ci"]}`
	for _, source := range []string{"webhook", "poll"} {
		t.Run(source, func(t *testing.T) {
			q := testQueue(t)
			f := &publicFixture{
				allow: true, repo: publicRepo, run: fork,
				forkRuns: map[string]string{"queued": fork},
				forkJobs: map[string]string{"17": candidate},
			}
			api := f.github(t)
			if source == "webhook" {
				body := `{"action":"queued","installation":{"id":1},"repository":` + publicRepo + `,"workflow_job":` + candidate + `}`
				secret := []byte("fixture-secret")
				req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
				req.Header.Set("X-GitHub-Event", "workflow_job")
				signWebhookFixture(req, secret, body)
				res := httptest.NewRecorder()
				webhook(q, api, secret, nil).ServeHTTP(res, req)
				if res.Code != http.StatusAccepted {
					t.Fatalf("webhook status %d, want %d", res.Code, http.StatusAccepted)
				}
			} else {
				repo := repository{ID: 2, FullName: "owner/repo", Visibility: "public"}
				repo.Owner.Login = "owner"
				p := &poller{github: api, queue: q, installationID: 1, cache: make(map[string]pollPage)}
				if _, err := p.scanRepository(t.Context(), repositoryScan{repo: repo, name: repo.FullName}); err != nil {
					t.Fatal(err)
				}
			}
			runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}
			s := &scheduler{queue: q, github: api, runtime: runtime, limit: 1}
			if err := s.tick(t.Context(), time.Now()); err != nil {
				t.Fatal(err)
			}
			job := readJob(t, q, 9)
			if job.phase != completed || !strings.Contains(job.failure, "comes from the fork") ||
				f.registered.Load() != 0 || len(runtime.started) != 0 {
				t.Fatalf("fork job did not stop before registration: %+v, registrations %d", job, f.registered.Load())
			}
		})
	}
}

func signWebhookFixture(req *http.Request, secret []byte, body string) {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
}
