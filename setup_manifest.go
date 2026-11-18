package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type manifestApp struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	Owner         struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}

var appManifestPage = template.Must(template.New("manifest").Parse(`<!doctype html>
<html lang="{{if .Korean}}ko{{else}}en{{end}}">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Runquill setup</title></head>
{{if .InWizard}}<link rel="stylesheet" href="/wizard.css">{{end}}
<body><main{{if .InWizard}} class="wizard-card"{{end}}>
{{if .InWizard}}<header class="wizard-header"><span class="brand"><span class="brand-mark" aria-hidden="true"></span>runquill</span><a class="language" href="/">{{if .Korean}}설치 상태{{else}}Setup status{{end}}</a></header><div class="wizard-content">{{end}}
<h1>{{if .Korean}}Runquill과 GitHub 연결{{else}}Connect Runquill to GitHub{{end}}</h1>
{{if .Korean}}<p>GitHub에서 Runquill의 비공개 앱 등록을 승인하세요. 승인 후 이 마법사로 돌아옵니다.</p>{{else}}
<p>Create a private GitHub App with Actions read and {{.RunnerPermission}} write permissions.
{{if .Webhook}}GitHub will send workflow job events to your HTTPS webhook.{{else}}Runquill will discover jobs by polling GitHub.{{end}}</p>{{end}}
{{if .Panel}}<p>{{if .Korean}}이 앱으로 러너를 등록하고 운영 패널에 로그인합니다.{{else}}Runquill uses this app for runner registration and panel sign-in.{{end}}</p>{{end}}
<form method="post" action="{{.Action}}?state={{.State}}">
  <input type="hidden" name="manifest" value="{{.Manifest}}">
  <button type="submit" class="primary">{{if .Korean}}GitHub App 등록{{else}}Create GitHub App{{end}}</button>
</form>
{{if .InWizard}}</div>{{end}}
</main></body>
</html>`))

func registerManifestApp(ctx context.Context, out io.Writer, client *http.Client, apiBase string, opts setupOptions) (manifestApp, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.port)))
	if err != nil {
		return manifestApp{}, fmt.Errorf("listen for GitHub App setup: %w", err)
	}
	defer listener.Close()

	host := listener.Addr().String()
	localURL := "http://" + host
	if web := currentWizard(ctx); web != nil {
		localURL = web.origin + "/connect"
	}
	state := rand.Text()
	action := "https://github.com/settings/apps/new"
	if opts.organization != "" {
		action = "https://github.com/organizations/" + url.PathEscape(opts.organization) + "/settings/apps/new"
	}
	runnerPermission, permissionLabel := "administration", "repository Administration"
	if opts.runnerGroupID > 0 {
		runnerPermission, permissionLabel = organizationRunnerPermission, "organization Self-hosted runners"
	}
	fields := map[string]any{
		"name":            "Runquill-" + rand.Text()[:8],
		"url":             "https://github.com/yakisoba0728/Runquill",
		"redirect_url":    localURL + "/callback",
		"public":          false,
		"hook_attributes": map[string]any{"url": "https://example.com/webhook", "active": false},
		"default_permissions": map[string]string{
			"actions": "read", runnerPermission: "write",
		},
	}
	if opts.webhookURL != "" {
		fields["hook_attributes"] = map[string]any{"url": opts.webhookURL, "active": true}
		fields["default_events"] = []string{"workflow_job"}
	}
	if opts.panel {
		origin, err := setupPanelOrigin(opts.panelURL)
		if err != nil {
			return manifestApp{}, err
		}
		fields["callback_urls"] = []string{origin + panelCallbackPath}
	}
	manifest, err := json.Marshal(fields)
	if err != nil {
		return manifestApp{}, fmt.Errorf("encode GitHub App manifest: %w", err)
	}

	type result struct {
		app manifestApp
		err error
	}
	converted := make(chan result, 1)
	var used atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := struct {
			Action, State, Manifest, RunnerPermission string
			Webhook, Korean, InWizard, Panel          bool
		}{action, state, string(manifest), permissionLabel, opts.webhookURL != "", wizardKorean(ctx), currentWizard(ctx) != nil, opts.panel}
		_ = appManifestPage.Execute(w, page)
	})
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Use GET for the GitHub App setup callback.", http.StatusMethodNotAllowed)
			return
		}
		query := r.URL.Query()
		code := query.Get("code")
		if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 ||
			code == "" || code == "." || code == ".." || len(code) > 512 {
			http.Error(w, "Invalid GitHub App setup callback.", http.StatusBadRequest)
			return
		}
		if !used.CompareAndSwap(false, true) {
			http.Error(w, "Runquill is already processing the GitHub App registration.", http.StatusConflict)
			return
		}
		app, err := convertManifestCode(ctx, client, apiBase, code)
		if err == nil {
			err = checkManifestOwner(app, opts.organization)
		}
		if err == nil && opts.panel && (!setupOAuthValue.MatchString(app.ClientID) || !setupOAuthValue.MatchString(app.ClientSecret)) {
			err = errors.New("GitHub did not return usable panel sign-in credentials")
		}
		if err != nil {
			http.Error(w, "GitHub App registration needs attention. Check the terminal for details.", http.StatusBadGateway)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if currentWizard(ctx) != nil {
				http.Redirect(w, r, "/", http.StatusSeeOther)
			} else {
				fmt.Fprintln(w, "GitHub App registered. Return to your terminal to continue Runquill setup.")
			}
		}
		converted <- result{app, err}
	})

	var res result
	if web := currentWizard(ctx); web != nil {
		res, err = wizardStep(ctx, web, "/connect", mux, converted)
	} else {
		res, err = runSetupBrowser(ctx, out, listener, mux, "to create your GitHub App", " https://github.com", converted)
	}
	if err != nil {
		return manifestApp{}, err
	}
	return res.app, res.err
}

