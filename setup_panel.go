package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const defaultSetupPanelURL = "http://127.0.0.1:8081"

var setupOAuthValue = regexp.MustCompile("^[A-Za-z0-9._-]{1,256}$")

var panelSetupPage = template.Must(template.New("panel-setup").Parse(`<!doctype html>
<html lang="{{if .Korean}}ko{{else}}en{{end}}">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <title>Runquill · {{if .Korean}}패널 로그인 설정{{else}}Panel setup{{end}}</title>
  {{- if .InWizard}}
  <link rel="stylesheet" href="/wizard.css">
  <script src="/wizard.js"></script>
  {{- end}}
</head>
<body>
{{- if .InWizard}}
<header class="topbar">
  <span class="brand">runquill</span>
  <span class="topbar-title">{{if .Korean}}설치 마법사{{else}}Setup wizard{{end}}</span>
  <div class="topbar-controls">
    <a href="/">{{if .Korean}}설치 상태{{else}}Setup status{{end}}</a>
    <label data-theme-field hidden><span class="visually-hidden">{{if .Korean}}화면 테마{{else}}Appearance{{end}}</span>
      <select data-theme-control><option value="system">{{if .Korean}}시스템{{else}}System{{end}}</option><option value="light">{{if .Korean}}밝게{{else}}Light{{end}}</option><option value="dark">{{if .Korean}}어둡게{{else}}Dark{{end}}</option></select>
    </label>
  </div>
</header>
{{- end}}
<main{{if .InWizard}} class="wizard"{{end}}>
  {{- if .InWizard}}
  <ol class="steps" aria-label="{{if .Korean}}설치 단계{{else}}Setup steps{{end}}">
    <li data-state="ok">{{if .Korean}}설치 옵션{{else}}Options{{end}}<span class="visually-hidden"> ({{if .Korean}}완료{{else}}done{{end}})</span></li>
    <li data-state="current" aria-current="step">{{if .Korean}}GitHub 연결{{else}}GitHub{{end}} <span class="detail">· {{if .Korean}}패널 로그인{{else}}panel sign-in{{end}}</span></li>
    <li data-state="pending">{{if .Korean}}배포{{else}}Deploy{{end}}<span class="visually-hidden"> ({{if .Korean}}대기{{else}}pending{{end}})</span></li>
    <li data-state="pending">{{if .Korean}}첫 작업{{else}}First job{{end}}<span class="visually-hidden"> ({{if .Korean}}대기{{else}}pending{{end}})</span></li>
  </ol>
  {{- end}}
  <h1>{{if .Korean}}Runquill 패널 로그인 설정{{else}}Set up the Runquill panel{{end}}</h1>
  {{- if .Manual}}
  <p>{{if .Korean}}패널 로그인에 쓸 GitHub OAuth App을 아래 주소로 등록하세요.{{else}}Register a GitHub OAuth App for panel sign-in with the addresses below.{{end}} <a href="https://github.com/settings/applications/new" target="_blank" rel="noopener noreferrer">{{if .Korean}}GitHub OAuth App 등록 열기{{else}}Open OAuth App registration{{end}}</a></p>
  <dl class="kv">
    <div><dt>{{if .Korean}}홈페이지 URL{{else}}Homepage URL{{end}}</dt><dd><code>{{.Origin}}</code></dd></div>
    <div><dt>{{if .Korean}}인증 콜백 URL{{else}}Authorization callback URL{{end}}</dt><dd><code>{{.Callback}}</code></dd></div>
  </dl>
  <p>{{if .Korean}}앱 설정에서 클라이언트 시크릿을 생성한 뒤 클라이언트 ID와 시크릿을 아래에 입력하세요.{{else}}In the OAuth App settings, generate a client secret, then enter its credentials below.{{end}}</p>
  {{- else}}
  <p>{{if .Korean}}GitHub App을 연결했습니다. 이 Runquill 패널의 첫 관리자가 될 개인 GitHub 계정의 숫자 ID를 입력하세요.{{else}}The GitHub App is connected. Enter the numeric ID of the individual GitHub account that will administer this Runquill panel.{{end}}</p>
  {{- end}}
  <form method="post" action="{{.Form}}">
    <input type="hidden" name="state" value="{{.State}}">
    {{- if .Manual}}
    <div class="field">
      <label for="panel-client-id">{{if .Korean}}클라이언트 ID{{else}}Client ID{{end}}</label>
      <input id="panel-client-id" name="client_id" required maxlength="256" autocomplete="off" spellcheck="false">
    </div>
    <div class="field">
      <label for="panel-client-secret">{{if .Korean}}클라이언트 시크릿{{else}}Client secret{{end}}</label>
      <input id="panel-client-secret" name="client_secret" type="password" required maxlength="256" autocomplete="off">
    </div>
    {{- end}}
    <div class="field">
      <label for="panel-bootstrap-id">{{if .Korean}}첫 관리자의 GitHub 숫자 ID{{else}}First administrator's numeric GitHub user ID{{end}}</label>
      <input id="panel-bootstrap-id" name="bootstrap_id" value="{{.Bootstrap}}" inputmode="numeric" pattern="[0-9]+" maxlength="19" required aria-describedby="panel-bootstrap-help">
      <p class="hint" id="panel-bootstrap-help">{{if .Korean}}https://api.github.com/users/&lt;로그인&gt; 응답의 id 값입니다.{{else}}The id value in the response from https://api.github.com/users/&lt;login&gt;.{{end}}</p>
    </div>
    <div class="actions"><button type="submit" class="button primary">{{if .Korean}}패널 설정 저장{{else}}Save panel configuration{{end}}</button></div>
  </form>
</main>
</body>
</html>`))

