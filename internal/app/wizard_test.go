package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yakisoba0728/Runquill/examples"
	"github.com/yakisoba0728/Runquill/internal/webui"
)

func TestBrowserWizardRunsSingleOriginThroughDeployment(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUNQUILL_WIZARD_TEST_LOG", filepath.Join(dir, "docker.log"))
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$RUNQUILL_WIZARD_TEST_LOG"
if [ "$1" = build ]; then
  printf 'Building fixture runner\n'
  printf 'fixture credential: example-'
  printf 'secret\n'
  while [ ! -e "$RUNQUILL_WIZARD_TEST_LOG.release" ]; do sleep 0.05; done
fi
`), 0700); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	var installed atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app-manifests/wizard-fixture/conversions":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":123,"slug":"runquill-test","pem":%q,"client_id":"example-client","client_secret":"example-secret","owner":{"login":"owner","type":"User"}}`, pemData)
		case "/users/owner/installation":
			if !installed.Load() {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, `{"id":7,"account":{"id":88,"type":"User","login":"owner"},"repository_selection":"selected","permissions":{"actions":"read","administration":"write"}}`)
		case "/app":
			fmt.Fprint(w, `{"slug":"runquill-test"}`)
		default:
			t.Errorf("unexpected GitHub request %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 18*time.Second)
	defer cancel()
	web := &setupWizard{dir: dir, key: "fixture-access", client: api.Client(), apiBase: api.URL, log: io.Discard, ctx: ctx,
		language: "en", phase: "options", sshConnection: "203.0.113.8 51420 198.51.100.7 22"}
	server := httptest.NewServer(web)
	defer server.Close()
	web.origin = server.URL
	cookies, _ := cookiejar.New(nil)
	client := &http.Client{Jar: cookies}
	request := func(method, route, origin string, form url.Values) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+route, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK && resp.Request.URL.Path == "/" && resp.Header.Get("Referrer-Policy") != "same-origin" {
			t.Fatal("wizard forms must preserve the Origin header on same-origin submits")
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	waitPage := func(route, marker string) string {
		t.Helper()
		for {
			_, body := request(http.MethodGet, route, "", nil)
			if strings.Contains(body, marker) {
				return body
			}
			select {
			case <-ctx.Done():
				t.Fatalf("wizard never showed %q on %s", marker, route)
			case <-time.After(30 * time.Millisecond):
			}
		}
	}
	if code, _ := request(http.MethodGet, "/", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("wizard without credentials returned %d", code)
	}
	if code, _ := request(http.MethodGet, "/status", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("wizard logs without credentials returned %d", code)
	}
	if code, body := request(http.MethodGet, "/?key=fixture-access&lang=ko", "", nil); code != 200 || !strings.Contains(body, "Runquill 시작하기") {
		t.Fatalf("localized wizard entry returned %d", code)
	}
	if code, _ := request(http.MethodPost, "/start", "https://other.example", url.Values{"lang": {"ko"}}); code != http.StatusForbidden {
		t.Fatalf("cross-origin setup returned %d", code)
	}
	if code, _ := request(http.MethodPost, "/start", "null", url.Values{"lang": {"ko"}}); code != http.StatusForbidden {
		t.Fatalf("opaque-origin setup returned %d", code)
	}
	request(http.MethodPost, "/start", server.URL, url.Values{
		"backend": {"docker"}, "repositories": {"selected"}, "lang": {"ko"},
		"max_runners": {"3"}, "runner_cpus": {"2"}, "runner_memory": {"4"},
	})
	manifest := waitPage("/", "GitHub App 등록")
	githubState := regexp.MustCompile(`state=([A-Za-z0-9]+)`).FindStringSubmatch(manifest)
	if len(githubState) != 2 || !strings.Contains(manifest, server.URL+"/connect/callback") {
		t.Fatal("GitHub manifest has no state or wizard callback")
	}
	if !strings.Contains(html.UnescapeString(manifest), `"callback_urls":["http://127.0.0.1:8081/auth/github/callback"]`) {
		t.Fatal("GitHub manifest must configure the panel login callback")
	}
	if code, _ := request(http.MethodGet, "/connect/callback?code=wizard-fixture&state=wrong", "", nil); code != 400 {
		t.Fatalf("GitHub callback without state returned %d", code)
	}
	request(http.MethodGet, "/connect/callback?code=wizard-fixture&state="+githubState[1], "", nil)
	install := waitPage("/", "https://github.com/apps/runquill-test/installations/new")
	if !strings.Contains(install, "<strong>Only select repositories</strong>") {
		t.Fatal("the App installation step did not show the selected access")
	}
	installed.Store(true)
	ready := waitPage("/", "배포 및 사전 점검")
	if strings.Contains(ready, "example-secret") {
		t.Fatal("GitHub client secret was displayed in the wizard")
	}
	envFile := filepath.Join(dir, ".env")
	touched, err := os.Stat(envFile)
	if err != nil || touched.Mode().Perm() != 0600 {
		t.Fatalf("wizard did not save a private environment file: %v", err)
	}
	envData, err := os.ReadFile(envFile)
	if err != nil || !strings.Contains(string(envData), "RUNQUILL_REPOSITORY_SCOPE=selected") ||
		!strings.Contains(string(envData), "RUNQUILL_PANEL_GITHUB_CLIENT_SECRET=example-secret") ||
		!strings.Contains(string(envData), "RUNQUILL_PANEL_GITHUB_CLIENT_ID=example-client") ||
		!strings.Contains(string(envData), "RUNQUILL_PANEL_BOOTSTRAP_ID=88") {
		t.Fatalf("wizard did not save selected coverage and OAuth settings: %v", err)
	}
	if !strings.Contains(string(envData), "\nRUNQUILL_MAX_RUNNERS=3\n") ||
		!strings.Contains(string(envData), "\nRUNQUILL_WORKER_POOLS='{\"default\":{\"cpus\":2,\"memory_mib\":4096}}'\n") {
		t.Fatalf("wizard did not save the runner capacity:\n%s", envData)
	}
	request(http.MethodPost, "/deploy", server.URL, nil)
	progress := waitPage("/status", "[redacted]")
	if !strings.Contains(progress, `"phase":"deploying"`) || !strings.Contains(progress, `"stage":"images"`) ||
		!strings.Contains(progress, "Building fixture runner") || strings.Contains(progress, "example-secret") {
		t.Fatal("deployment did not expose live sanitized output")
	}
	if err := os.WriteFile(filepath.Join(dir, "docker.log.release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	completed := waitPage("/", "운영 패널 열기")
	if !strings.Contains(completed, "ssh -L 8081:127.0.0.1:8081 "+wizardUser()+"@198.51.100.7") {
		t.Fatal("the local panel must show its SSH forward after remote setup")
	}
	if _, body := request(http.MethodGet, "/first-job.yml", "", nil); body != string(examples.FirstJob()) {
		t.Fatal("wizard served a different first-job workflow")
	}
	deploy, err := os.ReadFile(filepath.Join(dir, "docker.log"))
	if err != nil || !strings.Contains(string(deploy), "compose -f compose.yaml -f compose.panel.yaml up -d --build") ||
		!strings.Contains(string(deploy), "exec -T manager runquill doctor") {
		t.Fatal("wizard did not invoke deployment followed by doctor")
	}
}

func TestWizardStartsPublishedManagerWithoutWaitingForTheRunnerPull(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUNQUILL_WIZARD_TEST_LOG", filepath.Join(dir, "docker.log"))
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$RUNQUILL_WIZARD_TEST_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	environment := setupEnvironment("owner", 1, "ghcr.io/owner/runquill", "v0.3.2")
	if err := os.WriteFile(filepath.Join(dir, setupEnvFile), []byte(environment), 0600); err != nil {
		t.Fatal(err)
	}
	web := &setupWizard{dir: dir, log: io.Discard}
	if err := web.apply(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(os.Getenv("RUNQUILL_WIZARD_TEST_LOG"))
	const want = "compose -f compose.yaml -f compose.panel.yaml up -d --no-build --pull missing\n" +
		"compose -f compose.yaml -f compose.panel.yaml exec -T manager runquill doctor\n"
	if err != nil || string(commands) != want {
		t.Fatalf("release deployment commands: %s (%v)", commands, err)
	}
}

func TestWizardTailscaleRoutesKeepPanelPrivateAndRefuseConflicts(t *testing.T) {
	const hostname = "runquill.example.ts.net"
	private := wizardRoute{"serve", wizardServePort, "/", "http://127.0.0.1:8081"}
	public := wizardRoute{"funnel", wizardFunnelPort, "/webhook", "http://127.0.0.1:8080/webhook"}
	var config tailscaleMappings
	if err := json.Unmarshal([]byte(`{"TCP":{"8443":{"HTTPS":true},"9443":{"HTTPS":true}},"Web":{"runquill.example.ts.net:8443":{"Handlers":{"/webhook":{"Proxy":"http://127.0.0.1:8080/webhook"}}},"runquill.example.ts.net:9443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8081"}}}},"AllowFunnel":{"runquill.example.ts.net:8443":true}}`), &config); err != nil {
		t.Fatal(err)
	}
	for _, route := range []wizardRoute{private, public} {
		if exists, matches := config.routeState(hostname, route); !exists || !matches {
			t.Fatalf("configured route %s did not match: %t %t", route.path, exists, matches)
		}
	}
	private.command = "funnel"
	if exists, matches := config.routeState(hostname, private); !exists || matches {
		t.Fatal("the private panel must never be adopted as a public route")
	}
	public.path = "/"
	if exists, matches := config.routeState(hostname, public); !exists || matches {
		t.Fatal("refuse a different mapping on the reserved webhook port")
	}
}

func TestWizardKeepsPrivatePanelWhenFunnelFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := filepath.Join(dir, "serve-state")
	t.Setenv("RUNQUILL_TEST_SERVE_STATE", state)
	script := `#!/bin/sh
case "$1:$2" in
  status:--json)
    printf '%s\n' '{"BackendState":"Running","Self":{"DNSName":"runquill.example.ts.net.","CapMap":{"https":null,"funnel":null}},"CurrentTailnet":{"MagicDNSEnabled":true}}' ;;
  serve:status)
    if [ "$(cat "$RUNQUILL_TEST_SERVE_STATE")" = on ]; then
      printf '%s\n' '{"TCP":{"9443":{"HTTPS":true}},"Web":{"runquill.example.ts.net:9443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8081"}}}}}'
    else
      printf '%s\n' '{}'
    fi ;;
  serve:--https=9443)
    if [ "$4" = off ]; then printf off; else printf on; fi > "$RUNQUILL_TEST_SERVE_STATE" ;;
  funnel:--https=8443)
    printf '%s\n' 'funnel disabled by tailnet policy' >&2
    exit 1 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	network := wizardNetwork{serve: true, funnel: true, host: "runquill.example.ts.net"}
	for _, initial := range []string{"off", "on"} {
		if err := os.WriteFile(state, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
		var log strings.Builder
		serveFailed, funnelFailed := network.activate(t.Context(), &log)
		if serveFailed || !funnelFailed || !strings.Contains(log.String(), "funnel disabled") {
			t.Fatalf("Funnel failure was not surfaced independently: %s", &log)
		}
		result, err := os.ReadFile(state)
		if err != nil || string(result) != "on" {
			t.Fatalf("initial Serve state %q changed to %q: %v", initial, result, err)
		}
	}
}

func TestWizardDeploysToExplicitK3sContext(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	logPath := filepath.Join(dir, "kubectl.log")
	t.Setenv("RUNQUILL_KUBECTL_LOG", logPath)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$RUNQUILL_KUBECTL_LOG\"\n"
	script += "case \"$*\" in\n"
	script += "  *'create secret generic'*) printf '{\"kind\":\"Secret\"}\\n' ;;\n"
	script += "  *'--server-side'*) test \"$(cat)\" = '{\"kind\":\"Secret\"}' ;;\n"
	script += "  *'get namespace kube-system'*) printf '%s' cluster-uid ;;\n"
	script += "  *'get namespace runquill-system'*) test -z \"${RUNQUILL_WIZARD_NAMESPACE_EXISTS-}\" || printf '%s\\n' '{\"metadata\":{\"name\":\"runquill-system\"}}' ;;\n"
	script += "  *'create -f -'*) cat >/dev/null ;;\n"
	script += "esac\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	stageK3sWizardManifest(t, dir)
	web := &setupWizard{dir: dir, kubeContext: "fixture", log: io.Discard}
	t.Setenv("RUNQUILL_WIZARD_NAMESPACE_EXISTS", "1")
	if err := web.applyK3s(t.Context(), "123"); err == nil {
		t.Fatal("wizard accepted an existing Runquill namespace")
	}
	t.Setenv("RUNQUILL_WIZARD_NAMESPACE_EXISTS", "")
	if err := web.applyK3s(t.Context(), "123"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitSeq(strings.TrimSpace(string(data)), "\n")
	for line := range lines {
		if !strings.HasPrefix(line, "--context fixture ") {
			t.Fatalf("kubectl ran in the wrong context: %s", line)
		}
	}
	for _, action := range []string{
		"create -f -", "apply -f runquill-k3s.yaml", "create secret generic runquill-config --from-env-file=.env",
		"create secret generic runquill-github-key --from-file=github-app.pem",
		"rollout status deployment/runquill", "exec deployment/runquill -- runquill doctor",
	} {
		if !strings.Contains(string(data), action) {
			t.Fatalf("missing deployment step: %s", action)
		}
	}
}

func stageK3sWizardManifest(t *testing.T, dir string) {
	t.Helper()
	manifest, err := os.ReadFile("../../deploy/k3s/runquill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runquill-k3s.yaml"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWizardResumesOnlyItsK3sDeployment(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	stageK3sWizardManifest(t, dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUNQUILL_TEST_CLUSTER_UID", "first-cluster")
	const script = `#!/bin/sh
set -eu
shift 2
case "$*" in
  'get namespace kube-system '*) printf '%s' "$RUNQUILL_TEST_CLUSTER_UID" ;;
  'get namespace '*)
    if [ -f "$3.json" ]; then cat "$3.json"; fi ;;
  'create -f -')
    manifest=$(cat)
    case "$manifest" in
      *'"name":"runquill-system"'*) name=runquill-system ;;
      *'"name":"runquill-runners"'*) name=runquill-runners ;;
      *) exit 2 ;;
    esac
    test ! -f "$name.json"
    if [ "$name" = runquill-runners ] && [ "${RUNQUILL_TEST_FAIL_CREATE-}" = 1 ]; then exit 1; fi
    printf '%s' "$manifest" > "$name.json" ;;
  *'create secret generic'*) printf '{"kind":"Secret"}\n' ;;
  *'--server-side'*) cat >/dev/null ;;
  *'runquill doctor'*) test "${RUNQUILL_TEST_FAIL_CHECK-}" != 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	attempt := func(appID string) error {
		web := &setupWizard{dir: dir, kubeContext: "fixture", log: io.Discard}
		return web.applyK3s(t.Context(), appID)
	}
	markerPath := filepath.Join(dir, k3sWizardMarker)
	systemPath := filepath.Join(dir, "runquill-system.json")
	runnerPath := filepath.Join(dir, "runquill-runners.json")

	t.Setenv("RUNQUILL_TEST_FAIL_CREATE", "1")
	if err := attempt("123"); err == nil || !strings.Contains(err.Error(), "create K3s namespace runquill-runners") {
		t.Fatalf("expected a partial namespace deployment, got %v", err)
	}
	saved, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	var state k3sWizardDeployment
	if err := json.Unmarshal(saved, &state); err != nil || state.ID == "" {
		t.Fatalf("missing local setup ID: %v", err)
	}
	system, err := os.ReadFile(systemPath)
	if err != nil || !bytes.Contains(system, []byte(state.ID)) || !bytes.Contains(system, []byte(`"pod-security.kubernetes.io/enforce":"restricted"`)) {
		t.Fatalf("first namespace missing ownership or Pod Security: %s, %v", system, err)
	}
	t.Setenv("RUNQUILL_TEST_FAIL_CREATE", "")
	t.Setenv("RUNQUILL_TEST_CLUSTER_UID", "second-cluster")
	if err := attempt("123"); err == nil || !strings.Contains(err.Error(), "original cluster and GitHub App") {
		t.Fatalf("expected a different cluster to be rejected, got %v", err)
	}
	t.Setenv("RUNQUILL_TEST_CLUSTER_UID", "first-cluster")
	if err := attempt("456"); err == nil || !strings.Contains(err.Error(), "original cluster and GitHub App") {
		t.Fatalf("expected a different App to be rejected, got %v", err)
	}
	unowned := []byte(`{"metadata":{"name":"runquill-runners","annotations":{"runquill.io/setup-id":"another-install"}}}`)
	if err := os.WriteFile(runnerPath, unowned, 0600); err != nil {
		t.Fatal(err)
	}
	if err := attempt("123"); err == nil || !strings.Contains(err.Error(), "already has a Runquill namespace") {
		t.Fatalf("expected a namespace owned by another install to be rejected, got %v", err)
	}
	if err := os.Remove(runnerPath); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RUNQUILL_TEST_FAIL_CHECK", "1")
	if err := attempt("123"); err == nil {
		t.Fatal("expected doctor failure")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("setup marker did not survive doctor failure: %v", err)
	}
	runner, err := os.ReadFile(runnerPath)
	if err != nil || !bytes.Contains(runner, []byte(state.ID)) {
		t.Fatalf("second namespace not created with original ownership: %s, %v", runner, err)
	}
	t.Setenv("RUNQUILL_TEST_FAIL_CHECK", "")
	if err := attempt("123"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup marker remained after successful deployment: %v", err)
	}
	if err := attempt("123"); err == nil || !strings.Contains(err.Error(), "documented upgrade flow") {
		t.Fatalf("expected completed deployments to use the upgrade flow, got %v", err)
	}
}

