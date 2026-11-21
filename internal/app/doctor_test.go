package app

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorReportsContainerEngine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	for _, binary := range []string{"docker", "podman"} {
		if err := os.WriteFile(filepath.Join(dir, binary), []byte(`#!/bin/sh
case "$1" in
  info) printf '%s\n' "$DOCTOR_TEST_INFO" ;;
  image) printf '%s\n' "$DOCTOR_TEST_PLATFORM" ;;
esac
`), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name, engine, info, platform string
		target                       docker
		want                         []string
	}{
		{"docker rootful", "docker", dockerRootfulInfo, "linux/amd64", docker{image: "img"},
			[]string{"OK   container engine docker 29.0.0 (linux/amd64, cgroup v2, rootful)\n", "OK   runner image img"}},
		{"docker rootless default", "docker", dockerRootlessInfo, "linux/arm64", docker{image: "img"},
			[]string{"OK   container engine docker 28.5.1 (linux/arm64, cgroup v2, rootless); runners start unprivileged without Docker-in-Docker", "OK   runner image img"}},
		{"docker rootless privileged", "docker", dockerRootlessInfo, "linux/arm64", docker{image: "img", pool: "build", requirePrivileged: true},
			[]string{`FAIL container engine docker 28.5.1 (linux/arm64, cgroup v2, rootless): worker pool "build" sets "privileged": true, but docker host "local" is rootless`}},
		{"podman rootless plain", "podman", podmanRootlessInfo, "linux/arm64", docker{image: "img", unprivileged: true},
			[]string{"OK   container engine podman 5.4.2 (linux/amd64, cgroup v2, rootless)\n",
				"FAIL runner image img: image platform linux/arm64 differs from podman platform linux/amd64"}},
		{"podman cgroup v1", "podman", podmanRootfulInfo, "linux/arm64", docker{image: "img"},
			[]string{"FAIL container engine podman 4.9.3 (linux/arm64, cgroup v1, rootful): requires Linux podman on amd64 or arm64 with cgroup v2"}},
		{"unreadable", "docker", "not json", "", docker{image: "img"},
			[]string{`FAIL container engine: container engine on "local": read docker info`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("RUNQUILL_CONTAINER_ENGINE", test.engine)
			t.Setenv("DOCTOR_TEST_INFO", test.info)
			t.Setenv("DOCTOR_TEST_PLATFORM", test.platform)
			var out strings.Builder
			checkDocker(t.Context(), test.target, nil, func(name string, err error) {
				if err != nil {
					fmt.Fprintf(&out, "FAIL %s: %s\n", name, err)
				} else {
					fmt.Fprintf(&out, "OK   %s\n", name)
				}
			})
			for _, want := range test.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q in doctor output:\n%s", want, out.String())
				}
			}
		})
	}
}

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

func TestDoctorJSONAndNextStep(t *testing.T) {
	const secret = "doctor-test-webhook-secret"
	t.Setenv("RUNQUILL_OWNER", "")
	t.Setenv("RUNQUILL_APP_ID", "")
	t.Setenv("RUNQUILL_KEY_FILE", "")
	t.Setenv("RUNQUILL_WEBHOOK_SECRET", secret)
	t.Setenv("RUNQUILL_DB", filepath.Join(t.TempDir(), "missing.sqlite3"))
	t.Setenv("RUNQUILL_BACKEND", "invalid")
	t.Setenv("RUNQUILL_PANEL_ADDR", "")

	var out strings.Builder
	if err := doctor(t.Context(), &out, "", true); err == nil {
		t.Fatal("doctor passed without configuration")
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("doctor JSON exposes the webhook secret:\n%s", out.String())
	}
	var result struct {
		Checks []struct {
			Name  string
			OK    *bool
			Error string
		}
		Passed *bool
	}
	if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
		t.Fatalf("doctor --json is not a JSON document: %v\n%s", err, out.String())
	}
	if result.Passed == nil || *result.Passed || len(result.Checks) == 0 {
		t.Fatalf("got %+v, want failed checks", result)
	}
	found := false
	for _, check := range result.Checks {
		if check.OK == nil || *check.OK != (check.Error == "") {
			t.Errorf("check %q has inconsistent ok and error fields", check.Name)
		}
		if check.Name == "configuration" {
			found = true
			if !strings.Contains(check.Error, "RUNQUILL_OWNER") {
				t.Errorf("configuration error = %q", check.Error)
			}
		}
	}
	if !found {
		t.Errorf("missing configuration check in %+v", result.Checks)
	}

	out.Reset()
	if err := doctor(t.Context(), &out, "", false); err == nil {
		t.Fatal("doctor passed without configuration")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "Next: ") || !strings.HasSuffix(last, "docs/operations.md#troubleshooting") {
		t.Errorf("last line = %q, want a pointer to the troubleshooting section", last)
	}
	if !strings.Contains(out.String(), "FAIL configuration: ") {
		t.Errorf("missing human-readable failure:\n%s", out.String())
	}
}
