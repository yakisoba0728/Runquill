package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const wizardCookie = "runquill_setup"

type setupWizardKey struct{}

type setupWizard struct {
	origin, key      string
	dir              string
	client           *http.Client
	apiBase          string
	log              io.Writer
	ctx              context.Context
	mu               sync.Mutex
	step, installURL string
	handler          http.Handler
	language, phase  string
	failed           bool
	options          setupOptions
	network          wizardNetwork
	kubeContext      string
	kubeAccepted     bool
	networkEnabled   bool
	checkCompose     bool
	toggling         bool // a Tailscale pause or resume is running; only the page title and stage list use it
	progress         wizardProgress
	form             wizardForm
	issue            *wizardIssue
}

// wizardIssue is a first-step problem the browser can explain beside the field that caused it.
type wizardIssue struct {
	field, ko, en string
	cause         error
}

func (issue *wizardIssue) Error() string {
	if issue.cause != nil {
		return issue.en + " (" + issue.cause.Error() + ")"
	}
	return issue.en
}

func (issue *wizardIssue) Unwrap() error { return issue.cause }

func newWizardIssue(field, ko, en string) error { return &wizardIssue{field: field, ko: ko, en: en} }

// wizardForm keeps the first-step answers so a rejected submission can be corrected in place.
type wizardForm struct {
	Backend, Repositories, Context, Organization, Group, PanelURL, WebhookURL string
	Serve, Funnel                                                             bool
}

var wizardAdvancedFields = []string{"organization", "group", "panel_url", "webhook_url", "serve", "funnel"}

func wizardFormValues(form url.Values) wizardForm {
	return wizardForm{
		Backend: form.Get("backend"), Repositories: form.Get("repositories"), Context: form.Get("context"),
		Organization: form.Get("organization"), Group: form.Get("group"),
		PanelURL: wizardVisibleURL(form.Get("panel_url")), WebhookURL: wizardVisibleURL(form.Get("webhook_url")),
		Serve: form.Get("serve") == "on", Funnel: form.Get("funnel") == "on",
	}
}

// wizardVisibleURL drops an address that embeds credentials instead of echoing them into the page.
func wizardVisibleURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil {
		return ""
	}
	return raw
}

func (form wizardForm) advanced() bool {
	return form.Organization != "" || form.Group != "" || form.PanelURL != "" || form.WebhookURL != "" || form.Serve || form.Funnel
}

//go:embed panelweb/wizard.css
var wizardCSS string

//go:embed panelweb/wizard.js
var wizardJS string

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

// wizardMark is one row of the step list or the deployment stage list.
type wizardMark struct {
	Key, Label, Detail, State, StateText string
}

func wizardStateText(ko bool, state string) string {
	labels := map[string][2]string{
		"ok": {"완료", "done"}, "current": {"진행 중", "in progress"}, "pending": {"대기", "pending"}, "failed": {"실패", "failed"},
	}
	if ko {
		return labels[state][0]
	}
	return labels[state][1]
}

// wizardSteps lays out the four setup steps for the current phase.
func wizardSteps(ko bool, phase, step string, install, failed bool) []wizardMark {
	names := [][2]string{{"설치 옵션", "Options"}, {"GitHub 연결", "GitHub"}, {"배포", "Deploy"}, {"첫 작업", "First job"}}
	current := map[string]int{"options": 0, "working": 1, "ready": 2, "deploying": 2, "done": 3}[phase]
	marks := make([]wizardMark, len(names))
	for i, name := range names {
		mark := wizardMark{Label: name[1], State: "pending"}
		if ko {
			mark.Label = name[0]
		}
		switch {
		case i < current:
			mark.State = "ok"
		case i == current && failed && (phase == "options" || phase == "ready"):
			mark.State = "failed"
		case i == current:
			mark.State = "current"
		}
		if mark.State == "failed" {
			mark.Detail = wizardStateText(ko, "failed")
		} else if i == 1 && phase == "working" {
			detail := [2]string{"연결 확인", "checking"}
			switch {
			case step == "/connect":
				detail = [2]string{"앱 등록", "App registration"}
			case step == "/oauth":
				detail = [2]string{"패널 로그인", "panel sign-in"}
			case install:
				detail = [2]string{"앱 설치", "App installation"}
			}
			mark.Detail = detail[1]
			if ko {
				mark.Detail = detail[0]
			}
		}
		if mark.State == "ok" || mark.State == "pending" {
			mark.StateText = wizardStateText(ko, mark.State)
		}
		marks[i] = mark
	}
	return marks
}