func TestWizardFirstStepErrorsStayInBrowserWithAnswers(t *testing.T) {
	web := &setupWizard{dir: t.TempDir(), log: io.Discard, ctx: t.Context(), language: "en"}
	page := func(form url.Values, lang string) string {
		t.Helper()
		web.phase = "working"
		web.configure(form)
		if web.phase != "options" || !web.failed {
			t.Fatalf("invalid answers were not returned to the first step: %s %t", web.phase, web.failed)
		}
		rec := httptest.NewRecorder()
		web.render(rec, httptest.NewRequest(http.MethodGet, "/?lang="+lang, nil))
		return rec.Body.String()
	}
	invalid := func(body, name string) bool {
		return regexp.MustCompile(`name="` + name + `"[^>]*aria-invalid="true" aria-describedby="wizard-error[ "]`).MatchString(body)
	}
	errorText := func(body string) int { return strings.Count(body, `id="wizard-error"`) }

	body := page(url.Values{"backend": {"k3s"}, "repositories": {"selected"}, "context": {`-x"<b>`}, "organization": {"acme"}}, "ko")
	if !strings.Contains(body, "배포할 k3s 컨텍스트 이름을 정확히 입력하세요.") || !strings.Contains(body, `id="wizard-error"`) ||
		strings.Contains(body, "Runquill 터미널에서") {
		t.Fatal("the K3s context problem was not explained in the browser")
	}
	if !invalid(body, "context") || invalid(body, "organization") {
		t.Fatal("only the K3s context field should be marked invalid")
	}
	if errorText(body) != 1 || !regexp.MustCompile(`name="context"[^>]*>\s*<p class="field-error" id="wizard-error" role="alert"><strong>오류:</strong>`).MatchString(body) {
		t.Fatal("the K3s context problem must be shown once, right below its field, with a text prefix")
	}
	if !strings.Contains(body, `value="-x&#34;&lt;b&gt;"`) || strings.Contains(body, `-x"<b>`) {
		t.Fatal("the submitted context was not restored as escaped text")
	}
	if !strings.Contains(body, `value="acme"`) || !strings.Contains(body, `<option value="k3s" selected>`) ||
		!strings.Contains(body, `<input type="radio" name="repositories" value="selected" checked>`) || !strings.Contains(body, `<details class="advanced" open>`) {
		t.Fatal("the submitted answers were not restored")
	}
	if !regexp.MustCompile(`name="context"[^>]*required`).MatchString(body) || strings.Contains(body, "data-k3s hidden") {
		t.Fatal("K3s must show and require its context")
	}

	body = page(url.Values{"backend": {"docker"}, "group": {"abc"}}, "en")
	if !strings.Contains(body, "Enter a positive organization runner group ID.") || !invalid(body, "group") ||
		!strings.Contains(body, `value="abc"`) || !strings.Contains(body, `<details class="advanced" open>`) {
		t.Fatal("the runner group problem was not shown next to its field")
	}
	if regexp.MustCompile(`name="context"[^>]*required`).MatchString(body) || !strings.Contains(body, "data-k3s hidden") {
		t.Fatal("Docker must not require a K3s context")
	}

	body = page(url.Values{"backend": {"docker"}, "panel_url": {"https://admin:hunter2@panel.example"}}, "en")
	if !invalid(body, "panel_url") || strings.Contains(body, "hunter2") {
		t.Fatal("a panel origin with credentials must be flagged without echoing them")
	}

	body = page(url.Values{"backend": {"k3s"}, "context": {"fixture"}}, "ko")
	if !strings.Contains(body, "runquill-k3s.yaml 파일이 없습니다") || strings.Contains(body, `aria-invalid="true"`) ||
		!strings.Contains(body, `value="fixture"`) || errorText(body) != 1 || !strings.Contains(body, `<p class="notice" data-state="failed" role="alert" id="wizard-error">`) {
		t.Fatal("a missing K3s manifest was not explained in the browser")
	}

	web.form, web.issue, web.failed = wizardForm{}, nil, false
	rec := httptest.NewRecorder()
	web.render(rec, httptest.NewRequest(http.MethodGet, "/?lang=en", nil))
	body = rec.Body.String()
	if strings.Contains(body, `id="wizard-error"`) || strings.Contains(body, `<details class="advanced" open>`) {
		t.Fatal("a fresh first step must not show an earlier problem")
	}
	if scope, advanced := strings.Index(body, `name="repositories" value="" checked`), strings.Index(body, `<details class="advanced"`); scope < 0 || advanced < scope {
		t.Fatal("a fresh first step must show repository access outside advanced settings")
	}
}

