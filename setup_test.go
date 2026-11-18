package main

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
	"io"
	"net/http"
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

func TestSetupCreatesComposeConfiguration(t *testing.T) {
	t.Run("all by default", func(t *testing.T) { testSetupCreatesComposeConfiguration(t, "") })
	t.Run("selected", func(t *testing.T) { testSetupCreatesComposeConfiguration(t, scopeSelected) })
}

func testSetupCreatesComposeConfiguration(t *testing.T, scope string) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	const webhookURL = "https://runner.example.com/webhook"
	const webhookSecret = "generated-webhook-test-secret"
	selected, err := parseRepositoryScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app-manifests/create-test-app/conversions":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":123,"slug":"runquill-test","pem":%q,"webhook_secret":%q,"owner":{"login":"owner","type":"User"}}`, pemData, webhookSecret)
		case "/users/owner/installation":
			fmt.Fprintf(w, `{"id":1,"account":{"type":"User","login":"owner"},"repository_selection":%q,"permissions":{"actions":"read","administration":"write"}}`, selected)
		default:
			t.Errorf("unexpected GitHub call %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer service.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out := make(manifestTestOutput, 16)
	done := make(chan error, 1)
	go func() {
		done <- setup(ctx, out, dir, service.Client(), service.URL, setupOptions{webhookURL: webhookURL, repositoryScope: scope})
	}()
	var browserURL string
	select {
	case line := <-out:
		browserURL = strings.Fields(line)[1]
	case err := <-done:
		t.Fatalf("setup stopped before opening the browser: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	get := func(path string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("unexpected setup HTTP response %d: %v", res.StatusCode, err)
		}
		return string(body)
	}
	form := get(browserURL)
	fields := regexp.MustCompile(`apps/new\?state=([a-zA-Z0-9_-]+)"`).FindStringSubmatch(form)
	if len(fields) != 2 {
		t.Fatal("missing manifest state")
	}
	if !strings.Contains(form, webhookURL) || !strings.Contains(form, "workflow_job") {
		t.Fatal("missing webhook URL or workflow_job subscription")
	}
	query := url.Values{"state": {fields[1]}, "code": {"create-test-app"}}
	get(browserURL + "callback?" + query.Encode())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	expectedEnv := setupEnvironment("owner", 123, releaseImage, version)
	if scope != "" {
		expectedEnv += "RUNQUILL_REPOSITORY_SCOPE=" + scope + "\n"
	}
	want := map[string][]byte{
		setupEnvFile: []byte(expectedEnv +
			"RUNQUILL_WEBHOOK_URL=" + webhookURL + "\nRUNQUILL_WEBHOOK_SECRET=" + webhookSecret + "\nRUNQUILL_POLL_INTERVAL=30s\n"),
		setupKeyFile: pemData,
	}
	for name, expected := range want {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(data, expected) {
			t.Errorf("unexpected generated %s: %v", name, err)
		}
	}
	if scope == scopeSelected {
		var resumed bytes.Buffer
		if err := setup(t.Context(), &resumed, dir, service.Client(), service.URL, setupOptions{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(resumed.String(), "Only select repositories") {
			t.Fatal("resumed setup must report selected repository access")
		}
		err := setup(t.Context(), io.Discard, dir, service.Client(), service.URL, setupOptions{repositoryScope: scopeAll})
		if err == nil || !strings.Contains(err.Error(), repositoryScopeEnv) {
			t.Fatalf("expected actionable scope mismatch on resume, got %v", err)
		}
	}
}

func TestSetupRecoversAndReusesConfiguration(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	config := pendingSetup{
		Env: setupEnvironment("owner", 123, "ghcr.io/owner/runquill", "v0.1.0") +
			"RUNQUILL_WEBHOOK_URL=https://runner.example.com/webhook\nRUNQUILL_WEBHOOK_SECRET=generated-webhook-test-secret\nRUNQUILL_POLL_INTERVAL=0\n",
		PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	}
	pendingPath := filepath.Join(dir, setupPendingFile)
	keyPath := filepath.Join(dir, setupKeyFile)
	envPath := filepath.Join(dir, setupEnvFile)
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a stop after the key was installed and before .env was created.
	if err := writeSetupFile(pendingPath, data); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupFile(keyPath, []byte(config.PEM)); err != nil {
		t.Fatal(err)
	}
	var installed atomic.Bool
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/owner/installation":
			if !installed.Load() {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, `{"id":1,"account":{"type":"User","login":"owner"},"repository_selection":"all","permissions":{"actions":"read","administration":"write"}}`)
		case "/app":
			fmt.Fprint(w, `{"slug":"runquill-test"}`)
		default:
			t.Errorf("unexpected GitHub call %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer service.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out bytes.Buffer
	writer := setupWriteFunc(func(data []byte) (int, error) {
		if bytes.Contains(data, []byte("Waiting for installation")) {
			cancel()
		}
		return out.Write(data)
	})
	if err := setup(ctx, writer, dir, service.Client(), service.URL, setupOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("setup error = %v, want context cancellation", err)
	}
	if !strings.Contains(out.String(), "https://github.com/apps/runquill-test/installations/new") {
		t.Fatalf("missing installation link: %s", out.String())
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected pending file state: %v", err)
	}
	for path, want := range map[string]string{envPath: config.Env, keyPath: config.PEM} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("unexpected contents of %s: %v", filepath.Base(path), err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("want private file %s: %v", filepath.Base(path), err)
		}
	}
	installed.Store(true)
	out.Reset()
	if err := setup(t.Context(), &out, dir, service.Client(), service.URL, setupOptions{}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(envPath); err != nil || string(data) != config.Env {
		t.Fatalf("setup rerun changed the saved webhook-only configuration: %v", err)
	}
	for _, want := range []string{"docker pull ghcr.io/owner/runquill-runner:v0.1.0", "docker compose up -d --no-build", "docker compose exec manager runquill doctor"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in setup output: %s", want, out.String())
		}
	}
}

func TestSetupPreservesManualConfiguration(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, setupEnvFile)
	const envData = "RUNQUILL_OWNER=owner\n"
	if err := os.WriteFile(envPath, []byte(envData), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := setup(t.Context(), &out, dir, nil, "", setupOptions{})
	if err == nil || !strings.Contains(err.Error(), "manual configuration") {
		t.Fatalf("unexpected setup error: %v", err)
	}
	data, err := os.ReadFile(envPath)
	if err != nil || string(data) != envData {
		t.Fatalf("manual .env was changed: %v", err)
	}
}

type setupWriteFunc func([]byte) (int, error)

func (fn setupWriteFunc) Write(data []byte) (int, error) {
	return fn(data)
}