// wizardStageNames label the deployment stages wizard_deploy.go reports.
var wizardStageNames = map[string][2]string{
	"images": {"러너 이미지 준비", "Runner images"}, "resources": {"K3s 리소스 적용", "K3s resources"},
	"manager": {"매니저 시작", "Manager start"}, "network": {"Tailscale 경로 적용", "Tailscale routes"},
	"check": {"서버·GitHub 점검", "Server and GitHub check"},
}

// wizardActivity is the live status sentence for a stage; wizard.js keeps the same sentences.
var wizardActivity = map[string][2]string{
	"images": {"러너 이미지를 준비하고 있습니다.", "Preparing runner images."}, "resources": {"K3s 리소스를 적용하고 있습니다.", "Applying K3s resources."},
	"manager": {"매니저를 시작하고 있습니다.", "Starting the manager."}, "network": {"Tailscale 경로를 적용하고 있습니다.", "Applying Tailscale routes."},
	"check": {"서버와 GitHub 연결을 점검하고 있습니다.", "Checking the server and GitHub connection."},
	"":      {"배포를 준비하고 있습니다.", "Preparing the deployment."},
}

// wizardStages lists, in order, the stages the next or running deployment goes through.
func wizardStages(ko, k3s, checkCompose, network, toggling bool, phase, stage string, failed bool) []wizardMark {
	keys := []string{"images", "manager", "check"}
	switch {
	case toggling:
		keys = []string{"network"}
	case checkCompose:
		keys = []string{"check"}
	case k3s:
		keys = []string{"resources", "manager", "check"}
	case network:
		keys = []string{"images", "manager", "network", "check"}
	}
	at := -1
	if phase == "deploying" || phase == "ready" && failed {
		at = slices.Index(keys, stage)
	}
	marks := make([]wizardMark, len(keys))
	for i, key := range keys {
		mark := wizardMark{Key: key, Label: wizardStageNames[key][1], State: "pending"}
		if ko {
			mark.Label = wizardStageNames[key][0]
		}
		switch {
		case phase == "done" || at >= 0 && i < at:
			mark.State = "ok"
		case i == at && phase == "deploying":
			mark.State = "current"
		case i == at:
			mark.State = "failed"
		}
		mark.StateText = wizardStateText(ko, mark.State)
		marks[i] = mark
	}
	return marks
}

