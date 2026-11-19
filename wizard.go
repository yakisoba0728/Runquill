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
	host             wizardHost          // zero when unknown: the options step then suggests no limits
	command          wizardCommandRunner // nil skips the Docker access check
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
	MaxRunners, RunnerCPUs, RunnerMemory                                      string
	Serve, Funnel                                                             bool
}

var wizardAdvancedFields = []string{"organization", "group", "panel_url", "webhook_url", "serve", "funnel"}

func wizardFormValues(form url.Values) wizardForm {
	return wizardForm{
		Backend: form.Get("backend"), Repositories: form.Get("repositories"), Context: form.Get("context"),
		Organization: form.Get("organization"), Group: form.Get("group"),
		PanelURL: wizardVisibleURL(form.Get("panel_url")), WebhookURL: wizardVisibleURL(form.Get("webhook_url")),
		MaxRunners: form.Get("max_runners"), RunnerCPUs: form.Get("runner_cpus"), RunnerMemory: form.Get("runner_memory"),
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
	Number                               int
}

// wizardChrome is the frame every wizard page shares: the step rail, its compact
// phone header and the language and appearance controls.
type wizardChrome struct {
	KO, Status     bool // Status links back to the wizard's own page from a GitHub step
	Steps          []wizardMark
	Index, Count   int
	Current        string
	Other, OtherAs string // the other language's code and its own name
}

func newWizardChrome(ko bool, steps []wizardMark) *wizardChrome {
	chrome := &wizardChrome{KO: ko, Steps: steps, Count: len(steps), Other: "ko", OtherAs: "한국어"}
	if ko {
		chrome.Other, chrome.OtherAs = "en", "English"
	}
	for i, step := range steps {
		if step.State == "current" || step.State == "failed" {
			chrome.Index, chrome.Current = i+1, step.Label
		}
	}
	return chrome
}

// wizardPageChrome frames a GitHub-step page served inside the wizard, or returns nil
// for the standalone pages runquill setup serves on its own.
func wizardPageChrome(ctx context.Context, step string) *wizardChrome {
	web := currentWizard(ctx)
	if web == nil {
		return nil
	}
	ko := wizardKorean(ctx)
	chrome := newWizardChrome(ko, wizardSteps(ko, "working", step, false, false))
	chrome.Status = true
	return chrome
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
		mark := wizardMark{Label: name[1], State: "pending", Number: i + 1}
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
		// Every step says what it does, so the upcoming ones read as "what happens next".
		about := [][2]string{{"대상과 용량 선택", "Target and capacity"}, {"앱 등록과 설치", "Register and install the App"},
			{"매니저 시작과 점검", "Start and check the manager"}, {"워크플로 실행", "Run a workflow"}}[i]
		mark.Detail = about[1]
		if ko {
			mark.Detail = about[0]
		}
		if mark.State == "failed" {
			mark.Detail = wizardStateText(ko, "failed")
		} else if i == 1 && phase == "working" {
			detail := [2]string{"연결 확인", "Checking"}
			switch {
			case step == "/connect":
				detail = [2]string{"앱 등록", "App registration"}
			case step == "/oauth":
				detail = [2]string{"패널 로그인", "Panel sign-in"}
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

// wizardChromeTemplates are parsed into every wizard page: the wizard's own and the
// GitHub-step pages in setup_manifest.go and setup_panel.go. Each takes a *wizardChrome.
const wizardChromeTemplates = `{{define "rail"}}{{if .}}
<a class="skip" href="#content">{{if .KO}}본문으로 건너뛰기{{else}}Skip to content{{end}}</a>
<header class="rail">
  <p class="brand"><span class="brand-name">runquill</span><span class="brand-sub">{{if .KO}}설치 마법사{{else}}Setup wizard{{end}}</span></p>
  <nav class="progress" aria-label="{{if .KO}}설치 단계{{else}}Setup steps{{end}}">
    <p class="compact">{{if .KO}}{{.Count}}단계 중 {{.Index}}단계{{else}}Step {{.Index}} of {{.Count}}{{end}} — {{.Current}}</p>
    <ol class="bar" aria-hidden="true">{{range .Steps}}<li data-state="{{.State}}"></li>{{end}}</ol>
    <ol class="steps">
      {{- range .Steps}}
      <li data-state="{{.State}}"{{if eq .State "current"}} aria-current="step"{{end}}><span class="node" aria-hidden="true">{{.Number}}</span><span class="step-text"><span class="step-label">{{.Label}}</span>{{if .Detail}}<span class="step-detail">{{.Detail}}</span>{{end}}</span>{{if .StateText}}<span class="visually-hidden"> ({{.StateText}})</span>{{end}}</li>
      {{- end}}
    </ol>
  </nav>
  <div class="prefs">
    {{- if .Status}}
    <a href="/">{{if .KO}}설치 상태{{else}}Setup status{{end}}</a>
    {{- end}}
    <a class="lang" href="?lang={{.Other}}" lang="{{.Other}}" hreflang="{{.Other}}">{{.OtherAs}}</a>
    <label data-theme-field hidden><span class="visually-hidden">{{if .KO}}화면 테마{{else}}Appearance{{end}}</span>
      <select data-theme-control><option value="system">{{if .KO}}시스템{{else}}System{{end}}</option><option value="light">{{if .KO}}밝게{{else}}Light{{end}}</option><option value="dark">{{if .KO}}어둡게{{else}}Dark{{end}}</option></select>
    </label>
  </div>
</header>
<p class="visually-hidden" id="copy-status" aria-live="polite" data-copy="{{if .KO}}복사{{else}}Copy{{end}}" data-copied="{{if .KO}}복사했습니다{{else}}Copied{{end}}" data-manual="{{if .KO}}선택했습니다. 직접 복사하세요{{else}}Selected. Copy it manually{{end}}"></p>
{{- else}}
<header class="rail standalone"><p class="brand"><span class="brand-name">runquill</span></p></header>
{{- end}}{{end}}
{{define "value"}}<div class="value"><code>{{.}}</code><button type="button" class="copy" hidden></button></div>{{end}}`

var wizardHome = template.Must(template.Must(template.New("wizard").Parse(wizardChromeTemplates)).Parse(`<!doctype html>
<html lang="{{.Language}}">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <title>Runquill · {{if .KO}}설치 마법사{{else}}Setup wizard{{end}}</title>
  <link rel="stylesheet" href="/wizard.css">
  <script src="/wizard.js"></script>
  {{if or (eq .Phase "working") (eq .Phase "deploying")}}<noscript><meta http-equiv="refresh" content="4"></noscript>{{end}}
</head>
<body data-phase="{{.Phase}}" data-step="{{.Step}}" data-install="{{if .Install}}true{{else}}false{{end}}" data-stage="{{.Stage}}">
<div class="frame">
  {{- template "rail" .Chrome}}
  <main class="content" id="content" tabindex="-1">
    <p class="notice" data-state="waiting" id="wizard-connection" role="status" data-reconnecting="{{if .KO}}설치 마법사에 다시 연결하고 있습니다…{{else}}Reconnecting to the setup wizard…{{end}}" data-ended="{{if .KO}}설치 세션이 끝났습니다. 새 설치 링크를 여세요.{{else}}This setup session ended. Open a new setup link.{{end}}"></p>
    <div class="intro{{if eq .Phase "done"}} done{{end}}">
    {{- if eq .Phase "options"}}
      <h1>{{if .KO}}Runquill 시작하기{{else}}Set up Runquill{{end}}</h1>
      <p class="lead">{{if .KO}}러너를 실행할 곳과 동시에 실행할 개수를 정하세요. 나머지는 기본값으로 시작하고 나중에 .env에서 바꿀 수 있습니다.{{else}}Choose where runners run and how many run at once. Everything else starts with defaults you can change later in .env.{{end}}</p>
    {{- else if eq .Phase "working"}}
      <h1>{{if .KO}}GitHub 연결{{else}}Connect GitHub{{end}}</h1>
      {{- if .Step}}
      <p class="lead">{{if eq .Step "/oauth"}}{{if .KO}}운영 패널의 로그인 설정을 입력합니다.{{else}}Enter the operations panel's sign-in settings.{{end}}{{else}}{{if .KO}}GitHub에 Runquill용 비공개 GitHub App을 등록합니다.{{else}}Register a private GitHub App for Runquill.{{end}}{{end}}</p>
      {{- else if .Install}}
      <p class="lead">{{if .KO}}GitHub에서 앱을 설치하면 이 화면이 다음 단계로 넘어갑니다.{{else}}This page advances after you install the app on GitHub.{{end}}</p>
      {{- else}}
      <p class="lead waiting" role="status"><span class="spinner" aria-hidden="true"></span>{{if .KO}}연결을 확인하고 있습니다.{{else}}Checking the connection.{{end}}</p>
      {{- end}}
    {{- else if eq .Phase "ready"}}
      <h1>{{if .CheckCompose}}{{if .KO}}배포 상태 확인{{else}}Check deployment{{end}}{{else}}{{if .KO}}Runquill 배포{{else}}Deploy Runquill{{end}}{{end}}</h1>
      {{- if not .Error}}
      <p class="lead">{{if .KO}}GitHub 연결을 마쳤습니다. 배포 대상을 확인하세요.{{else}}GitHub is connected. Review the deployment target.{{end}}</p>
      {{- end}}
    {{- else if eq .Phase "deploying"}}
      <h1>{{if .Toggling}}{{if .KO}}Tailscale 경로 변경 중{{else}}Updating Tailscale routes{{end}}{{else if .CheckCompose}}{{if .KO}}배포 상태 확인 중{{else}}Checking deployment{{end}}{{else}}{{if .KO}}Runquill 배포 중{{else}}Deploying Runquill{{end}}{{end}}</h1>
      <p class="lead" role="status" id="wizard-activity"><span data-activity>{{.Activity}}</span></p>
    {{- else if eq .Phase "done"}}
      <h1><span class="tick large" aria-hidden="true"></span>{{if .KO}}배포 완료{{else}}Deployment complete{{end}}</h1>
      <p class="lead">{{if .KO}}매니저가 실행 중이며 사전 점검을 통과했습니다.{{else}}The manager is running and preflight checks passed.{{end}}</p>
    {{- end}}
    </div>
    {{- if and .Error (not .Field)}}
    <p class="notice" data-state="failed" role="alert" id="wizard-error"><strong>{{if .KO}}오류:{{else}}Error:{{end}}</strong>
      {{if .Problem}}{{.Problem}}
      {{- else if eq .Phase "ready"}}{{if .KO}}배포 점검을 통과하지 못했습니다. 아래 로그에서 원인을 확인하세요.{{else}}Deployment checks failed. See the log below.{{end}}
      {{- else}}{{if .KO}}설정 중 문제가 발생했습니다. Runquill 터미널에서 원인을 확인하세요.{{else}}Setup needs attention. Check the Runquill terminal for details.{{end}}{{end}}</p>
    {{- end}}
    {{- if eq .Phase "options"}}
    <form method="post" action="/start" class="steps-form">
      <section class="group" aria-labelledby="group-target">
        <h2 id="group-target">{{if .KO}}배포 대상{{else}}Where runners run{{end}}</h2>
        <div class="form-row">
          <div class="field">
            <label for="wizard-backend">{{if .KO}}배포 대상{{else}}Deployment target{{end}}</label>
            <select id="wizard-backend" name="backend"{{if eq .Field "backend"}} aria-invalid="true" aria-describedby="wizard-error{{if .Docker.Blocked}} wizard-docker{{end}}" autofocus{{else if .Docker.Blocked}} aria-describedby="wizard-docker"{{end}}><option value="docker">Docker Compose</option><option value="k3s"{{if eq .Form.Backend "k3s"}} selected{{end}}>K3s</option></select>
            {{- if eq .Field "backend"}}{{template "field-error" .}}{{end}}
          </div>
          <div class="field" data-k3s{{if ne .Form.Backend "k3s"}} hidden{{end}}>
            <label for="wizard-context">{{if .KO}}K3s 컨텍스트 이름{{else}}K3s context name{{end}}</label>
            <input id="wizard-context" name="context" maxlength="200" spellcheck="false" autocomplete="off" value="{{.Form.Context}}"{{if eq .Form.Backend "k3s"}} required{{end}}{{if eq .Field "context"}} aria-invalid="true" aria-describedby="wizard-error help-context" autofocus{{else}} aria-describedby="help-context"{{end}}>
            {{- if eq .Field "context"}}{{template "field-error" .}}{{end}}
            <p class="hint" id="help-context">{{if .KO}}kubectl config get-contexts에 표시되는 이름입니다.{{else}}As listed by kubectl config get-contexts.{{end}}</p>
          </div>
        </div>
        {{- if .Docker.Blocked}}
        <div class="callout" data-state="failed" id="wizard-docker" data-docker data-docker-blocked{{if eq .Form.Backend "k3s"}} hidden{{end}}>
          {{- if eq .Docker.State "permission"}}
          <h3>{{if .KO}}이 사용자는 Docker를 쓸 권한이 없습니다{{else}}This user cannot use Docker{{end}}</h3>
          <p>{{if .KO}}이대로는 배포 단계에서 실패합니다. 서버에서 아래 명령을 실행하고 로그아웃했다가 다시 로그인한 뒤 이 페이지를 새로고침하세요.{{else}}Deployment would fail at this point. Run this on the server, sign out and back in, then reload this page.{{end}}</p>
          {{template "value" (print "sudo usermod -aG docker " .User)}}
          <p class="hint">{{if .KO}}설치 스크립트를 다시 실행해도 이 설정을 대신 해 줍니다.{{else}}Re-running the installer can make this change for you.{{end}}</p>
          {{- else if eq .Docker.State "missing"}}
          <h3>{{if .KO}}Docker를 찾을 수 없습니다{{else}}Docker is not installed{{end}}</h3>
          <p>{{if .KO}}이 서버에 Docker Engine과 Compose 플러그인을 설치한 뒤 이 페이지를 새로고침하세요. 설치 스크립트를 다시 실행해도 됩니다.{{else}}Install Docker Engine and the Compose plugin on this server, then reload this page. Re-running the installer also works.{{end}}</p>
          {{- else}}
          <h3>{{if .KO}}Docker 데몬에 연결할 수 없습니다{{else}}Docker is not answering{{end}}</h3>
          <p>{{if .KO}}Docker 서비스가 실행 중인지 확인하세요. 시작한 뒤 이 페이지를 새로고침하세요.{{else}}Check that the Docker service is running. Start it, then reload this page.{{end}}</p>
          {{template "value" "sudo systemctl start docker"}}
          {{- end}}
          {{- if .Docker.Detail}}
          <details class="detail-line"><summary>{{if .KO}}Docker 응답 보기{{else}}Show Docker's reply{{end}}</summary><code>{{.Docker.Detail}}</code></details>
          {{- end}}
        </div>
        {{- else if eq .Docker.State "ok"}}
        <p class="confirm" data-docker{{if eq .Form.Backend "k3s"}} hidden{{end}}><span class="tick" aria-hidden="true"></span>{{if .KO}}Docker {{.Docker.Detail}}에 연결할 수 있습니다.{{else}}Docker {{.Docker.Detail}} is reachable.{{end}}</p>
        {{- end}}
      </section>
      <section class="group" aria-labelledby="group-capacity">
        <h2 id="group-capacity">{{if .KO}}러너 용량{{else}}Runner capacity{{end}}</h2>
        {{- if .Host.CPUs}}
        <p class="host" id="help-capacity">{{if .KO}}이 서버의 CPU {{.Host.CPUs}}개{{if .Host.MemoryMiB}}, 메모리 {{.HostMemory}}GiB{{end}}를 감지했습니다. 호스트용으로 CPU 약 2개{{if .Host.MemoryMiB}}와 메모리 약 4GiB{{end}}를 남기고 나머지를 러너에 나눈 값입니다.{{else}}Detected {{.Host.CPUs}} CPUs{{if .Host.MemoryMiB}} and {{.HostMemory}} GiB of memory{{end}} on this server. The suggestion keeps about 2 CPUs{{if .Host.MemoryMiB}} and 4 GiB{{end}} for the host and divides the rest among runners.{{end}}</p>
        {{- else}}
        <p class="host" id="help-capacity">{{if .KO}}이 서버의 크기를 읽지 못했습니다. 동시 러너 수를 정하고, 러너당 제한은 비워 두거나 직접 입력하세요.{{else}}The server's size could not be read. Set how many runners run at once; leave the per-runner limits empty or set them yourself.{{end}}</p>
        {{- end}}
        <p class="hint" data-k3s{{if ne .Form.Backend "k3s"}} hidden{{end}}>{{if .KO}}K3s에서는 이 서버가 아니라 클러스터 노드 크기에 맞춰 조정하세요.{{else}}For K3s, size these to the cluster's nodes rather than this server.{{end}}</p>
        <div class="form-row three">
          <div class="field">
            <label for="wizard-max-runners">{{if .KO}}동시 러너 수{{else}}Runners at once{{end}}</label>
            <input id="wizard-max-runners" name="max_runners" type="number" inputmode="numeric" min="1" max="1000" step="1" value="{{.Form.MaxRunners}}" placeholder="2"{{if eq .Field "max_runners"}} aria-invalid="true" aria-describedby="wizard-error help-capacity" autofocus{{else}} aria-describedby="help-capacity"{{end}}>
            {{- if eq .Field "max_runners"}}{{template "field-error" .}}{{end}}
          </div>
          <div class="field">
            <label for="wizard-runner-cpus">{{if .KO}}러너당 CPU{{else}}CPUs per runner{{end}}</label>
            <input id="wizard-runner-cpus" name="runner_cpus" type="number" inputmode="decimal" min="0.01" step="any" value="{{.Form.RunnerCPUs}}" placeholder="{{if .KO}}제한 없음{{else}}No limit{{end}}"{{if eq .Field "runner_cpus"}} aria-invalid="true" aria-describedby="wizard-error help-limits" autofocus{{else}} aria-describedby="help-limits"{{end}}>
            {{- if eq .Field "runner_cpus"}}{{template "field-error" .}}{{end}}
          </div>
          <div class="field">
            <label for="wizard-runner-memory">{{if .KO}}러너당 메모리 (GiB){{else}}Memory per runner (GiB){{end}}</label>
            <input id="wizard-runner-memory" name="runner_memory" type="number" inputmode="decimal" min="0.01" step="any" value="{{.Form.RunnerMemory}}" placeholder="{{if .KO}}제한 없음{{else}}No limit{{end}}"{{if eq .Field "runner_memory"}} aria-invalid="true" aria-describedby="wizard-error help-limits" autofocus{{else}} aria-describedby="help-limits"{{end}}>
            {{- if eq .Field "runner_memory"}}{{template "field-error" .}}{{end}}
          </div>
        </div>
        <p class="hint" id="help-limits">{{if .KO}}러너당 제한을 비워 두면 러너가 호스트 자원을 제한 없이 씁니다.{{else}}Leave a per-runner limit empty to let runners use the host without one.{{end}}</p>
      </section>
      <section class="group" aria-labelledby="group-github">
        <h2 id="group-github">GitHub</h2>
        <div class="field">
          <label for="wizard-repositories">{{if .KO}}저장소 접근 범위{{else}}Repository access{{end}}</label>
          <select id="wizard-repositories" name="repositories" aria-describedby="help-repositories"><option value="all">{{if .KO}}전체 저장소{{else}}All repositories{{end}}</option><option value="selected"{{if eq .Form.Repositories "selected"}} selected{{end}}>{{if .KO}}선택한 저장소{{else}}Selected repositories{{end}}</option></select>
          <p class="hint" id="help-repositories">{{if .KO}}GitHub에서 앱을 설치할 때도 같은 범위를 선택하세요.{{else}}Choose the same access when you install the App on GitHub.{{end}}</p>
        </div>
      </section>
      <details class="advanced"{{if .Advanced}} open{{end}}>
        <summary>{{if .KO}}조직 및 네트워크{{else}}Organization and network{{end}}</summary>
        <div class="advanced-body">
          <div class="form-row">
            <div class="field">
              <label for="wizard-organization">{{if .KO}}GitHub 조직 (선택){{else}}GitHub organization (optional){{end}}</label>
              <input id="wizard-organization" name="organization" maxlength="39" spellcheck="false" autocomplete="off" value="{{.Form.Organization}}"{{if eq .Field "organization"}} aria-invalid="true" aria-describedby="wizard-error help-organization" autofocus{{else}} aria-describedby="help-organization"{{end}}>
              {{- if eq .Field "organization"}}{{template "field-error" .}}{{end}}
              <p class="hint" id="help-organization">{{if .KO}}비워 두면 개인 계정에 앱을 등록합니다.{{else}}Leave empty to register the App on your personal account.{{end}}</p>
            </div>
            <div class="field">
              <label for="wizard-group">{{if .KO}}러너 그룹 ID (선택){{else}}Runner group ID (optional){{end}}</label>
              <input id="wizard-group" name="group" inputmode="numeric" pattern="[0-9]*" maxlength="19" autocomplete="off" value="{{.Form.Group}}"{{if eq .Field "group"}} aria-invalid="true" aria-describedby="wizard-error help-group" autofocus{{else}} aria-describedby="help-group"{{end}}>
              {{- if eq .Field "group"}}{{template "field-error" .}}{{end}}
              <p class="hint" id="help-group">{{if .KO}}조직 러너 그룹에 러너를 등록할 때만 입력하세요.{{else}}Only for an organization runner group.{{end}}</p>
            </div>
          </div>
          <div class="field">
            <label for="wizard-panel-url">{{if .KO}}패널 주소 (선택){{else}}Panel origin (optional){{end}}</label>
            <input id="wizard-panel-url" name="panel_url" type="url" spellcheck="false" placeholder="http://127.0.0.1:8081" value="{{.Form.PanelURL}}"{{if eq .Field "panel_url"}} aria-invalid="true" aria-describedby="wizard-error help-panel-url" autofocus{{else}} aria-describedby="help-panel-url"{{end}}>
            {{- if eq .Field "panel_url"}}{{template "field-error" .}}{{end}}
            <p class="hint" id="help-panel-url">{{if .KO}}비워 두면 http://127.0.0.1:8081을 씁니다.{{else}}Defaults to http://127.0.0.1:8081.{{end}}</p>
          </div>
          <div class="field">
            <label for="wizard-webhook-url">{{if .KO}}공개 웹훅 URL (선택){{else}}Public webhook URL (optional){{end}}</label>
            <input id="wizard-webhook-url" name="webhook_url" type="url" spellcheck="false" placeholder="https://example.com/webhook" value="{{.Form.WebhookURL}}"{{if eq .Field "webhook_url"}} aria-invalid="true" aria-describedby="wizard-error help-webhook-url" autofocus{{else}} aria-describedby="help-webhook-url"{{end}}>
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
      <div class="actions"><button class="button primary" data-start>{{if .KO}}GitHub 연결 시작{{else}}Connect GitHub{{end}}</button><p class="next">{{if .KO}}다음 단계에서 GitHub에 Runquill용 비공개 앱을 등록합니다.{{else}}Next, you register a private App for Runquill on GitHub.{{end}}</p></div>
    </form>
    {{- else if eq .Phase "working"}}
    {{- if .Step}}
    <div class="actions"><a class="button primary" href="{{.Step}}/">{{if .KO}}현재 단계 열기{{else}}Open the current step{{end}}</a></div>
    {{- else if .Install}}
    <ol class="howto">
      <li>{{if .KO}}아래 버튼으로 GitHub를 새 탭에서 엽니다.{{else}}Open GitHub in a new tab with the button below.{{end}}</li>
      <li>{{if .KO}}설치 옵션에서 고른 저장소 범위를 선택하고 앱을 설치합니다.{{else}}Pick the repository access you chose in the options and install the App.{{end}}</li>
      <li>{{if .KO}}이 탭으로 돌아오면 자동으로 다음 단계로 넘어갑니다.{{else}}Come back to this tab; it moves on by itself.{{end}}</li>
    </ol>
    <div class="actions"><a class="button primary" href="{{.Install}}" rel="noreferrer" target="_blank">{{if .KO}}GitHub에 앱 설치{{else}}Install the GitHub App{{end}}</a><p class="next waiting"><span class="spinner" aria-hidden="true"></span>{{if .KO}}설치를 기다리고 있습니다.{{else}}Waiting for the installation.{{end}}</p></div>
    {{- end}}
    {{- else if eq .Phase "ready"}}
    <dl class="kv">
      <div><dt>{{if .KO}}배포 대상{{else}}Deployment target{{end}}</dt><dd class="target">{{if .K3s}}K3s <code>{{.Target}}</code>{{else}}{{.Target}}{{end}}</dd></div>
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
    {{template "stages" .}}
    {{- else if eq .Phase "done"}}
    {{- if .PanelURL}}
    <section class="group" aria-labelledby="panel-title">
      <h2 id="panel-title">{{if .KO}}운영 패널{{else}}Operations panel{{end}}</h2>
      {{template "value" .PanelOrigin}}
      {{- if .K3s}}
      <p class="hint">{{if .KO}}루프백 주소의 K3s 패널은 포트를 포워딩한 뒤 여세요.{{else}}For a loopback K3s panel, forward the port first.{{end}}</p>
      {{template "value" "kubectl -n runquill-system port-forward svc/runquill 8081:8081"}}
      {{- end}}
      <div class="actions"><a class="button primary" href="{{.PanelURL}}">{{if .KO}}운영 패널 열기{{else}}Open the operations panel{{end}}</a></div>
    </section>
    {{- else if .K3s}}
    <p class="hint">{{if .KO}}루프백 주소의 K3s 패널은 포트를 포워딩한 뒤 여세요.{{else}}For a loopback K3s panel, forward the port first.{{end}}</p>
    {{template "value" "kubectl -n runquill-system port-forward svc/runquill 8081:8081"}}
    {{- end}}
    <section class="group" aria-labelledby="first-job-title">
      <h2 id="first-job-title">{{if .KO}}첫 작업 실행{{else}}Run a first job{{end}}</h2>
      <ol class="howto">
        <li><span>{{if .KO}}첫 작업 워크플로를 내려받습니다.{{else}}Download the first-job workflow.{{end}}</span><a class="button" href="/first-job.yml" download="runquill-first-job.yml">{{if .KO}}첫 작업 워크플로 다운로드{{else}}Download the first-job workflow{{end}}</a></li>
        <li>{{if .KO}}워크플로를 비공개 저장소의 .github/workflows/에 추가하고 Actions에서 실행하세요.{{else}}Add the workflow to .github/workflows/ in a private repository and run it from Actions.{{end}}</li>
        <li>{{if .KO}}운영 패널에서 작업 수신과 러너 정리를 확인하세요.{{else}}Follow job discovery and runner cleanup in the operations panel.{{end}}</li>
      </ol>
    </section>
    {{- if or .Serve .Funnel}}
    <section class="group" aria-labelledby="tailscale-title">
      <h2 id="tailscale-title">Tailscale</h2>
      <form action="/tailscale" method="post"><div class="actions"><button class="button">{{if .NetworkEnabled}}{{if .KO}}Tailscale 경로 일시 중지{{else}}Pause Tailscale routes{{end}}{{else}}{{if .KO}}Tailscale 경로 다시 켜기{{else}}Resume Tailscale routes{{end}}{{end}}</button><p class="next">{{if .NetworkEnabled}}{{if .KO}}현재 켜져 있습니다.{{else}}Routes are on.{{end}}{{else}}{{if .KO}}현재 꺼져 있습니다.{{else}}Routes are off.{{end}}{{end}}</p></div></form>
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
</div>
</body>
</html>
{{define "field-error"}}<p class="field-error" id="wizard-error" role="alert"><strong>{{if .KO}}오류:{{else}}Error:{{end}}</strong> {{.Problem}}</p>{{end}}
{{define "stages"}}<section class="group" aria-labelledby="stages-title">
      <h2 id="stages-title">{{if .Toggling}}{{if .KO}}진행 단계{{else}}Stages{{end}}{{else}}{{if .KO}}배포 단계{{else}}Deployment stages{{end}}{{end}}</h2>
      <ol class="stages" id="wizard-stages" data-ok="{{if .KO}}완료{{else}}done{{end}}" data-current="{{if .KO}}진행 중{{else}}in progress{{end}}" data-pending="{{if .KO}}대기{{else}}pending{{end}}">
        {{- range .Stages}}
        <li data-stage="{{.Key}}" data-state="{{.State}}"{{if eq .State "current"}} aria-current="step"{{end}}><span class="node" aria-hidden="true"></span><span class="stage-label">{{.Label}}</span><span class="state-text">{{.StateText}}</span></li>
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
		host: detectWizardHost(), command: execWizardCommand,
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
	if os.Getenv("RUNQUILL_SSH_HINT_SHOWN") == "1" {
		// install.sh has already printed the forwarding command and the note about the key.
		fmt.Fprintf(out, "Open %s/?key=%s in your browser.\n", web.origin, web.key)
	} else {
		fmt.Fprint(out, wizardStartMessage(web.origin, web.key, listener.Addr().(*net.TCPAddr).Port, os.Getenv("SSH_CONNECTION"), wizardUser()))
	}
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
	// A language link (?lang=) works on every wizard page, including the GitHub steps.
	if lang := r.URL.Query().Get("lang"); r.Method == http.MethodGet && (lang == "ko" || lang == "en") {
		web.mu.Lock()
		web.language = lang
		web.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "runquill_language", Value: lang, Path: "/", SameSite: http.SameSiteLaxMode, MaxAge: 3600})
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
	// Check Docker on every view of the first step, outside the lock the status poll shares.
	web.mu.Lock()
	options, run := web.phase == "options", web.command
	web.mu.Unlock()
	var docker wizardDocker
	if options {
		docker = probeWizardDocker(r.Context(), run)
	}
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
		User, HostMemory, PanelOrigin                                                          string
		KO, K3s, Serve, Funnel, NetworkEnabled, CheckCompose, Error, Advanced, Toggling        bool
		Form                                                                                   wizardForm
		Steps, Stages                                                                          []wizardMark
		Chrome                                                                                 *wizardChrome
		Host                                                                                   wizardHost
		Docker                                                                                 wizardDocker
	}{Language: language, Phase: web.phase, Step: web.step, Install: web.installURL, Target: web.kubeContext,
		PanelURL: web.panelLink(language), Stage: stage, Log: log, KO: language == "ko", K3s: web.options.kubernetes,
		Serve: web.network.serve, Funnel: web.network.funnel, NetworkEnabled: web.networkEnabled,
		CheckCompose: web.checkCompose, Error: web.failed, Toggling: web.toggling,
		Host: web.host, HostMemory: strconv.FormatFloat(float64(web.host.MemoryMiB)/1024, 'f', 1, 64), Docker: docker}
	view.Steps = wizardSteps(view.KO, view.Phase, view.Step, view.Install != "", view.Error)
	view.Chrome = newWizardChrome(view.KO, view.Steps)
	view.PanelOrigin, _, _ = strings.Cut(view.PanelURL, "/?")
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
	if web.phase == "options" && docker.State == "ok" && web.issue != nil && web.issue.field == "backend" {
		// Docker was refused earlier and is reachable now: the refusal no longer applies.
		web.failed, web.issue = false, nil
		view.Error = false
		view.Steps = wizardSteps(view.KO, view.Phase, view.Step, view.Install != "", false)
		view.Chrome = newWizardChrome(view.KO, view.Steps)
	}
	if web.phase == "options" {
		view.Form = web.form
		if view.Form == (wizardForm{}) && !web.failed && web.host.CPUs > 0 {
			suggested := recommendCapacity(web.host)
			view.Form.MaxRunners = strconv.Itoa(suggested.Runners)
			view.Form.RunnerCPUs, view.Form.RunnerMemory = wizardCPUText(suggested.CPUs), wizardGiB(suggested.MemoryMiB)
		}
		if view.Docker.Blocked() {
			view.User = wizardUser()
		}
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
	capacity, err := parseCapacity(wizardFormValues(form))
	if err != nil {
		web.reject(form, err)
		return
	}
	if !opts.kubernetes {
		if docker := probeWizardDocker(web.ctx, web.command); docker.Blocked() {
			web.reject(form, &wizardIssue{field: "backend", ko: "이 사용자는 Docker를 쓸 수 없습니다. 안내에 따라 해결한 뒤 다시 시도하세요.",
				en: "This user cannot use Docker. Fix it as described, then try again.", cause: errors.New(docker.State + ": " + docker.Detail)})
			return
		}
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
	if err := saveCapacity(web.dir, opts.kubernetes, capacity); err != nil {
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