func configurePanelSetup(ctx context.Context, out io.Writer, path string, original []byte, values map[string]string, opts setupOptions, ownerID int64) (panelOAuthConfig, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return panelOAuthConfig{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return panelOAuthConfig{}, errors.New("keep the generated .env as a private regular file (chmod 600 .env) before configuring the panel")
	}
	for _, key := range []string{"RUNQUILL_PANEL_URL", "RUNQUILL_PANEL_GITHUB_CLIENT_ID", "RUNQUILL_PANEL_GITHUB_CLIENT_SECRET", "RUNQUILL_PANEL_BOOTSTRAP_ID"} {
		if _, exists := values[key]; exists {
			cfg, err := savedPanelSetup(values, opts.panelURL)
			if err != nil {
				return panelOAuthConfig{}, err
			}
			if _, set := values["RUNQUILL_PANEL_BOOTSTRAP_ID"]; set {
				return finishPanelSetup(path, original, values, opts, cfg, "")
			}
			cfg.bootstrapID = ownerID
			if cfg.bootstrapID <= 0 {
				cfg, err = readPanelSetup(ctx, out, opts.port, cfg)
				if err != nil {
					return panelOAuthConfig{}, err
				}
			}
			return finishPanelSetup(path, original, values, opts, cfg, fmt.Sprintf("RUNQUILL_PANEL_BOOTSTRAP_ID=%d\n", cfg.bootstrapID))
		}
	}
	origin, err := setupPanelOrigin(opts.panelURL)
	if err != nil {
		return panelOAuthConfig{}, err
	}
	cfg, err := readPanelSetup(ctx, out, opts.port, panelOAuthConfig{origin: origin, bootstrapID: ownerID})
	if err != nil {
		return panelOAuthConfig{}, err
	}
	appended := fmt.Sprintf("RUNQUILL_PANEL_URL=%s\nRUNQUILL_PANEL_GITHUB_CLIENT_ID=%s\nRUNQUILL_PANEL_GITHUB_CLIENT_SECRET=%s\nRUNQUILL_PANEL_BOOTSTRAP_ID=%d\n",
		cfg.origin, cfg.clientID, cfg.clientSecret, cfg.bootstrapID)
	return finishPanelSetup(path, original, values, opts, cfg, appended)
}

func setupPanelOrigin(origin string) (string, error) {
	if origin == "" {
		origin = defaultSetupPanelURL
	}
	u, err := parsePanelURL(origin)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func savedPanelSetup(values map[string]string, requestedOrigin string) (panelOAuthConfig, error) {
	cfg, err := parsePanelOAuthConfig(values["RUNQUILL_PANEL_URL"], values["RUNQUILL_PANEL_GITHUB_CLIENT_ID"],
		values["RUNQUILL_PANEL_GITHUB_CLIENT_SECRET"], values["RUNQUILL_PANEL_BOOTSTRAP_ID"])
	if err != nil {
		return panelOAuthConfig{}, fmt.Errorf("existing panel configuration in .env needs attention: %w", err)
	}
	if requestedOrigin != "" {
		u, err := parsePanelURL(requestedOrigin)
		if err != nil {
			return panelOAuthConfig{}, err
		}
		if u.String() != cfg.origin {
			return panelOAuthConfig{}, errors.New("the panel already has a different origin in .env; update it together with the GitHub OAuth callback URL")
		}
	}
	return cfg, nil
}

func finishPanelSetup(path string, original []byte, values map[string]string, opts setupOptions, cfg panelOAuthConfig, appended string) (panelOAuthConfig, error) {
	if opts.kubernetes {
		addr, exists := values["RUNQUILL_PANEL_ADDR"]
		if !exists {
			appended += "RUNQUILL_PANEL_ADDR=0.0.0.0:8081\n"
		} else if addr == "" {
			return panelOAuthConfig{}, errors.New("set RUNQUILL_PANEL_ADDR=0.0.0.0:8081 in .env for the k3s panel")
		}
	}
	if appended == "" {
		return cfg, nil
	}
	if !bytes.HasSuffix(original, []byte("\n")) {
		appended = "\n" + appended
	}
	err := prepareSetupFile(path, append(bytes.Clone(original), appended...), func(tmp string) error {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New(".env changed during panel setup; rerun setup --panel")
		}
		current, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(original, current) {
			return errors.New(".env changed during panel setup; rerun setup --panel")
		}
		return os.Rename(tmp, path)
	})
	if err != nil {
		return panelOAuthConfig{}, fmt.Errorf("save panel settings in .env: %w", err)
	}
	return cfg, nil
}