func TestRecommendCapacityLeavesRoomForTheHost(t *testing.T) {
	for _, test := range []struct {
		host wizardHost
		want wizardCapacity
	}{
		{wizardHost{CPUs: 16, MemoryMiB: 65536}, wizardCapacity{Runners: 7, CPUs: 2, MemoryMiB: 8704}},
		{wizardHost{CPUs: 64, MemoryMiB: 262144}, wizardCapacity{Runners: 31, CPUs: 2, MemoryMiB: 8192}},
		{wizardHost{CPUs: 4, MemoryMiB: 8192}, wizardCapacity{Runners: 1, CPUs: 2, MemoryMiB: 4096}},
		{wizardHost{CPUs: 8, MemoryMiB: 6144}, wizardCapacity{Runners: 1, CPUs: 6, MemoryMiB: 3072}},
		{wizardHost{CPUs: 2, MemoryMiB: 2048}, wizardCapacity{Runners: 1, CPUs: 1, MemoryMiB: 1024}},
		{wizardHost{CPUs: 1, MemoryMiB: 1024}, wizardCapacity{Runners: 1, CPUs: 0.5, MemoryMiB: 512}},
		{wizardHost{CPUs: 12}, wizardCapacity{Runners: 5, CPUs: 2}},
		{wizardHost{CPUs: 24, MemoryMiB: 48128}, wizardCapacity{Runners: 11, CPUs: 2, MemoryMiB: 3584}},
		{wizardHost{}, wizardCapacity{Runners: 1, CPUs: 0.5}},
	} {
		if got := recommendCapacity(test.host); got != test.want {
			t.Errorf("recommendCapacity(%+v) = %+v, want %+v", test.host, got, test.want)
		}
	}
	if got := parseMemInfo([]byte("MemFree:  1000 kB\nMemTotal:       65755136 kB\n")); got != 64214 {
		t.Fatalf("MemTotal parsed as %d MiB", got)
	}
	if parseMemInfo([]byte("MemFree: 12 kB\n")) != 0 || parseMemInfo([]byte("MemTotal: lots kB\n")) != 0 {
		t.Fatal("an unreadable MemTotal must count as unknown")
	}
}

