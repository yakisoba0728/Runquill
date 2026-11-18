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
	"sync/atomic"
	"time"
)

type manifestApp struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	Owner         struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}

var appManifestPage = template.Must(template.New("manifest").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Runquill setup</title></head>
<body>
<h1>Connect Runquill to GitHub</h1>
<p>Create a private GitHub App with Actions read and Administration write permissions.
{{if .Webhook}}GitHub will send workflow job events to your HTTPS webhook.{{else}}Runquill will discover jobs by polling GitHub.{{end}}</p>
<form method="post" action="https://github.com/settings/apps/new?state={{.State}}">
  <input type="hidden" name="manifest" value="{{.Manifest}}">
  <button type="submit">Create GitHub App</button>
</form>
</body>
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
	state := rand.Text()
	fields := map[string]any{
		"name":            "Runquill-" + rand.Text()[:8],
		"url":             "https://github.com/yakisoba0728/Runquill",
		"redirect_url":    localURL + "/callback",
		"public":          false,
		"hook_attributes": map[string]any{"url": "https://example.com/webhook", "active": false},
		"default_permissions": map[string]string{
			"actions": "read", "administration": "write",
		},
	}
	if opts.webhookURL != "" {
		fields["hook_attributes"] = map[string]any{"url": opts.webhookURL, "active": true}
		fields["default_events"] = []string{"workflow_job"}
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
			State, Manifest string
			Webhook         bool
		}{state, string(manifest), opts.webhookURL != ""}
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
		if err != nil {
			http.Error(w, "GitHub App registration needs attention. Check the terminal for details.", http.StatusBadGateway)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "GitHub App registered. Return to your terminal to continue Runquill setup.")
		}
		converted <- result{app, err}
	})

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self' https://github.com; frame-ancestors 'none'; base-uri 'none'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if r.Host != host {
				http.Error(w, "Invalid setup host.", http.StatusBadRequest)
				return
			}
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       10 * time.Second,
	}
	if _, err := fmt.Fprintf(out, "Open %s/ in your browser to create your GitHub App.\n", localURL); err != nil {
		return manifestApp{}, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if _, err := fmt.Fprintf(out, "For a remote server, forward the setup port from your computer. Then open the URL above.\n  ssh -N -L 127.0.0.1:%[1]d:127.0.0.1:%[1]d user@server\n", port); err != nil {
		return manifestApp{}, err
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
	case res := <-converted:
		return res.app, res.err
	case err := <-serveErr:
		return manifestApp{}, fmt.Errorf("serve GitHub App setup: %w", err)
	case <-ctx.Done():
		return manifestApp{}, fmt.Errorf("wait for GitHub App registration: %w", ctx.Err())
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
	if app.ID <= 0 || app.Slug == "" || app.Owner.Login == "" || app.Owner.Type != "User" || app.PEM == "" {
		return manifestApp{}, errors.New("expected GitHub App credentials for a personal account")
	}
	return app, nil
}