func readPanelSetup(ctx context.Context, out io.Writer, port int, credentials panelOAuthConfig) (panelOAuthConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return panelOAuthConfig{}, fmt.Errorf("listen for panel setup: %w", err)
	}
	defer listener.Close()
	localURL := "http://" + listener.Addr().String()
	page := struct {
		Origin, Callback, State, Bootstrap, Form string
		Korean, InWizard, Manual                 bool
	}{Origin: credentials.origin, Callback: credentials.callbackURL(), State: rand.Text(), Form: "/", Korean: wizardKorean(ctx), InWizard: currentWizard(ctx) != nil, Manual: credentials.clientID == ""}
	if web := currentWizard(ctx); web != nil {
		localURL, page.Form = web.origin, "/oauth/"
	}
	if credentials.bootstrapID > 0 {
		page.Bootstrap = strconv.FormatInt(credentials.bootstrapID, 10)
	}
	var used atomic.Bool
	saved := make(chan panelOAuthConfig, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Referrer-Policy", "same-origin")
		_ = panelSetupPage.Execute(w, page)
	})
	mux.HandleFunc("POST /{$}", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if r.Header.Get("Origin") != localURL || r.ParseForm() != nil ||
			subtle.ConstantTimeCompare([]byte(r.PostForm.Get("state")), []byte(page.State)) != 1 {
			http.Error(w, "Invalid panel setup request. Return to the setup page and try again.", http.StatusBadRequest)
			return
		}
		id, secret := credentials.clientID, credentials.clientSecret
		if page.Manual {
			id = strings.TrimSpace(r.PostForm.Get("client_id"))
			secret = strings.TrimSpace(r.PostForm.Get("client_secret"))
			if !setupOAuthValue.MatchString(id) || !setupOAuthValue.MatchString(secret) {
				http.Error(w, "Enter the GitHub OAuth client ID and client secret from the app settings.", http.StatusBadRequest)
				return
			}
		}
		cfg, err := parsePanelOAuthConfig(credentials.origin, id, secret, strings.TrimSpace(r.PostForm.Get("bootstrap_id")))
		if err != nil || cfg.bootstrapID == 0 {
			http.Error(w, "Enter a positive numeric GitHub user ID.", http.StatusBadRequest)
			return
		}
		if !used.CompareAndSwap(false, true) {
			http.Error(w, "Runquill is already saving the panel configuration.", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if currentWizard(ctx) != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		} else {
			fmt.Fprintln(w, "Panel settings received. Return to your terminal to finish setup.")
		}
		saved <- cfg
	})
	if web := currentWizard(ctx); web != nil {
		return wizardStep(ctx, web, "/oauth", mux, saved)
	}
	return runSetupBrowser(ctx, out, listener, mux, "to configure GitHub sign-in for the panel", "", saved)
}

func printPanelSetupNext(out io.Writer, cfg panelOAuthConfig, kubernetes bool) {
	if cfg.origin != defaultSetupPanelURL && cfg.origin != "http://localhost:8081" {
		fmt.Fprintln(out, "Forward your panel origin to port 8081 and preserve the original Host header.")
	} else if !kubernetes {
		fmt.Fprintln(out, "For a remote Docker server, forward the panel port over SSH before opening it.")
		fmt.Fprintln(out, "  ssh -N -L 127.0.0.1:8081:127.0.0.1:8081 user@server")
	}
	fmt.Fprintf(out, "Open %s/ and choose Continue with GitHub.\n", cfg.origin)
}