func TestWizardCapacityReachesConfiguration(t *testing.T) {
	const base = setupEnvHeader + "RUNQUILL_OWNER=owner\nRUNQUILL_APP_ID=1\nRUNQUILL_KEY_FILE=./github-app.pem\n"
	write := func(t *testing.T, content string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	read := func(t *testing.T, dir string) map[string]string {
		data, err := os.ReadFile(filepath.Join(dir, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, ".env"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("the .env must stay private")
		}
		values, err := setupValues(data)
		if err != nil {
			t.Fatal(err)
		}
		return values
	}
	capacity, err := parseCapacity(wizardForm{MaxRunners: "3", RunnerCPUs: "1.5", RunnerMemory: "2.5"})
	if err != nil || capacity != (wizardCapacity{Runners: 3, CPUs: 1.5, MemoryMiB: 2560}) {
		t.Fatalf("parseCapacity = %+v, %v", capacity, err)
	}

	dir := write(t, base)
	if err := saveCapacity(dir, false, capacity); err != nil {
		t.Fatal(err)
	}
	values := read(t, dir)
	t.Setenv("RUNQUILL_MAX_RUNNERS", values["RUNQUILL_MAX_RUNNERS"])
	if limit, err := runnerLimit(); err != nil || limit != 3 {
		t.Fatalf("RUNQUILL_MAX_RUNNERS gave %d, %v", limit, err)
	}
	// Compose strips the single quotes, as for the README's examples.
	raw := values["RUNQUILL_WORKER_POOLS"]
	if !strings.HasPrefix(raw, "'") || !strings.HasSuffix(raw, "'") {
		t.Fatalf("worker pools are not quoted for Compose: %s", raw)
	}
	fleet, err := parseDockerFleet("runner:local", "local")
	if err != nil {
		t.Fatal(err)
	}
	pools, err := fleet.configurePools(strings.Trim(raw, "'"))
	if err != nil || pools[defaultWorkerPool].CPUs != 1.5 || pools[defaultWorkerPool].MemoryMiB != 2560 ||
		fleet.hosts[0].cpus != 1.5 || fleet.hosts[0].memoryMiB != 2560 {
		t.Fatalf("worker pools did not carry the limits: %+v %v", pools, err)
	}

	// A resumed directory keeps what it already has.
	dir = write(t, base+"RUNQUILL_MAX_RUNNERS=5\n")
	if err := saveCapacity(dir, false, wizardCapacity{Runners: 2}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".env")); strings.Count(string(data), "RUNQUILL_MAX_RUNNERS") != 1 || read(t, dir)["RUNQUILL_MAX_RUNNERS"] != "5" {
		t.Fatal("an existing runner limit was duplicated or replaced")
	}

	// K3s takes Kubernetes quantities and never worker pools, which its backend refuses.
	dir = write(t, base)
	if err := saveCapacity(dir, true, capacity); err != nil {
		t.Fatal(err)
	}
	values = read(t, dir)
	if _, exists := values["RUNQUILL_WORKER_POOLS"]; exists || values["RUNQUILL_MAX_RUNNERS"] != "3" {
		t.Fatalf("K3s configuration is wrong: %v", values)
	}
	t.Setenv("RUNQUILL_K8S_RUNNER_NAMESPACE", "runquill-runners")
	t.Setenv("RUNQUILL_K8S_JOB_CPU", values["RUNQUILL_K8S_JOB_CPU"])
	t.Setenv("RUNQUILL_K8S_JOB_MEMORY", values["RUNQUILL_K8S_JOB_MEMORY"])
	opts, err := readKubeRunnerOptions("runner:local")
	if err != nil || opts.resources.Limits.Cpu().String() != "1500m" || opts.resources.Limits.Memory().String() != "2560Mi" {
		t.Fatalf("Kubernetes limits = %v, %v", opts.resources.Limits, err)
	}

	// Empty limits write nothing beyond the runner count; invalid ones are flagged on their field.
	dir = write(t, base)
	if err := saveCapacity(dir, false, wizardCapacity{Runners: 1}); err != nil {
		t.Fatal(err)
	}
	if _, exists := read(t, dir)["RUNQUILL_WORKER_POOLS"]; exists {
		t.Fatal("no limits must leave worker pools unset")
	}
	for field, form := range map[string]wizardForm{
		"max_runners": {MaxRunners: "0"}, "runner_cpus": {RunnerCPUs: "0.001"}, "runner_memory": {RunnerMemory: "NaN"},
	} {
		var issue *wizardIssue
		if _, err := parseCapacity(form); !errors.As(err, &issue) || issue.field != field {
			t.Errorf("%s was not rejected on its field: %v", field, err)
		}
	}
}

func TestWizardDockerCheckBlocksBeforeGitHub(t *testing.T) {
	reply := func(output string, err error) wizardCommandRunner {
		return func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "docker" || strings.Join(args, " ") != "version --format {{.Server.Version}}" {
				t.Errorf("unexpected Docker check: %s %v", name, args)
			}
			return []byte(output), err
		}
	}
	for _, test := range []struct {
		run   wizardCommandRunner
		state string
	}{
		{nil, ""},
		{reply("27.3.1\n", nil), "ok"},
		{reply("", &exec.Error{Name: "docker", Err: exec.ErrNotFound}), "missing"},
		{reply("permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock\n", errors.New("exit status 1")), "permission"},
		{reply("Client: Docker Engine - Community\n Version: 27.3.1\npermission denied while trying to connect to the Docker daemon socket\n", errors.New("exit status 1")), "permission"},
		{reply("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n", errors.New("exit status 1")), "unreachable"},
	} {
		if got := probeWizardDocker(t.Context(), test.run); got.State != test.state {
			t.Errorf("Docker check state %q, want %q", got.State, test.state)
		}
	}

	web := &setupWizard{dir: t.TempDir(), log: io.Discard, ctx: t.Context(), language: "en", phase: "options",
		command: reply("permission denied while trying to connect to the Docker daemon socket\n", errors.New("exit status 1"))}
	page := func(lang string) string {
		rec := httptest.NewRecorder()
		web.render(rec, httptest.NewRequest(http.MethodGet, "/?lang="+lang, nil))
		return rec.Body.String()
	}
	body := page("en")
	if !strings.Contains(body, `id="wizard-docker"`) || !strings.Contains(body, "sudo usermod -aG docker "+html.EscapeString(wizardUser())) ||
		!regexp.MustCompile(`name="backend" aria-describedby="wizard-docker"`).MatchString(body) || strings.Contains(body, `id="wizard-error"`) {
		t.Fatal("a Docker permission problem must be explained on the first step with the fix")
	}
	if body = page("ko"); !strings.Contains(body, "이 사용자는 Docker를 쓸 권한이 없습니다") {
		t.Fatal("the Docker notice is missing in Korean")
	}
	web.phase = "working"
	web.configure(url.Values{"backend": {"docker"}, "max_runners": {"2"}})
	if web.phase != "options" || web.issue == nil || web.issue.field != "backend" {
		t.Fatalf("setup went on without Docker access: %s %+v", web.phase, web.issue)
	}
	if body = page("en"); !strings.Contains(body, `aria-describedby="wizard-error wizard-docker"`) || !strings.Contains(body, `value="2"`) {
		t.Fatal("the refused start must point at the Docker notice and keep the answers")
	}

	// Fixed and reloaded: the notice and the refusal go away, and the answers stay.
	web.command, web.host = reply("27.3.1\n", nil), wizardHost{CPUs: 4, MemoryMiB: 8192}
	body = page("en")
	if strings.Contains(body, `id="wizard-docker"`) || strings.Contains(body, `id="wizard-error"`) || strings.Contains(body, `data-state="failed"`) ||
		!strings.Contains(body, "Docker Compose · Up to 2 at once") || !strings.Contains(body, `name="max_runners" type="number" inputmode="numeric" min="1" max="1000" step="1" value="2"`) {
		t.Fatal("a fixed Docker host must clear the earlier refusal and keep the answers")
	}

	// A fresh first step suggests limits from the host's size.
	web.form, web.host = wizardForm{}, wizardHost{CPUs: 6, MemoryMiB: 8192}
	body = page("en")
	if strings.Contains(body, `id="wizard-docker"`) || !strings.Contains(body, "Docker Compose · Up to 2 at once") ||
		!strings.Contains(body, `name="max_runners" type="number" inputmode="numeric" min="1" max="1000" step="1" value="2"`) ||
		!strings.Contains(body, `name="runner_memory" type="number" inputmode="decimal" min="0.01" step="any" value="2"`) ||
		!strings.Contains(body, "Detected 6 CPUs and 8.0 GiB") {
		t.Fatal("a reachable Docker host must show its size and the suggested limits")
	}
}