var wizardHome = template.Must(template.New("wizard").Parse(`<!doctype html>
<html lang="{{.Language}}">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Runquill · {{if .KO}}설치 마법사{{else}}Setup wizard{{end}}</title>
  <link rel="stylesheet" href="/wizard.css">
  <script src="/wizard.js"></script>
  {{if or (eq .Phase "working") (eq .Phase "deploying")}}<noscript><meta http-equiv="refresh" content="4"></noscript>{{end}}
</head>
<body data-phase="{{.Phase}}" data-step="{{.Step}}" data-install="{{if .Install}}true{{else}}false{{end}}" data-stage="{{.Stage}}">
  <header class="topbar">
    <span class="brand">runquill</span>
    <span class="topbar-title">{{if .KO}}설치 마법사{{else}}Setup wizard{{end}}</span>
    <div class="topbar-controls">
      <a href="/?lang={{if .KO}}en{{else}}ko{{end}}" lang="{{if .KO}}en{{else}}ko{{end}}">{{if .KO}}English{{else}}한국어{{end}}</a>
      <label data-theme-field hidden><span class="visually-hidden">{{if .KO}}화면 테마{{else}}Appearance{{end}}</span>
        <select data-theme-control><option value="system">{{if .KO}}시스템{{else}}System{{end}}</option><option value="light">{{if .KO}}밝게{{else}}Light{{end}}</option><option value="dark">{{if .KO}}어둡게{{else}}Dark{{end}}</option></select>
      </label>
    </div>
  </header>
  <main class="wizard">
    <ol class="steps" aria-label="{{if .KO}}설치 단계{{else}}Setup steps{{end}}">
      {{- range .Steps}}
      <li data-state="{{.State}}"{{if eq .State "current"}} aria-current="step"{{end}}>{{.Label}}{{if .Detail}} <span class="detail">· {{.Detail}}</span>{{end}}{{if .StateText}}<span class="visually-hidden"> ({{.StateText}})</span>{{end}}</li>
      {{- end}}
    </ol>
    <p class="notice" data-state="waiting" id="wizard-connection" role="status" data-reconnecting="{{if .KO}}설치 마법사에 다시 연결하고 있습니다…{{else}}Reconnecting to the setup wizard…{{end}}" data-ended="{{if .KO}}설치 세션이 끝났습니다. 새 설치 링크를 여세요.{{else}}This setup session ended. Open a new setup link.{{end}}"></p>
    {{- if and .Error (not .Field)}}
    <p class="notice" data-state="failed" role="alert" id="wizard-error"><strong>{{if .KO}}오류:{{else}}Error:{{end}}</strong>
      {{if .Problem}}{{.Problem}}
      {{- else if eq .Phase "ready"}}{{if .KO}}배포 점검을 통과하지 못했습니다. 아래 로그에서 원인을 확인하세요.{{else}}Deployment checks failed. See the log below.{{end}}
      {{- else}}{{if .KO}}설정 중 문제가 발생했습니다. Runquill 터미널에서 원인을 확인하세요.{{else}}Setup needs attention. Check the Runquill terminal for details.{{end}}{{end}}</p>
    {{- end}}
    {{- if eq .Phase "options"}}
    <h1>{{if .KO}}Runquill 시작하기{{else}}Set up Runquill{{end}}</h1>
    <form method="post" action="/start">
      <div class="form-row">
        <div class="field">
          <label for="wizard-backend">{{if .KO}}배포 대상{{else}}Deployment target{{end}}</label>
          <select id="wizard-backend" name="backend"{{if eq .Field "backend"}} aria-invalid="true" aria-describedby="wizard-error" autofocus{{end}}><option value="docker">Docker Compose</option><option value="k3s"{{if eq .Form.Backend "k3s"}} selected{{end}}>K3s</option></select>
          {{- if eq .Field "backend"}}{{template "field-error" .}}{{end}}
        </div>
        <div class="field">
          <label for="wizard-repositories">{{if .KO}}저장소 접근 범위{{else}}Repository access{{end}}</label>
          <select id="wizard-repositories" name="repositories" aria-describedby="help-repositories"><option value="all">{{if .KO}}전체 저장소{{else}}All repositories{{end}}</option><option value="selected"{{if eq .Form.Repositories "selected"}} selected{{end}}>{{if .KO}}선택한 저장소{{else}}Selected repositories{{end}}</option></select>
          <p class="hint" id="help-repositories">{{if .KO}}GitHub에서 앱을 설치할 때도 같은 범위를 선택하세요.{{else}}Choose the same access when you install the App on GitHub.{{end}}</p>
        </div>
      </div>
      <div class="field" data-k3s{{if ne .Form.Backend "k3s"}} hidden{{end}}>
        <label for="wizard-context">{{if .KO}}K3s 컨텍스트 이름{{else}}K3s context name{{end}}</label>
        <input id="wizard-context" name="context" maxlength="200" spellcheck="false" value="{{.Form.Context}}"{{if eq .Form.Backend "k3s"}} required{{end}}{{if eq .Field "context"}} aria-invalid="true" aria-describedby="wizard-error help-context" autofocus{{else}} aria-describedby="help-context"{{end}}>
        {{- if eq .Field "context"}}{{template "field-error" .}}{{end}}
        <p class="hint" id="help-context">{{if .KO}}kubectl config get-contexts에 표시되는 이름입니다.{{else}}As listed by kubectl config get-contexts.{{end}}</p>
      </div>
      <details class="advanced"{{if .Advanced}} open{{end}}>
        <summary>{{if .KO}}조직 및 네트워크{{else}}Organization and network{{end}}</summary>
        <div class="advanced-body">
          <div class="form-row">
            <div class="field">
              <label for="wizard-organization">{{if .KO}}GitHub 조직 (선택){{else}}GitHub organization (optional){{end}}</label>
              <input id="wizard-organization" name="organization" maxlength="39" spellcheck="false" value="{{.Form.Organization}}"{{if eq .Field "organization"}} aria-invalid="true" aria-describedby="wizard-error help-organization" autofocus{{else}} aria-describedby="help-organization"{{end}}>
              {{- if eq .Field "organization"}}{{template "field-error" .}}{{end}}
              <p class="hint" id="help-organization">{{if .KO}}비워 두면 개인 계정에 앱을 등록합니다.{{else}}Leave empty to register the App on your personal account.{{end}}</p>
            </div>
            <div class="field">
              <label for="wizard-group">{{if .KO}}러너 그룹 ID (선택){{else}}Runner group ID (optional){{end}}</label>
              <input id="wizard-group" name="group" inputmode="numeric" pattern="[0-9]*" maxlength="19" value="{{.Form.Group}}"{{if eq .Field "group"}} aria-invalid="true" aria-describedby="wizard-error help-group" autofocus{{else}} aria-describedby="help-group"{{end}}>
              {{- if eq .Field "group"}}{{template "field-error" .}}{{end}}
              <p class="hint" id="help-group">{{if .KO}}조직 러너 그룹에 러너를 등록할 때만 입력하세요.{{else}}Only for an organization runner group.{{end}}</p>
            </div>
          </div>
          <div class="field">
            <label for="wizard-panel-url">{{if .KO}}패널 주소 (선택){{else}}Panel origin (optional){{end}}</label>
            <input id="wizard-panel-url" name="panel_url" type="url" placeholder="http://127.0.0.1:8081" value="{{.Form.PanelURL}}"{{if eq .Field "panel_url"}} aria-invalid="true" aria-describedby="wizard-error help-panel-url" autofocus{{else}} aria-describedby="help-panel-url"{{end}}>
            {{- if eq .Field "panel_url"}}{{template "field-error" .}}{{end}}
            <p class="hint" id="help-panel-url">{{if .KO}}비워 두면 http://127.0.0.1:8081을 씁니다.{{else}}Defaults to http://127.0.0.1:8081.{{end}}</p>
          </div>
          <div class="field">
            <label for="wizard-webhook-url">{{if .KO}}공개 웹훅 URL (선택){{else}}Public webhook URL (optional){{end}}</label>
            <input id="wizard-webhook-url" name="webhook_url" type="url" placeholder="https://example.com/webhook" value="{{.Form.WebhookURL}}"{{if eq .Field "webhook_url"}} aria-invalid="true" aria-describedby="wizard-error help-webhook-url" autofocus{{else}} aria-describedby="help-webhook-url"{{end}}>
            {{- if eq .Field "webhook_url"}}{{template "field-error" .}}{{end}}
            <p class="hint" id="help-webhook-url">{{if .KO}}비워 두면 Runquill이 GitHub를 폴링해 작업을 찾습니다.{{else}}Leave empty to discover jobs by polling GitHub.{{end}}</p>
          </div>
          <fieldset aria-describedby="help-tailscale">
            <legend>Tailscale</legend>
            <label class="check"><input type="checkbox" name="serve"{{if .Form.Serve}} checked{{end}}{{if eq .Field "serve"}} aria-invalid="true" aria-describedby="wizard-error" autofocus{{end}}>{{if .KO}}Serve로 패널을 tailnet에만 공개{{else}}Serve the panel privately on the tailnet{{end}}</label>
            <label class="check"><input type="checkbox" name="funnel"{{if .Form.Funnel}} checked{{end}}{{if eq .Field "funnel"}} aria-invalid="true" aria-describedby="wizard-error" autofocus{{end}}>{{if .KO}}Funnel로 웹훅 경로를 인터넷에 공개{{else}}Expose the webhook path through Funnel{{end}}</label>
            {{- if or (eq .Field "serve") (eq .Field "funnel")}}{{template "field-error" .}}{{end}}
            <p class="hint" id="help-tailscale">{{if .KO}}Docker Compose 설치에서만 쓸 수 있습니다. 이 호스트가 Tailscale에 연결되어 있고 MagicDNS가 켜져 있어야 합니다.{{else}}Docker Compose only. This host must be connected to Tailscale with MagicDNS enabled.{{end}}</p>
          </fieldset>
        </div>
      </details>
      <input type="hidden" name="lang" value="{{.Language}}">
      <div class="actions"><button class="button primary">{{if .KO}}GitHub 연결 시작{{else}}Connect GitHub{{end}}</button></div>
    </form>
    {{- else if eq .Phase "working"}}
    <h1>{{if .KO}}GitHub 연결{{else}}Connect GitHub{{end}}</h1>
    {{- if .Step}}
    <p class="muted">{{if eq .Step "/oauth"}}{{if .KO}}운영 패널의 로그인 설정을 입력합니다.{{else}}Enter the operations panel's sign-in settings.{{end}}{{else}}{{if .KO}}GitHub에 Runquill용 비공개 GitHub App을 등록합니다.{{else}}Register a private GitHub App for Runquill.{{end}}{{end}}</p>
    <div class="actions"><a class="button primary" href="{{.Step}}/">{{if .KO}}현재 단계 열기{{else}}Open the current step{{end}}</a></div>
    {{- else if .Install}}
    <p class="muted">{{if .KO}}GitHub에서 앱을 설치하면 이 화면이 다음 단계로 넘어갑니다.{{else}}This page advances after you install the app on GitHub.{{end}}</p>
    <div class="actions"><a class="button primary" href="{{.Install}}" rel="noreferrer" target="_blank">{{if .KO}}GitHub에 앱 설치{{else}}Install the GitHub App{{end}}</a></div>
    {{- else}}
    <p role="status">{{if .KO}}연결을 확인하고 있습니다.{{else}}Checking the connection.{{end}}</p>
    {{- end}}
    {{- else if eq .Phase "ready"}}
    <h1>{{if .CheckCompose}}{{if .KO}}배포 상태 확인{{else}}Check deployment{{end}}{{else}}{{if .KO}}Runquill 배포{{else}}Deploy Runquill{{end}}{{end}}</h1>
    {{- if not .Error}}
    <p class="muted">{{if .KO}}GitHub 연결을 마쳤습니다. 배포 대상을 확인하세요.{{else}}GitHub is connected. Review the deployment target.{{end}}</p>
    {{- end}}
    <dl class="kv">
      <div><dt>{{if .KO}}배포 대상{{else}}Deployment target{{end}}</dt><dd class="target">{{if .K3s}}K3s · <code>{{.Target}}</code>{{else}}{{.Target}}{{end}}</dd></div>
      {{- if .Serve}}
      <div><dt>{{if .KO}}패널{{else}}Panel{{end}}</dt><dd>{{if .KO}}Tailscale Serve로 tailnet에만 공개{{else}}Available through Tailscale Serve on the tailnet{{end}}</dd></div>
      {{- end}}
      {{- if .Funnel}}
      <div><dt>{{if .KO}}웹훅{{else}}Webhook{{end}}</dt><dd>{{if .KO}}Tailscale Funnel로 인터넷에 공개{{else}}Public through Tailscale Funnel{{end}}</dd></div>
      {{- end}}
    </dl>
    {{- if .Funnel}}
    <p class="notice" data-state="waiting">{{if .KO}}배포하면 웹훅 경로가 인터넷에 공개됩니다.{{else}}Deploying publishes the webhook path to the internet.{{end}}</p>
    {{- end}}
    {{template "stages" .}}
    <form method="post" action="/deploy"><div class="actions"><button class="button primary">{{if .CheckCompose}}{{if .KO}}사전 점검 다시 실행{{else}}Run preflight again{{end}}{{else}}{{if .KO}}배포 및 사전 점검{{else}}Deploy and check installation{{end}}{{end}}</button></div></form>
    {{- else if eq .Phase "deploying"}}
    <h1>{{if .Toggling}}{{if .KO}}Tailscale 경로 변경 중{{else}}Updating Tailscale routes{{end}}{{else if .CheckCompose}}{{if .KO}}배포 상태 확인 중{{else}}Checking deployment{{end}}{{else}}{{if .KO}}Runquill 배포 중{{else}}Deploying Runquill{{end}}{{end}}</h1>
    {{template "stages" .}}
    {{- else if eq .Phase "done"}}
    <h1>{{if .KO}}배포 완료{{else}}Deployment complete{{end}}</h1>
    <p class="muted">{{if .KO}}매니저가 실행 중이며 사전 점검을 통과했습니다.{{else}}The manager is running and preflight checks passed.{{end}}</p>
    {{- if .PanelURL}}
    <div class="actions"><a class="button primary" href="{{.PanelURL}}">{{if .KO}}운영 패널 열기{{else}}Open the operations panel{{end}}</a></div>
    {{- end}}
    {{- if .K3s}}
    <p class="hint">{{if .KO}}루프백 주소의 K3s 패널은 포트를 포워딩한 뒤 여세요.{{else}}For a loopback K3s panel, forward the port first.{{end}}<br><code>kubectl -n runquill-system port-forward svc/runquill 8081:8081</code></p>
    {{- end}}
    <section aria-labelledby="first-job-title">
      <h2 id="first-job-title">{{if .KO}}첫 작업 실행{{else}}Run a first job{{end}}</h2>
      <ol class="howto">
        <li><a class="button" href="/first-job.yml" download="runquill-first-job.yml">{{if .KO}}첫 작업 워크플로 다운로드{{else}}Download the first-job workflow{{end}}</a></li>
        <li>{{if .KO}}워크플로를 비공개 저장소의 .github/workflows/에 추가하고 Actions에서 실행하세요.{{else}}Add the workflow to .github/workflows/ in a private repository and run it from Actions.{{end}}</li>
        <li>{{if .KO}}운영 패널에서 작업 수신과 러너 정리를 확인하세요.{{else}}Follow job discovery and runner cleanup in the operations panel.{{end}}</li>
      </ol>
    </section>
    {{- if or .Serve .Funnel}}
    <section aria-labelledby="tailscale-title">
      <h2 id="tailscale-title">Tailscale</h2>
      <form action="/tailscale" method="post"><div class="actions"><button class="button">{{if .NetworkEnabled}}{{if .KO}}Tailscale 경로 일시 중지{{else}}Pause Tailscale routes{{end}}{{else}}{{if .KO}}Tailscale 경로 다시 켜기{{else}}Resume Tailscale routes{{end}}{{end}}</button><span class="muted">{{if .NetworkEnabled}}{{if .KO}}현재 켜져 있습니다.{{else}}Routes are on.{{end}}{{else}}{{if .KO}}현재 꺼져 있습니다.{{else}}Routes are off.{{end}}{{end}}</span></div></form>
    </section>
    {{- end}}
    {{- end}}
    {{- if or .Log (eq .Phase "deploying")}}
    <details class="deploy-log"{{if ne .Phase "done"}} open{{end}}>
      <summary>{{if .KO}}배포 로그{{else}}Deployment log{{end}}</summary>
      <pre id="wizard-log" tabindex="0">{{.Log}}</pre>
      {{- if eq .Phase "deploying"}}
      <a class="hint" href="/">{{if .KO}}상태 새로고침{{else}}Refresh status{{end}}</a>
      {{- end}}
    </details>
    {{- end}}
  </main>
</body>
</html>
{{define "field-error"}}<p class="field-error" id="wizard-error" role="alert"><strong>{{if .KO}}오류:{{else}}Error:{{end}}</strong> {{.Problem}}</p>{{end}}
{{define "stages"}}<section aria-labelledby="stages-title">
      <h2 id="stages-title">{{if .Toggling}}{{if .KO}}진행 단계{{else}}Stages{{end}}{{else}}{{if .KO}}배포 단계{{else}}Deployment stages{{end}}{{end}}</h2>
      {{- if eq .Phase "deploying"}}
      <p class="visually-hidden" role="status" id="wizard-activity">{{.Activity}}</p>
      {{- end}}
      <ol class="stages" id="wizard-stages" data-ok="{{if .KO}}완료{{else}}done{{end}}" data-current="{{if .KO}}진행 중{{else}}in progress{{end}}" data-pending="{{if .KO}}대기{{else}}pending{{end}}">
        {{- range .Stages}}
        <li data-stage="{{.Key}}" data-state="{{.State}}"{{if eq .State "current"}} aria-current="step"{{end}}>{{.Label}}<span class="state-text">{{.StateText}}</span></li>
        {{- end}}
      </ol>
    </section>{{end}}`))

func wizardCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("runquill wizard", flag.ContinueOnError)
	port := flags.Uint("listen-port", 0, "loopback port (0 selects a free port)")
	checkCompose := flags.Bool("check-compose", false, "check an existing Compose installation without redeploying it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *port > 65535 {
		return fmt.Errorf("use runquill wizard [--listen-port PORT] [--check-compose]")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	return runWizard(ctx, os.Stdout, dir, &http.Client{Timeout: 10 * time.Second}, githubAPIBase, *port, *checkCompose)
}

func runWizard(ctx context.Context, out io.Writer, dir string, client *http.Client, apiBase string, port uint, checkCompose bool) error {
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
	if checkCompose {
		data, err := os.ReadFile(filepath.Join(dir, setupEnvFile))
		if err != nil {
			return err
		}
		values, err := setupValues(data)
		if err != nil {
			return err
		}
		cfg, err := savedPanelSetup(values, "")
		if err != nil {
			return err
		}
		web.options = setupOptions{panel: true, panelURL: cfg.origin}
		web.phase = "deploying"
		web.checkCompose = true
		web.launchDeployment()
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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self' https://github.com; frame-ancestors 'none'; base-uri 'none'")
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
	case r.Method == http.MethodGet && r.URL.Path == "/wizard.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		fmt.Fprint(w, wizardJS)
	case r.Method == http.MethodGet && r.URL.Path == "/status":
		web.status(w)
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
	stage, log := web.progress.snapshot()
	view := struct {
		Language, Phase, Step, Install, Target, PanelURL, Stage, Log, Problem, Field, Activity string
		KO, K3s, Serve, Funnel, NetworkEnabled, CheckCompose, Error, Advanced, Toggling        bool
		Form                                                                                   wizardForm
		Steps, Stages                                                                          []wizardMark
	}{Language: language, Phase: web.phase, Step: web.step, Install: web.installURL, Target: web.kubeContext,
		PanelURL: web.panelLink(language), Stage: stage, Log: log, KO: language == "ko", K3s: web.options.kubernetes,
		Serve: web.network.serve, Funnel: web.network.funnel, NetworkEnabled: web.networkEnabled,
		CheckCompose: web.checkCompose, Error: web.failed, Toggling: web.toggling}
	view.Steps = wizardSteps(view.KO, view.Phase, view.Step, view.Install != "", view.Error)
	view.Stages = wizardStages(view.KO, view.K3s, view.CheckCompose, web.network.host != "", view.Toggling, view.Phase, stage, view.Error)
	view.Activity = wizardActivity[""][1]
	if activity, ok := wizardActivity[stage]; ok {
		view.Activity = activity[1]
		if view.KO {
			view.Activity = activity[0]
		}
	} else if view.KO {
		view.Activity = wizardActivity[""][0]
	}
	if web.phase == "options" {
		view.Form = web.form
		if web.failed && web.issue != nil {
			view.Field, view.Problem = web.issue.field, web.issue.en
			if view.KO {
				view.Problem = web.issue.ko
			}
		}
		view.Advanced = web.form.advanced() || slices.Contains(wizardAdvancedFields, view.Field)
	}
	web.mu.Unlock()
	if !view.K3s {
		view.Target = "Docker Compose"
	}
	// Same-origin browser form submissions need a non-null Origin header.
	w.Header().Set("Referrer-Policy", "same-origin")
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
	web.phase, web.failed = "working", false
	web.form, web.issue = wizardForm{}, nil
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
	web.phase, web.failed = phase, err != nil
	if err != nil {
		fmt.Fprintln(web.log, err)
	}
}

// reject returns to the first step with the submitted answers and, when known, the field to correct.
func (web *setupWizard) reject(form url.Values, err error) {
	var issue *wizardIssue
	errors.As(err, &issue)
	web.mu.Lock()
	web.form, web.issue = wizardFormValues(form), issue
	web.mu.Unlock()
	web.setResult("options", err)
}

// wizardOptions checks the first-step answers before any GitHub or host change.
func (web *setupWizard) wizardOptions(form url.Values) (setupOptions, string, error) {
	opts := setupOptions{
		panel: true, kubernetes: form.Get("backend") == "k3s", organization: strings.TrimSpace(form.Get("organization")),
		repositoryScope: form.Get("repositories"), panelURL: strings.TrimSpace(form.Get("panel_url")),
		webhookURL: strings.TrimSpace(form.Get("webhook_url")),
	}
	if opts.organization != "" && !setupLogin.MatchString(opts.organization) {
		return opts, "", newWizardIssue("organization", "GitHub 조직 로그인 이름을 정확히 입력하세요.", "Enter the GitHub organization's login name.")
	}
	if raw := strings.TrimSpace(form.Get("group")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			return opts, "", newWizardIssue("group", "러너 그룹 ID는 1 이상의 숫자여야 합니다.", "Enter a positive organization runner group ID.")
		}
		if opts.organization == "" {
			return opts, "", newWizardIssue("group", "러너 그룹을 쓰려면 GitHub 조직도 입력하세요.", "A runner group needs a GitHub organization.")
		}
		opts.runnerGroupID = value
	}
	if opts.panelURL != "" {
		if _, err := parsePanelURL(opts.panelURL); err != nil {
			return opts, "", newWizardIssue("panel_url", "패널 주소에는 경로와 인증 정보 없이 HTTPS 주소나 루프백 HTTP 주소를 입력하세요.", "Use an HTTPS origin or a loopback HTTP origin for the panel, without a path or credentials.")
		}
	}
	if opts.webhookURL != "" {
		if err := (setupOptions{panel: true, webhookURL: opts.webhookURL}).validate(); err != nil {
			return opts, "", newWizardIssue("webhook_url", "웹훅 URL에는 /webhook으로 끝나는 HTTPS 주소를 입력하세요. 인증 정보, 쿼리, 프래그먼트는 넣을 수 없습니다.", "Use an HTTPS URL ending in /webhook without credentials, query, or fragment.")
		}
	}
	kubeContext := strings.TrimSpace(form.Get("context"))
	if !opts.kubernetes {
		return opts, kubeContext, nil
	}
	if kubeContext == "" || strings.HasPrefix(kubeContext, "-") || len(kubeContext) > 200 || strings.ContainsAny(kubeContext, "\r\n") {
		return opts, kubeContext, newWizardIssue("context", "배포할 k3s 컨텍스트 이름을 정확히 입력하세요.", "Enter an explicit K3s context.")
	}
	if _, err := os.Stat(filepath.Join(web.dir, "runquill-k3s.yaml")); err != nil {
		return opts, kubeContext, &wizardIssue{
			ko:    "설치 디렉터리에 같은 릴리스의 runquill-k3s.yaml 파일이 없습니다. 파일을 넣은 뒤 다시 시도하세요.",
			en:    "Put the matching Runquill release's runquill-k3s.yaml in the installation directory, then try again.",
			cause: err,
		}
	}
	return opts, kubeContext, nil
}

