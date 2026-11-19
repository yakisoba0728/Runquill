package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

var testAPIToken = "rqk_" + strings.Repeat("A", 43)

func TestAPIClientAcceptsOnlyHTTPSOrLoopback(t *testing.T) {
	for _, origin := range []string{"https://panel.example", "https://panel.example:8443/", "http://127.0.0.1:8081", "http://localhost", "http://[::1]:9000"} {
		if _, err := newAPIClient(origin, testAPIToken); err != nil {
			t.Errorf("newAPIClient(%q): %v", origin, err)
		}
	}
	for _, origin := range []string{"", "http://panel.example", "http://10.0.0.5:8081", "ftp://127.0.0.1", "https://user:pass@panel.example",
		"https://panel.example/panel", "https://panel.example/?x=1", "https://panel.example/#x", "panel.example"} {
		if _, err := newAPIClient(origin, testAPIToken); err == nil {
			t.Errorf("newAPIClient(%q) accepted an unsafe origin", origin)
		}
	}
	for _, token := range []string{"", "rqk_short", "Bearer " + testAPIToken, testAPIToken + "\r\nX: y", strings.TrimPrefix(testAPIToken, "rqk_")} {
		_, err := newAPIClient("https://panel.example", token)
		if err == nil {
			t.Errorf("newAPIClient accepted token %q", token)
		} else if token != "" && strings.Contains(err.Error(), token) {
			t.Errorf("the error repeats the token: %v", err)
		}
	}
}

func TestAPIClientSendsTokenAndDecodesErrors(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		switch r.URL.Path {
		case "/snapshot":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"fleet":null,"runner_inventory":null,"recent":null,"poll_failed":false,"polling":true,"webhook":false,"first_job_seen":false}`)
		case "/api/jobs":
			http.Error(w, "You do not have access to this operation.", http.StatusForbidden)
		case "/api/audit":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"bad \u001b[31mfilter"}`)
		case "/html":
			fmt.Fprint(w, "<html>sign in</html>")
		default:
			http.Error(w, strings.Repeat("x", 10<<10), http.StatusTeapot)
		}
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, testAPIToken)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, raw, err := client.snapshot(t.Context())
	if err != nil || !snapshot.Polling || !strings.Contains(string(raw), `"polling":true`) {
		t.Fatalf("snapshot = %+v, %s, %v", snapshot, raw, err)
	}
	if seen.Get("Authorization") != "Bearer "+testAPIToken || seen.Get("Accept") != "application/json" || !strings.HasPrefix(seen.Get("User-Agent"), "runquill-cli/") {
		t.Fatalf("unexpected request headers %v", seen)
	}

	_, _, err = client.jobHistory(t.Context(), jobHistoryQuery{limit: 5})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || apiErr.Message != "You do not have access to this operation." {
		t.Fatalf("jobHistory error = %#v", err)
	}
	_, _, err = client.auditEvents(t.Context(), 5)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest || apiErr.Message != "bad  [31mfilter" {
		t.Fatalf("auditEvents error = %#v", err)
	}
	_, _, err = client.do(t.Context(), http.MethodGet, "/long", nil, nil)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTeapot || len(apiErr.Message) > 310 {
		t.Fatalf("long error = %v", err)
	}
	if _, _, err := getAPIJSON[panelSnapshot](t.Context(), client, "/html", nil); err == nil || !strings.Contains(err.Error(), "did not answer with JSON") {
		t.Fatalf("HTML answer error = %v", err)
	}
	for _, err := range []error{err, apiErr} {
		if strings.Contains(fmt.Sprint(err), testAPIToken) {
			t.Fatalf("an error repeats the token: %v", err)
		}
	}
}

func TestAPIClientBoundsResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jobs":[],"pad":"`+strings.Repeat("x", apiMaxResponse)+`"}`)
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, testAPIToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.jobHistory(t.Context(), jobHistoryQuery{}); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestAPIClientRefusesCrossOriginRedirects(t *testing.T) {
	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
	}))
	defer elsewhere.Close()
	var panel *httptest.Server
	panel = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/snapshot":
			http.Redirect(w, r, elsewhere.URL+"/snapshot", http.StatusFound)
		case "/api/jobs":
			http.Redirect(w, r, "/moved", http.StatusFound)
		case "/moved":
			if r.Header.Get("Authorization") == "" {
				t.Error("same-origin redirect dropped the token")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jobs":[]}`)
		}
	}))
	defer panel.Close()
	client, err := newAPIClient(panel.URL, testAPIToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.snapshot(t.Context()); err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("cross-origin redirect error = %v", err)
	}
	if leaked.Load() != 0 {
		t.Fatal("the client followed a redirect to another origin")
	}
	if _, _, err := client.jobHistory(t.Context(), jobHistoryQuery{}); err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
}

func TestAPIClientUsesHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("request did not use TLS")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, testAPIToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.setHostCordon(t.Context(), "build-1", true); err == nil {
		t.Fatal("the client trusted an unknown certificate")
	}
	client.http.Transport = server.Client().Transport
	if err := client.setHostCordon(t.Context(), "build-1", true); err != nil {
		t.Fatal(err)
	}
}