func TestWizardStartMessageOffersSSHForward(t *testing.T) {
	plain := wizardStartMessage("http://127.0.0.1:40123", "SECRETKEY", 40123, "", "alice")
	if !strings.HasPrefix(plain, "Open http://127.0.0.1:40123/?key=SECRETKEY in your browser.\n") ||
		!strings.Contains(plain, "forward port 40123 over SSH") || strings.Contains(plain, "ssh -L") ||
		!strings.Contains(plain, "secret for this setup session") {
		t.Fatalf("local start message:\n%s", plain)
	}
	ssh := wizardStartMessage("http://127.0.0.1:40123", "SECRETKEY", 40123, "203.0.113.9 51234 198.51.100.7 22", "alice")
	if !strings.Contains(ssh, "\n  ssh -L 40123:127.0.0.1:40123 alice@198.51.100.7\n") || !strings.Contains(ssh, "On your own computer") ||
		strings.Count(ssh, "SECRETKEY") != 1 {
		t.Fatalf("SSH start message:\n%s", ssh)
	}
	for connection, want := range map[string]string{
		"203.0.113.9 51234 198.51.100.7 2222": "ssh -L 8080:127.0.0.1:8080 -p 2222 alice@198.51.100.7",
		"2001:db8::1 51234 2001:db8::2 22":    "ssh -L 8080:127.0.0.1:8080 alice@2001:db8::2",
		"203.0.113.9 51234 not-an-address 22": "",
		"garbage":                             "",
	} {
		if got := wizardSSHForward(8080, connection, "alice"); got != want {
			t.Errorf("wizardSSHForward(%q) = %q, want %q", connection, got, want)
		}
	}
	if got := wizardSSHForward(8080, "203.0.113.9 51234 198.51.100.7 22", "a b;rm"); got != "ssh -L 8080:127.0.0.1:8080 USER@198.51.100.7" {
		t.Errorf("an odd login must fall back to a placeholder: %q", got)
	}
}

