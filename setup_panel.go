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
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Runquill panel setup</title></head>
{{if .InWizard}}<link rel="stylesheet" href="/wizard.css">{{end}}
<body><main{{if .InWizard}} class="wizard-card"{{end}}>
{{if .InWizard}}<header class="wizard-header"><span class="brand"><span class="brand-mark" aria-hidden="true"></span>runquill</span><a class="language" href="/">{{if .Korean}}설치 상태{{else}}Setup status{{end}}</a></header><div class="wizard-content">{{end}}
<h1>{{if .Korean}}Runquill 패널 로그인 설정{{else}}Set up the Runquill panel{{end}}</h1>
{{if .Manual}}
<p><a href="https://github.com/settings/applications/new" target="_blank" rel="noopener noreferrer">{{if .Korean}}GitHub OAuth App 등록 열기{{else}}Register a GitHub OAuth App{{end}}</a>{{if not .Korean}} for panel sign-in.{{end}}</p>
<dl>
  <dt>{{if .Korean}}홈페이지 URL{{else}}Homepage URL{{end}}</dt><dd><code>{{.Origin}}</code></dd>
  <dt>{{if .Korean}}인증 콜백 URL{{else}}Authorization callback URL{{end}}</dt><dd><code>{{.Callback}}</code></dd>
</dl>
<p>{{if .Korean}}위 주소를 GitHub 설정에 입력하고 생성한 클라이언트 시크릿을 여기에 입력하세요.{{else}}In the OAuth App settings, generate a client secret, then enter its credentials below.{{end}}</p>
{{else}}
<p>{{if .Korean}}GitHub App이 연결되었습니다. 조직의 첫 Runquill 관리자로 등록할 개인 계정의 숫자 ID를 입력하세요.{{else}}The GitHub App is connected. Enter the numeric ID of the individual GitHub account that will administer this Runquill panel.{{end}}</p>
{{end}}
<form method="post" action="{{.Form}}">
  <input type="hidden" name="state" value="{{.State}}">
  {{if .Manual}}
  <p><label>{{if .Korean}}클라이언트 ID{{else}}Client ID{{end}} <input name="client_id" required maxlength="256" autocomplete="off" spellcheck="false"></label></p>
  <p><label>{{if .Korean}}클라이언트 시크릿{{else}}Client secret{{end}} <input name="client_secret" type="password" required maxlength="256" autocomplete="off"></label></p>
  {{end}}
  <p><label>{{if .Korean}}첫 관리자의 GitHub 숫자 ID{{else}}First administrator's numeric GitHub user ID{{end}} <input name="bootstrap_id" value="{{.Bootstrap}}" inputmode="numeric" pattern="[0-9]+" maxlength="19" required></label></p>
  <button type="submit" class="primary">{{if .Korean}}패널 설정 저장{{else}}Save panel configuration{{end}}</button>
</form>
{{if .InWizard}}</div>{{end}}
</main></body>
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
