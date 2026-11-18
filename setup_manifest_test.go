package main

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

type manifestTestOutput chan string

func (o manifestTestOutput) Write(p []byte) (int, error) {
	o <- string(p)
	return len(p), nil
}

func TestRegisterManifestApp(t *testing.T) {
	converting := make(chan struct{}, 1)
	finish := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/test-code/conversions" {
			t.Errorf("unexpected conversion request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("manifest conversion received an authorization header")
		}
		converting <- struct{}{}
		select {
		case <-finish:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":123,"slug":"runquill-test","pem":"fixture-key","owner":{"login":"alice","type":"User"}}`)
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output := make(manifestTestOutput, 2)
	apps := make(chan manifestApp, 1)
	errs := make(chan error, 1)
	go func() {
		app, err := registerManifestApp(ctx, output, api.Client(), api.URL, setupOptions{})
		apps <- app
		errs <- err
	}()

	var browserURL string
	select {
	case line := <-output:
		browserURL = strings.Fields(line)[1]
	case err := <-errs:
		t.Fatalf("setup stopped before opening the browser: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	browser := &http.Client{Timeout: 2 * time.Second}
	request := func(method, address, host string) (int, string, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, address, nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		res, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, string(body), res.Header
	}
	if status, _, _ := request(http.MethodGet, browserURL, "setup.invalid"); status != http.StatusBadRequest {
		t.Fatalf("wrong Host accepted with HTTP %d", status)
	}
	status, page, headers := request(http.MethodGet, browserURL, "")
	if status != http.StatusOK || headers.Get("Cache-Control") != "no-store" || headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("setup page has HTTP %d and headers %v", status, headers)
	}
	form := regexp.MustCompile(`action="https://github.com/settings/apps/new\?state=([a-zA-Z0-9_-]+)"`).FindStringSubmatch(page)
	field := regexp.MustCompile(`name="manifest" value="([^"]+)"`).FindStringSubmatch(page)
	if len(form) != 2 || len(form[1]) < 22 || len(field) != 2 {
		t.Fatalf("could not find the registration form, token, and manifest: %s", page)
	}
	var manifest struct {
		Public         *bool
		RedirectURL    string                 `json:"redirect_url"`
		HookAttributes struct{ Active *bool } `json:"hook_attributes"`
		Permissions    map[string]string      `json:"default_permissions"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(field[1])), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Public == nil || *manifest.Public || manifest.HookAttributes.Active == nil || *manifest.HookAttributes.Active ||
		manifest.RedirectURL != browserURL+"callback" ||
		!reflect.DeepEqual(manifest.Permissions, map[string]string{"actions": "read", "administration": "write"}) {
		t.Fatalf("unexpected App configuration: %+v", manifest)
	}
	callback := manifest.RedirectURL + "?" + url.Values{"code": {"test-code"}, "state": {form[1]}}.Encode()
	if status, _, _ := request(http.MethodGet, manifest.RedirectURL+"?code=test-code&state=wrong", ""); status != http.StatusBadRequest {
		t.Fatalf("wrong state accepted with HTTP %d", status)
	}
	if status, _, _ := request(http.MethodHead, callback, ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD callback accepted with HTTP %d", status)
	}

	type response struct {
		res *http.Response
		err error
	}
	responses := make(chan response, 1)
	go func() {
		res, err := browser.Get(callback)
		responses <- response{res, err}
	}()
	select {
	case <-converting:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if status, _, _ := request(http.MethodGet, callback, ""); status != http.StatusConflict {
		t.Fatalf("duplicate conversion accepted with HTTP %d", status)
	}
	close(finish)
	result := <-responses
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.res.Body.Close()
	body, err := io.ReadAll(result.res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if result.res.StatusCode != http.StatusOK || strings.Contains(string(body), "fixture-key") || strings.Contains(string(body), "test-code") {
		t.Fatalf("unexpected callback result HTTP %d: %s", result.res.StatusCode, body)
	}
	app, err := <-apps, <-errs
	if err != nil || app.ID != 123 || app.Slug != "runquill-test" || app.Owner.Login != "alice" || app.PEM != "fixture-key" {
		t.Fatalf("manifest conversion returned unexpected credentials or error: %v", err)
	}
}

func TestConvertManifestCodeKeepsCredentialsOutOfErrors(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "response may contain a PEM", http.StatusUnprocessableEntity)
	}))
	defer api.Close()
	if _, err := convertManifestCode(t.Context(), api.Client(), api.URL, "secret-code"); err == nil ||
		!strings.Contains(err.Error(), "HTTP 422") ||
		strings.Contains(err.Error(), "secret-code") || strings.Contains(err.Error(), "PEM") {
		t.Fatalf("unexpected conversion error: %v", err)
	}
}