func TestWizardLanguageSwitchKeepsTheStep(t *testing.T) {
	web := &setupWizard{origin: "http://example.test", key: "fixture-access", log: io.Discard, ctx: t.Context(), language: "en", phase: "working"}
	web.step, web.handler = "/connect", http.StripPrefix("/connect", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %t", r.URL.Path, wizardKorean(context.WithValue(r.Context(), setupWizardKey{}, web)))
	}))
	req := httptest.NewRequest(http.MethodGet, "http://example.test/connect/?lang=ko", nil)
	req.AddCookie(&http.Cookie{Name: wizardCookie, Value: "fixture-access"})
	rec := httptest.NewRecorder()
	web.ServeHTTP(rec, req)
	if rec.Body.String() != "/ true" || !strings.Contains(rec.Header().Get("Set-Cookie"), "runquill_language=ko") {
		t.Fatalf("switching language on a GitHub step returned %q", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "http://example.test/connect/?lang=en", nil)
	rec = httptest.NewRecorder()
	web.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || web.language != "ko" {
		t.Fatal("a language link must not work without the session")
	}
	page := httptest.NewRecorder()
	web.render(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(page.Body.String(), `<a class="lang" href="?lang=en" lang="en" hreflang="en">English</a>`) {
		t.Fatal("every wizard page needs a language link")
	}
}

func TestStandaloneSetupPagesLoadTheWizardStylesheet(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = panelSetupPage.Execute(w, struct {
			Origin, Callback, State, Bootstrap, Form string
			Korean, InWizard, Manual                 bool
			Chrome                                   *wizardChrome
		}{Origin: "https://panel.example", Callback: "https://panel.example/auth/github/callback", State: "s", Form: "/", Manual: true})
	})
	finished := make(chan struct{})
	t.Setenv("SSH_CONNECTION", "203.0.113.9 51234 198.51.100.7 22")
	output := make(manifestTestOutput, 8)
	done := make(chan error, 1)
	go func() {
		_, err := runSetupBrowser(t.Context(), output, listener, mux, "to test", "", finished)
		done <- err
	}()
	base := "http://" + listener.Addr().String()
	get := func(path string) *http.Response {
		t.Helper()
		res, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	css := get("/wizard.css")
	data, _ := io.ReadAll(css.Body)
	css.Body.Close()
	policy := css.Header.Get("Content-Security-Policy")
	if css.StatusCode != 200 || !strings.HasPrefix(css.Header.Get("Content-Type"), "text/css") || string(data) != webui.WizardCSS ||
		!strings.Contains(policy, "default-src 'none'") || !strings.Contains(policy, "style-src 'self'") ||
		strings.Contains(policy, "unsafe-inline") || strings.Contains(policy, "script-src") {
		t.Fatalf("standalone stylesheet: HTTP %d, CSP %q", css.StatusCode, policy)
	}
	page := get("/")
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), `<link rel="stylesheet" href="/wizard.css">`) || strings.Contains(string(body), "<script") ||
		!strings.Contains(string(body), `<button type="button" class="copy" hidden>`) {
		t.Fatal("a standalone page must link the stylesheet, load no script and hide script-only controls")
	}
	close(finished)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var printed strings.Builder
	for len(output) > 0 {
		printed.WriteString(<-output)
	}
	if !strings.Contains(printed.String(), "@198.51.100.7\n") {
		t.Fatalf("the forward command should name this server:\n%s", printed.String())
	}
}