func runSetupBrowser[T any](ctx context.Context, out io.Writer, listener net.Listener, handler http.Handler, action, formOrigin string, finished <-chan T) (T, error) {
	var zero T
	host := listener.Addr().String()
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'"+formOrigin+"; frame-ancestors 'none'; base-uri 'none'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if r.Host != host {
				http.Error(w, "Invalid setup host.", http.StatusBadRequest)
				return
			}
			handler.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       10 * time.Second,
	}
	if _, err := fmt.Fprintf(out, "Open http://%s/ in your browser %s.\n", host, action); err != nil {
		return zero, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if _, err := fmt.Fprintf(out, "For a remote server, forward the setup port from your computer. Then open the URL above.\n  ssh -N -L 127.0.0.1:%[1]d:127.0.0.1:%[1]d user@server\n", port); err != nil {
		return zero, err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			server.Close()
		}
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case res := <-finished:
		return res, nil
	case err := <-serveErr:
		return zero, fmt.Errorf("serve browser setup: %w", err)
	case <-ctx.Done():
		return zero, fmt.Errorf("wait for browser setup: %w", ctx.Err())
	}
}

func convertManifestCode(ctx context.Context, client *http.Client, apiBase, code string) (manifestApp, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var app manifestApp
	endpoint := apiBase + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return app, errors.New("build GitHub App manifest conversion request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Runquill")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	conversionClient := *client
	conversionClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	res, err := conversionClient.Do(req)
	if err != nil {
		return app, errors.New("contact GitHub to convert the App manifest")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		return app, fmt.Errorf("GitHub App manifest conversion returned HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&app); err != nil {
		return manifestApp{}, errors.New("decode GitHub App manifest conversion response")
	}
	if app.ID <= 0 || app.Slug == "" || app.Owner.Login == "" || app.PEM == "" {
		return manifestApp{}, errors.New("expected complete GitHub App credentials")
	}
	return app, nil
}

func checkManifestOwner(app manifestApp, organization string) error {
	if organization != "" {
		if app.Owner.Type != "Organization" || !strings.EqualFold(app.Owner.Login, organization) {
			return fmt.Errorf("GitHub did not return an App registered to the organization %s", organization)
		}
	} else if app.Owner.Type != "User" || !setupLogin.MatchString(app.Owner.Login) {
		return errors.New("GitHub did not return an App registered to a personal account")
	}
	return nil
}
