package main

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoctorChecksPersonalInstallation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, expected, selection, administration, event, scheme string
		repoInstallationID                                       int
		wantFailures                                             string
	}{
		{"ready", "", "all", "write", "workflow_job", "https", 123, ""},
		{"selected", "selected", "selected", "write", "workflow_job", "https", 123, ""},
		{"excluded", "selected", "selected", "write", "workflow_job", "https", 0, "GitHub App covers owner/new-repo"},
		{"incomplete", "", "selected", "read", "push", "http", 321,
			"GitHub App repository access (Only select repositories), GitHub App permissions, GitHub App event subscription, GitHub webhook configuration, GitHub App covers owner/new-repo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					t.Errorf("unexpected diagnostic request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch r.URL.Path {
				case "/users/owner/installation":
					fmt.Fprintf(w, `{"id":123,"account":{"login":"Owner","type":"User"},
						"repository_selection":%q,"permissions":{"actions":"read","administration":%q},"events":[%q]}`,
						test.selection, test.administration, test.event)
				case "/app/hook/config":
					fmt.Fprintf(w, `{"url":%q,"content_type":"json","insecure_ssl":"0"}`, test.scheme+"://example.com/webhook")
				case "/repos/owner/new-repo/installation":
					if test.repoInstallationID == 0 {
						http.NotFound(w, r)
						return
					}
					fmt.Fprintf(w, `{"id":%d}`, test.repoInstallationID)
				default:
					t.Errorf("unexpected diagnostic path: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)

			api := &github{client: server.Client(), base: server.URL, key: key, appID: "1234", owner: "owner", repositoryScope: test.expected}
			var failed []string
			checks := 0
			api.checkInstallation(t.Context(), "Owner/New-Repo", true, func(name string, err error) {
				checks++
				if err != nil {
					failed = append(failed, name)
					if test.name == "excluded" && !strings.Contains(err.Error(), "Add owner/new-repo at https://github.com/settings/installations/123") {
						t.Errorf("missing selected-repository instructions: %v", err)
					}
				}
			})
			if checks != 6 || strings.Join(failed, ", ") != test.wantFailures {
				t.Fatalf("got %d checks and failures %v, want 6 checks and %q", checks, failed, test.wantFailures)
			}
		})
	}
}