func TestWizardDiscoversUsableTailscaleRoutes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUNQUILL_TEST_TAILSCALE_USER", wizardUser())
	for _, test := range []struct {
		self, mappings string
		serve, funnel  bool
	}{
		{`"DNSName":"runquill.example.ts.net.","CapMap":{"https":null,"funnel":null}`, `{}`, true, true},
		{`"DNSName":"runquill.example.ts.net.","CapMap":{"https://tailscale.com/cap/https":null,"https://tailscale.com/cap/funnel":null}`, `{"TCP":{"8443":{"TCPForward":"localhost:4000"}}}`, true, false},
		{`"DNSName":"runquill.example.ts.net.","Capabilities":["https://tailscale.com/cap/https","https://tailscale.com/cap/funnel"]`, `{"TCP":{"9443":{"HTTPS":true}}}`, false, true},
		{`"DNSName":"runquill.example.ts.net.","CapMap":{"https":null}`, `{}`, true, false},
		{`"DNSName":"runquill.example.ts.net."`, `{}`, false, false},
		{`"DNSName":""`, `{}`, false, false},
	} {
		script := "#!/bin/sh\ncase \"$1:$2\" in\n" +
			"status:--json) printf '%s\\n' '{\"BackendState\":\"Running\",\"Self\":{" + test.self + "},\"CurrentTailnet\":{\"MagicDNSEnabled\":true}}' ;;\n" +
			"serve:status) printf '%s\\n' '" + test.mappings + "' ;;\n" +
			"debug:prefs) printf '{\"OperatorUser\":\"%s\"}\\n' \"$RUNQUILL_TEST_TAILSCALE_USER\" ;;\n" +
			"*) exit 1 ;;\nesac\n"
		if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		if network := discoverWizardNetwork(t.Context()); network.serve != test.serve || network.funnel != test.funnel {
			t.Fatalf("%s %s: Serve = %t, Funnel = %t", test.self, test.mappings, network.serve, network.funnel)
		}
	}
}

