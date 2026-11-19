package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	wizardServePort  = "9443"
	wizardFunnelPort = "8443"
)

var wizardTailDNS = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]+\.ts\.net$`)

type wizardNetwork struct {
	serve, funnel bool
	host          string
}

type wizardRoute struct {
	command, port, path, target string
}

type tailscaleMappings struct {
	TCP map[string]json.RawMessage
	Web map[string]struct {
		Handlers map[string]struct{ Proxy string }
	}
	AllowFunnel map[string]bool
	Foreground  map[string]tailscaleMappings
}

func (network *wizardNetwork) configure(ctx context.Context, opts *setupOptions) error {
	if !network.serve && !network.funnel {
		return nil
	}
	field := "serve"
	if !network.serve {
		field = "funnel"
	}
	if opts.kubernetes {
		return newWizardIssue("backend", "Tailscale 자동 연결은 Docker Compose 설치에서만 쓸 수 있습니다.", "Managed Tailscale directions require a Docker Compose host.")
	}
	if network.serve && opts.panelURL != "" {
		return newWizardIssue("panel_url", "Tailscale Serve를 쓰려면 패널 주소를 비워 두세요.", "Clear the panel origin when Tailscale Serve provides it.")
	}
	if network.funnel && opts.webhookURL != "" {
		return newWizardIssue("webhook_url", "Tailscale Funnel을 쓰려면 공개 웹훅 URL을 비워 두세요.", "Clear the public webhook URL when Tailscale Funnel provides it.")
	}
	addr, err := readTailscaleName(ctx)
	if err != nil {
		return &wizardIssue{field: field, cause: err,
			ko: "이 호스트에서 Tailscale에 연결하고 MagicDNS를 켠 뒤 다시 시도하세요.",
			en: "Connect Tailscale on this host and enable MagicDNS, then try again."}
	}
	network.host = addr
	if network.serve {
		opts.panelURL = "https://" + net.JoinHostPort(addr, wizardServePort)
	}
	if network.funnel {
		opts.webhookURL = "https://" + net.JoinHostPort(addr, wizardFunnelPort) + "/webhook"
	}
	mappings, err := readTailscaleMappings(ctx)
	if err != nil {
		return fmt.Errorf("read Tailscale listeners: %w", err)
	}
	for _, route := range network.routes() {
		if exists, matches := mappings.routeState(addr, route); exists && !matches {
			return &wizardIssue{field: route.command,
				ko: "다른 리스너가 이미 Tailscale 포트 " + route.port + "을 사용하고 있습니다.",
				en: "Tailscale port " + route.port + " is already used by a different listener."}
		}
	}
	return nil
}

func readTailscaleName(ctx context.Context) (string, error) {
	raw, err := wizardTailscale(ctx, "status", "--json")
	if err != nil {
		return "", err
	}
	var status struct {
		BackendState   string
		Self           struct{ DNSName string }
		CurrentTailnet struct{ MagicDNSEnabled bool }
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", fmt.Errorf("read Tailscale connection: %w", err)
	}
	dns := strings.TrimSuffix(strings.ToLower(status.Self.DNSName), ".")
	if status.BackendState != "Running" || !status.CurrentTailnet.MagicDNSEnabled || !wizardTailDNS.MatchString(dns) {
		return "", errors.New("connect Tailscale on this host and enable MagicDNS before starting the wizard")
	}
	return dns, nil
}

func (network wizardNetwork) routes() []wizardRoute {
	var routes []wizardRoute
	if network.serve {
		routes = append(routes, wizardRoute{"serve", wizardServePort, "/", "http://127.0.0.1:8081"})
	}
	if network.funnel {
		routes = append(routes, wizardRoute{"funnel", wizardFunnelPort, "/webhook", "http://127.0.0.1:8080/webhook"})
	}
	return routes
}

func (network wizardNetwork) apply(ctx context.Context, enabled bool) error {
	if network.host == "" {
		return nil
	}
	name, err := readTailscaleName(ctx)
	if err != nil {
		return err
	}
	if name != network.host {
		return errors.New("the Tailscale host changed after setup; confirm the configuration again")
	}
	var changed []wizardRoute
	for _, route := range network.routes() {
		attempted, err := network.setRoute(ctx, route, enabled)
		if attempted {
			changed = append(changed, route)
		}
		if err == nil {
			continue
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		slices.Reverse(changed)
		for _, previous := range changed {
			if _, rollbackErr := network.setRoute(cleanup, previous, !enabled); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("restore Tailscale %s on port %s: %w", previous.command, previous.port, rollbackErr))
			}
		}
		return err
	}
	return nil
}

func readTailscaleMappings(ctx context.Context) (tailscaleMappings, error) {
	var configuration tailscaleMappings
	raw, err := wizardTailscale(ctx, "serve", "status", "--json")
	if err == nil {
		err = json.Unmarshal(raw, &configuration)
	}
	return configuration, err
}

func (network wizardNetwork) setRoute(ctx context.Context, route wizardRoute, enabled bool) (bool, error) {
	mapping, err := readTailscaleMappings(ctx)
	if err != nil {
		return false, fmt.Errorf("read Tailscale listeners: %w", err)
	}
	exists, matches := mapping.routeState(network.host, route)
	if exists && !matches {
		return false, fmt.Errorf("Tailscale port %s is already used by a different listener", route.port)
	}
	if exists == enabled {
		return false, nil
	}
	args := []string{route.command, "--https=" + route.port, "--set-path=" + route.path}
	if enabled {
		args = append(args, "--bg", route.target)
	} else {
		args = append(args, "off")
	}
	if _, err = wizardTailscale(ctx, args...); err != nil {
		return true, fmt.Errorf("configure Tailscale %s: %w", route.command, err)
	}
	mapping, err = readTailscaleMappings(ctx)
	if err != nil {
		return true, fmt.Errorf("verify Tailscale listener: %w", err)
	}
	if exists, matches := mapping.routeState(network.host, route); exists != enabled || enabled && !matches {
		return true, fmt.Errorf("Tailscale did not report the expected %s listener state on port %s", route.command, route.port)
	}
	return true, nil
}

func (config tailscaleMappings) routeState(host string, route wizardRoute) (exists, matches bool) {
	for _, active := range config.Foreground {
		if exists, _ := active.routeState(host, route); exists {
			return true, false
		}
	}
	name := net.JoinHostPort(host, route.port)
	for hostPort := range config.Web {
		_, port, _ := net.SplitHostPort(hostPort)
		if port == route.port && hostPort != name {
			return true, false
		}
	}
	handlers := config.Web[name].Handlers
	public := config.AllowFunnel[name]
	_, tcp := config.TCP[route.port]
	exists = len(handlers) != 0 || public || tcp
	return exists, len(handlers) == 1 && strings.TrimSuffix(handlers[route.path].Proxy, "/") == route.target && public == (route.command == "funnel")
}

func wizardTailscale(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	result, err := exec.CommandContext(ctx, "tailscale", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Tailscale CLI unavailable or denied the request: %w (%s)", err, strings.TrimSpace(string(result)))
	}
	return result, nil
}
