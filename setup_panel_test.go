package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestGuidedPanelSetupReusesInstalledAppAndSavesPrivateConfig(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	envPath := filepath.Join(dir, setupEnvFile)
	original := setupEnvironment("owner", 123, "ghcr.io/owner/runquill", "v0.2.1") + "# Keep this comment"
	for path, content := range map[string][]byte{envPath: []byte(original), filepath.Join(dir, setupKeyFile): keyData} {
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/owner/installation" {
			t.Errorf("unexpected GitHub request to %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":1,"account":{"id":321,"login":"owner","type":"User"},"repository_selection":"all","permissions":{"actions":"read","administration":"write"}}`)
	}))
	defer gh.Close()

	const origin = "https://panel.example.com"
	const secret = "fixture-oauth-secret"
	opts := setupOptions{panel: true, panelURL: "https://PANEL.example.com:443/"}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output := make(manifestTestOutput, 32)
	done := make(chan error, 1)
	go func() { done <- setup(ctx, output, dir, gh.Client(), gh.URL, opts) }()

	var browserURL string
	for browserURL == "" {
		select {
		case line := <-output:
			if strings.Contains(line, "in your browser to configure") {
				browserURL = strings.Fields(line)[1]
			}
		case err := <-done:
			t.Fatalf("setup stopped before the OAuth form: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	response, err := gh.Client().Get(browserURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(string(body), origin+panelCallbackPath) || !strings.Contains(string(body), `value="321"`) ||
		!strings.Contains(string(body), `type="password"`) {
		t.Fatalf("invalid panel setup form (HTTP %d, read error %v)", response.StatusCode, err)
	}
	state := regexp.MustCompile(`name="state" value="([^"]+)"`).FindStringSubmatch(string(body))
	if len(state) != 2 {
		t.Fatal("setup form has no state")
	}
	values := url.Values{"state": {state[1]}, "client_id": {"client-fixture"}, "client_secret": {secret}, "bootstrap_id": {"321"}}
	submit := func(from string, expected int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, browserURL, strings.NewReader(values.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", from)
		res, err := gh.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != expected || bytes.Contains(payload, []byte(secret)) {
			t.Fatalf("unexpected form result: HTTP %d, read error %v", res.StatusCode, err)
		}
	}
	submit("https://other.example.com", http.StatusBadRequest)
	values.Set("client_secret", secret+"\nCOMPOSE_FILE=other.yaml")
	submit(strings.TrimSuffix(browserURL, "/"), http.StatusBadRequest)
	values.Set("client_secret", secret)
	submit(strings.TrimSuffix(browserURL, "/"), http.StatusOK)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(output)
	var log strings.Builder
	for line := range output {
		log.WriteString(line)
	}
	if strings.Contains(log.String(), secret) || !strings.Contains(log.String(), "docker compose -f compose.yaml -f compose.panel.yaml up -d --no-build") {
		t.Fatal("setup did not print the panel Compose command without exposing its secret")
	}
	expected := original + "\nRUNQUILL_PANEL_URL=" + origin + "\nRUNQUILL_PANEL_GITHUB_CLIENT_ID=client-fixture\nRUNQUILL_PANEL_GITHUB_CLIENT_SECRET=" +
		secret + "\nRUNQUILL_PANEL_BOOTSTRAP_ID=321\n"
	data, err := os.ReadFile(envPath)
	if err != nil || string(data) != expected {
		t.Fatalf("panel setup did not preserve the existing env and append the settings: %v", err)
	}
	info, err := os.Stat(envPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("panel credentials file is not private: %v", err)
	}

	opts.panelURL = "https://wrong.example.com"
	if err := setup(ctx, io.Discard, dir, gh.Client(), gh.URL, opts); err == nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("expected changing the callback to require editing the saved configuration: %v", err)
	}
	opts.panelURL = ""
	if err := setup(ctx, io.Discard, dir, gh.Client(), gh.URL, opts); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(envPath)
	if err != nil || string(data) != expected {
		t.Fatalf("repeated panel setup changed the existing credentials: %v", err)
	}
	opts.kubernetes = true
	if err := setup(ctx, io.Discard, dir, gh.Client(), gh.URL, opts); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(envPath)
	if err != nil || string(data) != expected+"RUNQUILL_PANEL_ADDR=0.0.0.0:8081\n" {
		t.Fatalf("k3s setup did not add the internal listener and preserve panel credentials: %v", err)
	}
}

func TestOrganizationPanelSetupUsesAppCredentialsAndAsksForAdministrator(t *testing.T) {
	const secret = "manifest-client-secret"
	env := []byte(setupEnvironment("team", 123, "", "") +
		"RUNQUILL_PANEL_URL=https://panel.example.com\nRUNQUILL_PANEL_GITHUB_CLIENT_ID=manifest-client\nRUNQUILL_PANEL_GITHUB_CLIENT_SECRET=" + secret + "\n")
	path := filepath.Join(t.TempDir(), setupEnvFile)
	if err := os.WriteFile(path, env, 0600); err != nil {
		t.Fatal(err)
	}
	values, err := setupValues(env)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output := make(manifestTestOutput, 2)
	done := make(chan error, 1)
	go func() {
		_, err := configurePanelSetup(ctx, output, path, env, values, setupOptions{panel: true, organization: "team"}, 0)
		done <- err
	}()
	var browserURL string
	select {
	case line := <-output:
		browserURL = strings.Fields(line)[1]
	case err := <-done:
		t.Fatalf("setup stopped before asking for an administrator: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	resp, err := http.Get(browserURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	state := regexp.MustCompile(`name="state" value="([^"]+)"`).FindSubmatch(body)
	if err != nil || len(state) != 2 || bytes.Contains(body, []byte(secret)) ||
		bytes.Contains(body, []byte(`name="client_id"`)) || bytes.Contains(body, []byte(`settings/applications/new`)) {
		t.Fatal("organization setup must ask for an administrator only")
	}
	form := url.Values{"state": {string(state[1])}, "bootstrap_id": {"42"}, "client_id": {"other-client"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, browserURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", strings.TrimSuffix(browserURL, "/"))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("administrator form returned HTTP %d", resp.StatusCode)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || string(saved) != string(env)+"RUNQUILL_PANEL_BOOTSTRAP_ID=42\n" {
		t.Fatal("setup must preserve the App credentials and save the first administrator")
	}
}

func TestPanelDoctorChecksConfiguredHostWithoutCredentials(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "panel.example.com" || r.URL.Path != "/api/me" ||
			r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected doctor probe: host %q path %q", r.Host, r.URL.Path)
			http.Error(w, "bad probe", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer app.Close()
	cfg, err := parsePanelOAuthConfig("https://PANEL.example.com:443/", "client", "fixture", "7")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.callbackURL() != "https://panel.example.com/auth/github/callback" {
		t.Fatalf("incorrect canonical callback URL %s", cfg.callbackURL())
	}
	if err := checkPanelListener(t.Context(), strings.TrimPrefix(app.URL, "http://"), cfg); err != nil {
		t.Fatal(err)
	}
	if err := checkPanelListener(t.Context(), "127.0.0.1:0", cfg); err == nil {
		t.Fatal("doctor accepted a listener without a fixed port")
	}
}