func TestWizardChecksServeBeforeChoosingOAuthOrigin(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUNQUILL_TEST_TAILSCALE_USER", wizardUser())
	t.Setenv("RUNQUILL_TEST_SERVE_DENIED", "")
	script := `#!/bin/sh
case "$1:$2" in
  status:--json)
    printf '%s\n' '{"BackendState":"Running","Self":{"DNSName":"runquill.example.ts.net.","CapMap":{"https":null,"funnel":null}},"CurrentTailnet":{"MagicDNSEnabled":true}}' ;;
  serve:status)
    if [ -e "$RUNQUILL_TEST_SERVE_STATE" ]; then
      printf '%s\n' '{"Web":{"runquill.example.ts.net:9443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8081"}}}}}'
    else
      printf '%s\n' '{}'
    fi ;;
  debug:prefs) printf '{"OperatorUser":"%s"}\n' "$RUNQUILL_TEST_TAILSCALE_USER" ;;
  serve:--https=9443)
    printf '%s\n' "$*" >> "$RUNQUILL_TEST_SERVE_LOG"
    if [ "$4" = off ]; then
      rm "$RUNQUILL_TEST_SERVE_STATE"
    elif [ "$RUNQUILL_TEST_SERVE_DENIED" = yes ]; then
      printf '%s\n' 'permission denied' >&2
      exit 1
    else
      : > "$RUNQUILL_TEST_SERVE_STATE"
    fi ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                               string
		denied, saved, persisted, portBusy bool
		localOnly, skipTailscale           bool
		panelURL                           string
		wantWrites                         int
	}{
		{name: "permission denied selects local OAuth callback", denied: true, wantWrites: 1},
		{name: "working Serve supplies callback and is removed before persistence", wantWrites: 2},
		{name: "newly saved callback keeps the route", persisted: true, wantWrites: 1},
		{name: "saved setup does not change callbacks or create a listener", saved: true},
		{name: "an occupied backend stays private", portBusy: true},
		{name: "explicit URL takes precedence", panelURL: "https://private.example"},
		{name: "opt out skips all managed routes", localOnly: true},
		{name: "installer opt out skips all managed routes", skipTailscale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "tailnet.log")
			state := filepath.Join(dir, "serve-state")
			t.Setenv("RUNQUILL_TEST_SERVE_LOG", log)
			t.Setenv("RUNQUILL_TEST_SERVE_STATE", state)
			if test.denied {
				t.Setenv("RUNQUILL_TEST_SERVE_DENIED", "yes")
			}
			if test.saved {
				if err := os.WriteFile(filepath.Join(dir, setupEnvFile), []byte("RUNQUILL_PANEL_URL="+defaultSetupPanelURL+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			web := &setupWizard{dir: dir, log: io.Discard, command: execWizardCommand,
				form: wizardForm{LocalOnly: test.localOnly}, skipTailscale: test.skipTailscale,
				listen: func(network, address string) (net.Listener, error) {
					if address != "127.0.0.1:8081" {
						t.Errorf("unexpected panel listener address %s", address)
					}
					if test.portBusy {
						return nil, errors.New("port already occupied")
					}
					return net.Listen(network, "127.0.0.1:0")
				},
			}
			opts := setupOptions{panel: true, panelURL: test.panelURL}
			cleanup := web.prepareNetwork(t.Context(), &opts)
			wantPanelURL := test.panelURL
			if test.persisted || test.wantWrites == 2 {
				wantPanelURL = "https://runquill.example.ts.net:9443"
			}
			if opts.panelURL != wantPanelURL {
				t.Errorf("panel origin %q, want %q", opts.panelURL, wantPanelURL)
			}
			if wantFunnel := !test.saved && !test.localOnly && !test.skipTailscale; (opts.webhookURL != "") != wantFunnel {
				t.Error("Funnel choice must be independent of Serve and preserve saved endpoints")
			}
			if test.persisted {
				if err := os.WriteFile(filepath.Join(dir, setupEnvFile), []byte("RUNQUILL_PANEL_URL="+opts.panelURL+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cleanup()
			commands, _ := os.ReadFile(log)
			if writes := strings.Count(string(commands), "\n"); writes != test.wantWrites {
				t.Errorf("wrote %d route commands, want %d: %s", writes, test.wantWrites, commands)
			}
			if _, err := os.Stat(state); (err == nil) != test.persisted {
				t.Error("the Serve route must survive only with saved OAuth configuration")
			}
		})
	}
}
