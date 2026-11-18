package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const wizardCookie = "runquill_setup"

type setupWizardKey struct{}

type setupWizard struct {
	origin, key              string
	dir                      string
	client                   *http.Client
	apiBase                  string
	log                      io.Writer
	ctx                      context.Context
	mu                       sync.Mutex
	step, installURL         string
	handler                  http.Handler
	language, phase, message string
	options                  setupOptions
	network                  wizardNetwork
	kubeContext              string
	kubeAccepted             bool
	networkEnabled           bool
}

func currentWizard(ctx context.Context) *setupWizard {
	web, _ := ctx.Value(setupWizardKey{}).(*setupWizard)
	return web
}

func wizardKorean(ctx context.Context) bool {
	web := currentWizard(ctx)
	if web == nil {
		return false
	}
	web.mu.Lock()
	defer web.mu.Unlock()
	return web.language == "ko"
}

func wizardStep[T any](ctx context.Context, web *setupWizard, prefix string, handler http.Handler, done <-chan T) (T, error) {
	web.mu.Lock()
	web.step, web.handler = prefix, http.StripPrefix(prefix, handler)
	web.installURL = ""
	web.mu.Unlock()
	defer func() {
		web.mu.Lock()
		web.step, web.handler = "", nil
		web.mu.Unlock()
	}()
	select {
	case res := <-done:
		return res, nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (web *setupWizard) setInstallURL(installURL string) {
	web.mu.Lock()
	defer web.mu.Unlock()
	web.installURL = installURL
}

var wizardHome = template.Must(template.New("wizard").Parse(`
<!DOCTYPE html>
<html lang="{{.Language}}">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
{{if or (eq .Phase "working") (eq .Phase "deploying")}}<meta http-equiv="refresh" content="4">{{end}}
<title>Runquill · {{if .KO}}설치 마법사{{else}}Setup wizard{{end}}</title>
<link rel="stylesheet" href="/wizard.css">
<main>
  <header><span>runquill</span><a href="/?lang={{if .KO}}en{{else}}ko{{end}}">{{if .KO}}English{{else}}한국어{{end}}</a></header>
  <h1>{{if .KO}}Runquill 시작하기{{else}}Set up Runquill{{end}}</h1>
  <nav aria-label="Setup progress">
    <span>{{if .KO}}1. 설치 범위{{else}}1. Options{{end}}</span><span>{{if .KO}}2. GitHub 연결{{else}}2. GitHub{{end}}</span><span>{{if .KO}}3. 배포 및 첫 작업{{else}}3. Deploy and verify{{end}}</span>
  </nav>
  {{if .Error}}<p class="notice" role="alert">{{.Error}}</p>{{end}}
  {{if eq .Phase "options"}}
  <form method="post" action="/start">
    <label>{{if .KO}}설치 환경{{else}}Deployment target{{end}}<select name="backend"><option value="docker">Docker Compose</option><option value="k3s">K3s</option></select></label>
    <label>{{if .KO}}k3s 컨텍스트 이름{{else}}K3s context name{{end}} <input name="context" maxlength="200" spellcheck="false" placeholder="kubectl context"></label>
    <label>{{if .KO}}GitHub 조직 이름 (개인 계정은 비워 두기){{else}}GitHub organization (leave empty for a personal account){{end}}<input name="organization" maxlength="39" spellcheck="false"></label>
    <label>{{if .KO}}조직 러너 그룹 ID (선택){{else}}Organization runner group ID (optional){{end}}<input name="group" inputmode="numeric" pattern="[0-9]*" maxlength="19"></label>
    <label>{{if .KO}}저장소 연결 범위{{else}}Repository access{{end}}<select name="repositories"><option value="all">{{if .KO}}전체 저장소{{else}}All repositories{{end}}</option><option value="selected">{{if .KO}}선택한 저장소{{else}}Selected repositories{{end}}</option></select></label>
    <label>{{if .KO}}패널 주소 (기본값은 이 서버의 8081 포트){{else}}Panel origin (defaults to loopback port 8081){{end}}<input name="panel_url" type="url" placeholder="http://127.0.0.1:8081"></label>
    <label>{{if .KO}}외부 웹훅 주소 (선택){{else}}External webhook address (optional){{end}}<input name="webhook_url" type="url" placeholder="https://example.com/webhook"></label>
    <fieldset><legend>Tailscale</legend>
      <p>{{if .KO}}Compose 호스트에서 로그인된 Tailscale을 사용할 수 있습니다. 기본 연결은 GitHub 폴링입니다.{{else}}Docker Compose can use the host’s signed-in Tailscale client. GitHub polling is the default.{{end}}</p>
      <label class="check"><input type="checkbox" name="serve">{{if .KO}}Serve로 패널을 tailnet에 연결 (9443){{else}}Serve the panel privately on the tailnet (9443){{end}}</label>
      <label class="check"><input type="checkbox" name="funnel">{{if .KO}}Funnel로 GitHub 웹훅 경로만 공개 (8443){{else}}Expose only the GitHub webhook path through Funnel (8443){{end}}</label>
    </fieldset>
    <input type="hidden" name="lang" value="{{.Language}}">
    <button>{{if .KO}}GitHub 연결 시작{{else}}Connect GitHub{{end}}</button>
  </form>
  {{else if eq .Phase "working"}}
    {{if .Step}}<p><a class="button" href="{{.Step}}/">{{if .KO}}현재 단계 열기{{else}}Open the current step{{end}}</a></p>{{else if .Install}}<p><a class="button" href="{{.Install}}" rel="noreferrer" target="_blank">{{if .KO}}GitHub에 앱 설치{{else}}Install the GitHub App{{end}}</a></p><p>{{if .KO}}GitHub에서 설치를 마치면 다음 단계가 여기에 표시됩니다.{{else}}Complete installation on GitHub. The next step will appear here.{{end}}</p>{{else}}<p role="status">{{if .KO}}상태를 확인하고 있습니다…{{else}}Checking setup status…{{end}}</p>{{end}}
  {{else if eq .Phase "ready"}}
    <p>{{if .KO}}GitHub App과 패널 설정을 저장했습니다. 아래 대상에 Runquill을 설치합니다.{{else}}GitHub App and panel settings are saved. Start Runquill on the following target.{{end}}</p>
    <p><strong>{{.Target}}</strong></p>
    {{if .Serve}}<p>{{if .KO}}패널은 Tailscale Serve로 연결됩니다.{{else}}The panel will be available through Tailscale Serve.{{end}}</p>{{end}}
    {{if .Funnel}}<p class="notice">{{if .KO}}배포하면 GitHub가 접근할 수 있도록 웹훅 경로가 인터넷에 공개됩니다.{{else}}Deploying publishes the webhook path to the internet so GitHub can reach it.{{end}}</p>{{end}}
    <form method="post" action="/deploy"><button>{{if .KO}}배포 및 사전 점검{{else}}Deploy and check installation{{end}}</button></form>
  {{else if eq .Phase "deploying"}}
    <p role="status">{{if .KO}}매니저를 시작하고 있습니다.{{else}}Starting the manager.{{end}} <a href="/">{{if .KO}}상태 확인{{else}}Check status{{end}}</a></p>
  {{else if eq .Phase "done"}}
    <p>{{if .KO}}매니저 배포와 사전 점검을 마쳤습니다.{{else}}The manager is deployed and preflight checks passed.{{end}}</p>
    {{if .PanelURL}}<p><a class="button" href="{{.PanelURL}}">{{if .KO}}운영 패널 열기{{else}}Open the operations panel{{end}}</a></p>{{end}}
    {{if .K3s}}<p>{{if .KO}}로컬 패널에 접속하려면 선택한 컨텍스트에서 포트 포워딩을 실행하세요.{{else}}To reach a loopback panel, forward the port in the selected context.{{end}} <code>kubectl -n runquill-system port-forward svc/runquill 8081:8081</code></p>{{end}}
    {{if or .Serve .Funnel}}<fieldset><legend>Tailscale</legend>
      <p>{{if .NetworkEnabled}}{{if .KO}}이 설치의 Tailscale 경로가 연결되어 있습니다.{{else}}Tailscale routes for this installation are connected.{{end}}{{else}}{{if .KO}}이 설치의 Tailscale 경로가 중지되어 있습니다.{{else}}Tailscale routes for this installation are paused.{{end}}{{end}}</p>
      <form action="/tailscale" method="post">
        <button>{{if .NetworkEnabled}}{{if .KO}}Tailscale 연결 중지{{else}}Pause Tailscale routes{{end}}{{else}}{{if .KO}}Tailscale 연결 복구{{else}}Resume Tailscale routes{{end}}{{end}}</button>
      </form>
    </fieldset>{{end}}
    <h2>{{if .KO}}첫 작업 확인{{else}}Verify your first job{{end}}</h2>
    <p>{{if .KO}}아래 워크플로 파일을 비공개 저장소의 .github/workflows/에 추가하고, GitHub Actions에서 실행하세요. 패널에서 러너 생성과 정리를 확인할 수 있습니다.{{else}}Add this workflow to .github/workflows/ in a private repository and run it from GitHub Actions. The panel shows discovery and cleanup.{{end}}</p>
    <a href="/first-job.yml" download="runquill-first-job.yml">{{if .KO}}첫 작업 워크플로 다운로드{{else}}Download the first-job workflow{{end}}</a>
  {{end}}
</main>
</html>`))

func wizardCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("runquill wizard", flag.ContinueOnError)
	port := flags.Uint("listen-port", 0, "loopback port (0 selects a free port)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *port > 65535 {
		return fmt.Errorf("use runquill wizard [--listen-port PORT]")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	return runWizard(ctx, os.Stdout, dir, &http.Client{Timeout: 10 * time.Second}, githubAPIBase, *port)
}

func runWizard(ctx context.Context, out io.Writer, dir string, client *http.Client, apiBase string, port uint) error {
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10)))
	if err != nil {
		return err
	}
	defer listener.Close()
	web := &setupWizard{
		origin: "http://" + listener.Addr().String(), key: rand.Text(), dir: dir,
		client: client, apiBase: apiBase, log: out, ctx: ctx, language: "en", phase: "options",
	}
	server := &http.Server{Handler: web, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 20 * time.Second, MaxHeaderBytes: 16 << 10}
	fmt.Fprintf(out, "Open %s/?key=%s in your browser.\n", web.origin, web.key)
	fmt.Fprintf(out, "On a remote server, forward port %d over SSH before opening the URL.\n", listener.Addr().(*net.TCPAddr).Port)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func (web *setupWizard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self' https://github.com; frame-ancestors 'none'; base-uri 'none'")
	if "http://"+r.Host != web.origin {
		http.Error(w, "Use the configured loopback address.", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" && wizardEquals(r.URL.Query().Get("key"), web.key) {
		http.SetCookie(w, &http.Cookie{Name: wizardCookie, Value: web.key, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 3600})
		destination := "/"
		if lang := r.URL.Query().Get("lang"); lang == "ko" || lang == "en" {
			destination += "?lang=" + lang
		}
		http.Redirect(w, r, destination, http.StatusSeeOther)
		return
	}
	cookie, err := r.Cookie(wizardCookie)
	if err != nil || !wizardEquals(cookie.Value, web.key) {
		http.Error(w, "Open the private setup link printed by Runquill.", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("Origin") != web.origin {
		http.Error(w, "Invalid setup origin.", http.StatusForbidden)
		return
	}
	web.mu.Lock()
	step, handler := web.step, web.handler
	web.mu.Unlock()
	if handler != nil && strings.HasPrefix(r.URL.Path, step+"/") {
		handler.ServeHTTP(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/connect/" || r.URL.Path == "/oauth/"):
		web.render(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/wizard.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		fmt.Fprint(w, wizardCSS)
	case r.Method == http.MethodPost && r.URL.Path == "/start":
		web.start(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/deploy":
		web.deploy(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/tailscale":
		web.toggleNetwork(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/first-job.yml":
		web.mu.Lock()
		isDone := web.phase == "done"
		web.mu.Unlock()
		if !isDone {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		fmt.Fprint(w, string(firstWorkflow))
	default:
		http.NotFound(w, r)
	}
}

func wizardEquals(received, want string) bool {
	return subtle.ConstantTimeCompare([]byte(received), []byte(want)) == 1
}

func (web *setupWizard) render(w http.ResponseWriter, r *http.Request) {
	web.mu.Lock()
	language := web.language
	if requested := r.URL.Query().Get("lang"); requested == "en" || requested == "ko" {
		language = requested
	} else if saved, err := r.Cookie("runquill_language"); err == nil && (saved.Value == "ko" || saved.Value == "en") {
		language = saved.Value
	} else if web.phase == "options" && strings.HasPrefix(strings.ToLower(r.Header.Get("Accept-Language")), "ko") {
		language = "ko"
	}
	http.SetCookie(w, &http.Cookie{Name: "runquill_language", Value: language, Path: "/", SameSite: http.SameSiteLaxMode, MaxAge: 3600})
	view := struct {
		Language, Phase, Error, Step, Install, Target, PanelURL string
		KO, K3s, Serve, Funnel, NetworkEnabled                  bool
	}{language, web.phase, web.message, web.step, web.installURL, web.kubeContext, web.panelLink(language), language == "ko", web.options.kubernetes, web.network.serve, web.network.funnel, web.networkEnabled}
	web.mu.Unlock()
	if !view.K3s {
		view.Target = "Docker Compose"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = wizardHome.Execute(w, view)
}

func (web *setupWizard) panelLink(language string) string {
	origin := web.options.panelURL
	if origin == "" {
		origin = defaultSetupPanelURL
	}
	parsed, err := parsePanelURL(origin)
	if err != nil {
		return ""
	}
	return parsed.String() + "/?lang=" + language
}

func (web *setupWizard) start(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid setup options.", http.StatusBadRequest)
		return
	}
	web.mu.Lock()
	if web.phase != "options" {
		web.mu.Unlock()
		http.Error(w, "Setup is already in progress.", http.StatusConflict)
		return
	}
	web.phase, web.message = "working", ""
	if r.PostForm.Get("lang") == "ko" {
		web.language = "ko"
	} else {
		web.language = "en"
	}
	form := r.PostForm
	web.mu.Unlock()
	go web.configure(form)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (web *setupWizard) setResult(phase string, err error) {
	web.mu.Lock()
	defer web.mu.Unlock()
	web.phase, web.message = phase, ""
	if err != nil {
		fmt.Fprintln(web.log, err)
		web.message = "Setup needs attention. Check the Runquill terminal for details."
		if web.language == "ko" {
			web.message = "설정 중 문제가 발생했습니다. Runquill 터미널에서 원인을 확인하세요."
		}
	}
}

func (web *setupWizard) configure(form url.Values) {
	opts := setupOptions{
		panel: true, kubernetes: form.Get("backend") == "k3s", organization: strings.TrimSpace(form.Get("organization")),
		repositoryScope: form.Get("repositories"), panelURL: strings.TrimSpace(form.Get("panel_url")),
		webhookURL: strings.TrimSpace(form.Get("webhook_url")),
	}
	if raw := form.Get("group"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			web.setResult("options", fmt.Errorf("enter a positive organization runner group ID"))
			return
		}
		opts.runnerGroupID = value
	}
	kubeContext := strings.TrimSpace(form.Get("context"))
	if opts.kubernetes && (kubeContext == "" || strings.HasPrefix(kubeContext, "-") || len(kubeContext) > 200 || strings.ContainsAny(kubeContext, "\r\n")) {
		web.setResult("options", fmt.Errorf("enter an explicit K3s context"))
		return
	}
	if opts.kubernetes {
		if _, err := os.Stat(filepath.Join(web.dir, "runquill-k3s.yaml")); err != nil {
			web.setResult("options", fmt.Errorf("put the matching Runquill release's runquill-k3s.yaml in the installation directory: %w", err))
			return
		}
	}
	network := wizardNetwork{serve: form.Get("serve") == "on", funnel: form.Get("funnel") == "on"}
	if err := network.configure(web.ctx, &opts); err != nil {
		web.setResult("options", err)
		return
	}
	web.mu.Lock()
	web.options, web.network, web.kubeContext = opts, network, kubeContext
	web.mu.Unlock()
	ctx := context.WithValue(web.ctx, setupWizardKey{}, web)
	if err := setup(ctx, web.log, web.dir, web.client, web.apiBase, opts); err != nil {
		web.setResult("options", err)
		return
	}
	data, err := os.ReadFile(filepath.Join(web.dir, setupEnvFile))
	if err != nil {
		web.setResult("options", err)
		return
	}
	values, err := setupValues(data)
	if err != nil {
		web.setResult("options", err)
		return
	}
	web.mu.Lock()
	web.options.panelURL = values["RUNQUILL_PANEL_URL"]
	web.mu.Unlock()
	web.setResult("ready", nil)
}

func (web *setupWizard) deploy(w http.ResponseWriter, r *http.Request) {
	web.mu.Lock()
	if web.phase != "ready" {
		web.mu.Unlock()
		http.Error(w, "Deployment is unavailable in this step.", http.StatusConflict)
		return
	}
	web.phase, web.message = "deploying", ""
	web.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(web.ctx, 8*time.Minute)
		defer cancel()
		if err := web.apply(ctx); err != nil {
			web.setResult("ready", err)
		} else {
			web.mu.Lock()
			web.networkEnabled = web.network.host != ""
			web.mu.Unlock()
			web.setResult("done", nil)
		}
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (web *setupWizard) toggleNetwork(w http.ResponseWriter, r *http.Request) {
	web.mu.Lock()
	if web.phase != "done" || web.network.host == "" {
		web.mu.Unlock()
		http.Error(w, "Network settings are unavailable in this step.", http.StatusConflict)
		return
	}
	enabled := !web.networkEnabled
	web.phase, web.message = "deploying", ""
	web.mu.Unlock()
	go func() {
		err := web.network.apply(web.ctx, enabled)
		if err == nil {
			web.mu.Lock()
			web.networkEnabled = enabled
			web.mu.Unlock()
		}
		web.setResult("done", err)
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

const wizardCSS = `
:root{color-scheme:light dark;font:16px/1.55 system-ui,sans-serif;color:#1c2623;background:#f6f6f1}
body{margin:0}main{width:min(620px,calc(100% - 32px));margin:2rem auto 5rem}
header,nav{display:flex;gap:1rem;justify-content:space-between;flex-wrap:wrap}header{border-bottom:1px solid #9ca6a0;padding-bottom:1rem;font-weight:700}
h1{font-size:2.1rem;line-height:1.15;margin:1.6rem 0 1rem}nav{color:#405a50;border-bottom:1px solid #c0c9c0;padding-bottom:1rem;margin-bottom:1.3rem;font-size:.9rem}
label{display:flex;flex-direction:column;gap:.35rem;font-size:.92rem;margin-bottom:.85rem}.check{align-items:center;flex-direction:row;gap:.55rem}
input,select,button,.button{font:inherit;padding:.55rem .7rem;border-radius:5px;border:1px solid #85968e}
input,select{width:100%;box-sizing:border-box;background:Canvas;color:CanvasText}input[type=checkbox]{width:auto}
button,.button{width:fit-content;background:#225849;color:#fff;cursor:pointer;display:inline-block;text-decoration:none;border-color:#225849}
fieldset{border:1px solid #9ca6a0;margin:1.1rem 0;padding:.6rem 1rem}fieldset p{font-size:.9rem;margin:0 0 .8rem}
a{color:#225849}code{overflow-wrap:anywhere}.notice{border-inline-start:3px solid #9a6b22;padding:.5rem .8rem;background:#f8edda}
@media(prefers-color-scheme:dark){:root{background:#17211e;color:#f2f4ed}a,nav{color:#abe0c9}.notice{background:#43361d}}
`
