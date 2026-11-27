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

func TestDoctorReportsOptionalRunnerSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("RUNQUILL_OWNER", "")
	t.Setenv("RUNQUILL_APP_ID", "")
	t.Setenv("RUNQUILL_KEY_FILE", "")
	t.Setenv("RUNQUILL_PANEL_ADDR", "")
	t.Setenv("RUNQUILL_DB", filepath.Join(dir, "missing.sqlite3"))
	t.Setenv("RUNQUILL_BACKEND", "docker")
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	t.Setenv("RUNQUILL_DOCKER_HOSTS", "local")
	t.Setenv("DOCTOR_TEST_INFO", dockerRootfulInfo)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(`#!/bin/sh
case "$1" in
  info) printf '%s\n' "$DOCTOR_TEST_INFO" ;;
  image) printf '%s\n' linux/amd64 ;;
  volume) printf '%s' "$DOCTOR_TEST_VOLUMES" ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, pools, mirror, volumes, level string
		want                                []string
	}{
		{"unset", "", "", "", "info", []string{"RUNQUILL_REGISTRY_MIRROR): unset", `worker pool "default": cache=false, warm=0 per host`}},
		{"cache present", `{"build":{"hosts":["local"],"cache":true}}`, "https://mirror.internal", "runquill-cache-7", "info", []string{"configured and valid", `worker pool "build": cache=true, warm=0 per host`, `worker pool "build" cache volumes on local: 1`}},
		{"cache empty", `{"default":{"cache":true}}`, "", "", "info", []string{`worker pool "default" cache volumes on local: 0`}},
		{"warm", `{"default":{"warm":2}}`, "", "", "info", []string{`worker pool "default": cache=false, warm=2 per host`}},
		{"invalid mirror", "", "https://user:mirror-password@mirror.internal", "", "warn", []string{"RUNQUILL_REGISTRY_MIRROR must be an http:// or https:// origin"}},
		{"invalid warm size", `{"default":{"warm":101}}`, "", "", "warn", []string{"warm must be between 0 and 100"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("RUNQUILL_WORKER_POOLS", test.pools)
			t.Setenv("RUNQUILL_REGISTRY_MIRROR", test.mirror)
			t.Setenv("DOCTOR_TEST_VOLUMES", test.volumes)
			var out strings.Builder
			err := doctor(t.Context(), &out, "", true)
			if err == nil || err.Error() != "set RUNQUILL_OWNER, RUNQUILL_APP_ID, RUNQUILL_KEY_FILE" {
				t.Fatalf("expected only the missing GitHub configuration to fail, got %v\n%s", err, &out)
			}
			if strings.Contains(out.String(), "mirror-password") {
				t.Fatal("doctor exposed credentials from the invalid mirror URL")
			}
			var result struct{ Checks []doctorCheck }
			if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				found := false
				for _, check := range result.Checks {
					if check.Level == test.level && check.OK && strings.Contains(check.Name, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing %s check containing %q:\n%s", test.level, want, &out)
				}
			}
		})
	}
	for _, check := range []struct{ backend, name string }{
		{"docker", "saved operation settings"},
		{"kubernetes", "runner backend"},
	} {
		t.Run("invalid mirror prevents "+check.name, func(t *testing.T) {
			t.Setenv("RUNQUILL_BACKEND", check.backend)
			t.Setenv("RUNQUILL_K8S_RUNNER_NAMESPACE", "runner-fixture")
			t.Setenv("RUNQUILL_K8S_RUNNER_MODE", "plain")
			t.Setenv("RUNQUILL_WORKER_POOLS", "")
			t.Setenv("RUNQUILL_DOCKER_HOST_LIMITS", "")
			t.Setenv("RUNQUILL_REGISTRY_MIRROR", "invalid")
			if check.backend == "docker" {
				path := filepath.Join(t.TempDir(), "queue.sqlite3")
				q, err := openQueue(path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = q.Exec(`INSERT INTO operation_settings VALUES (1, 1, '{}')`)
				q.close()
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("RUNQUILL_DB", path)
			}
			var out strings.Builder
			_ = doctor(t.Context(), &out, "", true)
			var result struct{ Checks []doctorCheck }
			if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
				t.Fatal(err)
			}
			for _, got := range result.Checks {
				if got.Name == check.name && !got.OK && strings.Contains(got.Error, "RUNQUILL_REGISTRY_MIRROR") {
					return
				}
			}
			t.Fatalf("missing failed %s check:\n%s", check.name, &out)
		})
	}
}