func (web *setupWizard) configure(form url.Values) {
	opts, kubeContext, err := web.wizardOptions(form)
	if err != nil {
		web.reject(form, err)
		return
	}
	network := wizardNetwork{serve: form.Get("serve") == "on", funnel: form.Get("funnel") == "on"}
	if err := network.configure(web.ctx, &opts); err != nil {
		web.reject(form, err)
		return
	}
	web.mu.Lock()
	web.options, web.network, web.kubeContext = opts, network, kubeContext
	web.mu.Unlock()
	ctx := context.WithValue(web.ctx, setupWizardKey{}, web)
	if err := setup(ctx, web.log, web.dir, web.client, web.apiBase, opts); err != nil {
		web.reject(form, err)
		return
	}
	data, err := os.ReadFile(filepath.Join(web.dir, setupEnvFile))
	if err != nil {
		web.reject(form, err)
		return
	}
	values, err := setupValues(data)
	if err != nil {
		web.reject(form, err)
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
	web.phase, web.failed = "deploying", false
	web.mu.Unlock()
	web.launchDeployment()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (web *setupWizard) launchDeployment() {
	go func() {
		ctx, cancel := context.WithTimeout(web.ctx, 8*time.Minute)
		defer cancel()
		if err := web.apply(ctx, web.checkCompose); err != nil {
			fmt.Fprintln(&web.progress, err)
			web.setResult("ready", err)
		} else {
			web.mu.Lock()
			web.networkEnabled = web.network.host != ""
			web.mu.Unlock()
			web.setResult("done", nil)
		}
	}()
}

func (web *setupWizard) toggleNetwork(w http.ResponseWriter, r *http.Request) {
	web.mu.Lock()
	if web.phase != "done" || web.network.host == "" {
		web.mu.Unlock()
		http.Error(w, "Network settings are unavailable in this step.", http.StatusConflict)
		return
	}
	enabled := !web.networkEnabled
	web.progress.mark("network")
	web.phase, web.failed, web.toggling = "deploying", false, true
	web.mu.Unlock()
	go func() {
		err := web.network.apply(web.ctx, enabled)
		if err != nil {
			fmt.Fprintln(&web.progress, err)
		}
		web.mu.Lock()
		web.toggling = false
		if err == nil {
			web.networkEnabled = enabled
		}
		web.mu.Unlock()
		web.setResult("done", err)
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
