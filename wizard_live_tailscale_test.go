package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// The operator must have approved the temporary listeners on this tailnet.
func TestLiveWizardTailscale(t *testing.T) {
	if os.Getenv("RUNQUILL_TEST_TAILSCALE") != "1" {
		t.Skip("requires a test tailnet with Serve and Funnel enabled and reserved ports unused")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	opts := setupOptions{panel: true}
	routes := wizardNetwork{serve: true, funnel: true}
	if err := routes.configure(ctx, &opts); err != nil {
		t.Fatal(err)
	}
	current, err := readTailscaleMappings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes.routes() {
		if exists, _ := current.routeState(routes.host, route); exists {
			t.Fatalf("test requires unused Tailscale port %s", route.port)
		}
	}
	for _, addr := range []string{"127.0.0.1:8080", "127.0.0.1:8081"} {
		listener, err := net.Listen("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{ReadHeaderTimeout: 3 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case addr == "127.0.0.1:8080" && r.Method == http.MethodPost && r.URL.Path == "/webhook":
				w.WriteHeader(http.StatusAccepted)
			case addr == "127.0.0.1:8081" && r.Method == http.MethodGet && r.URL.Path == "/":
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		})}
		go server.Serve(listener)
		t.Cleanup(func() { server.Close() })
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 35*time.Second)
		defer stop()
		if err := routes.apply(cleanup, false); err != nil {
			t.Errorf("remove temporary Tailscale listeners: %v", err)
		}
	})
	if err := routes.apply(ctx, true); err != nil {
		t.Fatal(err)
	}
	t.Log("Tailscale configured a tailnet-only panel and a public webhook-only handler")
	webhook, err := url.Parse(opts.webhookURL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, target := range []struct {
		method, address string
		status          int
	}{
		{http.MethodGet, opts.panelURL + "/", http.StatusNoContent},
		{http.MethodPost, webhook.String(), http.StatusAccepted},
		{http.MethodGet, "https://" + webhook.Host + "/", http.StatusNotFound},
	} {
		result := errors.New("no successful response before timeout")
		for attempt := 0; attempt < 6 && ctx.Err() == nil; attempt++ {
			req, _ := http.NewRequestWithContext(ctx, target.method, target.address, nil)
			res, err := client.Do(req)
			result = err
			if res != nil {
				res.Body.Close()
				if res.StatusCode == target.status {
					result = nil
					break
				}
				result = &url.Error{Op: "unexpected response " + res.Status, URL: req.URL.Path, Err: http.ErrNotSupported}
			}
			time.Sleep(2 * time.Second)
		}
		if result != nil {
			t.Fatal(result)
		}
		t.Logf("%s reached expected HTTP %d over verified TLS", target.method, target.status)
	}
}
