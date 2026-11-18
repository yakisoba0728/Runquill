package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetricsListenerIsolation(t *testing.T) {
	if endpoint, err := openMetrics("", "unused-file"); err != nil || endpoint != nil {
		t.Fatalf("disabled listener: %v, %v", endpoint, err)
	}
	for _, addr := range []string{":0", "localhost:0", "0.0.0.0:0", "[::]:0", "127.0.0.1:", "127.0.0.1:http"} {
		if endpoint, err := openMetrics(addr, ""); err == nil {
			endpoint.listener.Close()
			t.Errorf("accepted unprotected address %q", addr)
		}
	}
	local, err := openMetrics("127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.listener.Close() })
	for _, check := range []struct {
		url  string
		code int
	}{
		{"http://rebound.invalid/metrics", http.StatusForbidden},
		{"http://127.0.0.1/metrics", http.StatusOK},
		{"http://127.0.0.1/webhook", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		local.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, check.url, nil))
		if rec.Code != check.code {
			t.Errorf("%s: got %d, want %d", check.url, rec.Code, check.code)
		}
	}
}

func TestMetricsAuthenticatedScrape(t *testing.T) {
	token := strings.Repeat("a", 64)
	tokenFile := filepath.Join(t.TempDir(), "metrics-token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint, err := openMetrics("127.0.0.1:0", tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		endpoint.server.Serve(endpoint.listener)
	}()
	t.Cleanup(func() {
		endpoint.server.Close()
		<-done
	})
	target := "http://" + endpoint.listener.Addr().String() + "/metrics"
	for _, check := range []struct {
		token string
		code  int
	}{
		{"", http.StatusUnauthorized},
		{"wrong-token", http.StatusUnauthorized},
		{token, http.StatusOK},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if check.token != "" {
			req.Header.Set("Authorization", "Bearer "+check.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != check.code {
			t.Fatalf("scrape status %d, want %d: %v", resp.StatusCode, check.code, err)
		}
		if check.code == http.StatusOK {
			if resp.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" ||
				!strings.Contains(string(body), "\nrunquill_runner_starts_total 0\n") || strings.Contains(string(body), token) {
				t.Errorf("unexpected metrics response: %s", body)
			}
		} else if strings.Contains(string(body), "runquill_") {
			t.Errorf("unauthenticated scrape exposed metrics: %s", body)
		}
	}
}
