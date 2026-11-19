package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	web := &setupWizard{dir: dir, key: "fixture-access", client: api.Client(), apiBase: api.URL, log: io.Discard, ctx: ctx, language: "en", phase: "options"}
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
	})
	manifest := waitPage("/connect/", "GitHub App 등록")
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
	waitPage("/", "https://github.com/apps/runquill-test/installations/new")
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
	request(http.MethodPost, "/deploy", server.URL, nil)
	progress := waitPage("/status", "[redacted]")
	if !strings.Contains(progress, `"phase":"deploying"`) || !strings.Contains(progress, `"stage":"images"`) ||
		!strings.Contains(progress, "Building fixture runner") || strings.Contains(progress, "example-secret") {
		t.Fatal("deployment did not expose live sanitized output")
	}
	if err := os.WriteFile(filepath.Join(dir, "docker.log.release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitPage("/", "운영 패널 열기")
	if _, body := request(http.MethodGet, "/first-job.yml", "", nil); body != string(firstWorkflow) {
		t.Fatal("wizard served a different first-job workflow")
	}
	deploy, err := os.ReadFile(filepath.Join(dir, "docker.log"))
	if err != nil || !strings.Contains(string(deploy), "compose -f compose.yaml -f compose.panel.yaml up -d --build") ||
		!strings.Contains(string(deploy), "exec -T manager runquill doctor") {
		t.Fatal("wizard did not invoke deployment followed by doctor")
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

func TestWizardRestoresOnlyRoutesChangedDuringFailedApply(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := filepath.Join(dir, "serve-state")
	t.Setenv("RUNQUILL_TEST_SERVE_STATE", state)
	script := `#!/bin/sh
case "$1:$2" in
  status:--json)
    printf '%s\n' '{"BackendState":"Running","Self":{"DNSName":"runquill.example.ts.net."},"CurrentTailnet":{"MagicDNSEnabled":true}}' ;;
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
		if err := network.apply(t.Context(), true); err == nil || !strings.Contains(err.Error(), "funnel disabled") {
			t.Fatalf("Funnel failure was not surfaced: %v", err)
		}
		result, err := os.ReadFile(state)
		if err != nil || string(result) != initial {
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
	script += "  *'get namespace'*) test -z \"${RUNQUILL_WIZARD_NAMESPACE_EXISTS-}\" || printf '%s\\n' namespace/runquill-system ;;\n"
	script += "esac\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	web := &setupWizard{dir: dir, kubeContext: "fixture", log: io.Discard}
	t.Setenv("RUNQUILL_WIZARD_NAMESPACE_EXISTS", "1")
	if err := web.applyK3s(t.Context()); err == nil {
		t.Fatal("wizard accepted an existing Runquill namespace")
	}
	t.Setenv("RUNQUILL_WIZARD_NAMESPACE_EXISTS", "")
	if err := web.applyK3s(t.Context()); err != nil {
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
		"apply -f runquill-k3s.yaml", "create secret generic runquill-config --from-env-file=.env",
		"create secret generic runquill-github-key --from-file=github-app.pem",
		"rollout status deployment/runquill", "exec deployment/runquill -- runquill doctor",
	} {
		if !strings.Contains(string(data), action) {
			t.Fatalf("missing deployment step: %s", action)
		}
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
		!strings.Contains(body, `<option value="selected" selected>`) || !strings.Contains(body, `<details class="advanced" open>`) {
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

	body = page(url.Values{"backend": {"docker"}, "panel_url": {"https://panel.example"}, "serve": {"on"}}, "en")
	if !strings.Contains(body, "Clear the panel origin when Tailscale Serve provides it.") || !invalid(body, "panel_url") ||
		!regexp.MustCompile(`name="serve" checked`).MatchString(body) {
		t.Fatal("a Tailscale Serve conflict was not shown next to the panel origin")
	}

	body = page(url.Values{"backend": {"k3s"}, "context": {"fixture"}}, "ko")
	if !strings.Contains(body, "runquill-k3s.yaml 파일이 없습니다") || strings.Contains(body, `aria-invalid="true"`) ||
		!strings.Contains(body, `value="fixture"`) || errorText(body) != 1 || !strings.Contains(body, `<p class="notice" data-state="failed" role="alert" id="wizard-error">`) {
		t.Fatal("a missing K3s manifest was not explained in the browser")
	}

	web.form, web.issue, web.failed = wizardForm{}, nil, false
	rec := httptest.NewRecorder()
	web.render(rec, httptest.NewRequest(http.MethodGet, "/?lang=en", nil))
	if strings.Contains(rec.Body.String(), `id="wizard-error"`) || strings.Contains(rec.Body.String(), `<details class="advanced" open>`) {
		t.Fatal("a fresh first step must not show an earlier problem")
	}
}
