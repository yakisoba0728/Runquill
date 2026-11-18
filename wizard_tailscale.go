package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
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
	if opts.kubernetes {
		return errors.New("managed Tailscale directions require a Docker Compose host")
	}
	if network.serve && opts.panelURL != "" || network.funnel && opts.webhookURL != "" {
		return errors.New("clear the external address when using Tailscale for that endpoint")
	}
	addr, err := readTailscaleName(ctx)
	if err != nil {
		return err
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
			return fmt.Errorf("Tailscale port %s is already used by a different listener", route.port)
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
	for _, route := range network.routes() {
		if err := network.setRoute(ctx, route, enabled); err != nil {
			return err
		}
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

func (network wizardNetwork) setRoute(ctx context.Context, route wizardRoute, enabled bool) error {
	mapping, err := readTailscaleMappings(ctx)
	if err != nil {
		return fmt.Errorf("read Tailscale listeners: %w", err)
	}
	exists, matches := mapping.routeState(network.host, route)
	if exists && !matches {
		return fmt.Errorf("Tailscale port %s is already used by a different listener", route.port)
	}
	if exists == enabled {
		return nil
	}
	args := []string{route.command, "--https=" + route.port, "--set-path=" + route.path}
	if enabled {
		args = append(args, "--bg", route.target)
	} else {
		args = append(args, "off")
	}
	if _, err = wizardTailscale(ctx, args...); err != nil {
		return fmt.Errorf("configure Tailscale %s: %w", route.command, err)
	}
	mapping, err = readTailscaleMappings(ctx)
	if err != nil {
		return fmt.Errorf("verify Tailscale listener: %w", err)
	}
	if exists, matches := mapping.routeState(network.host, route); exists != enabled || enabled && !matches {
		return fmt.Errorf("Tailscale did not report the expected %s listener state on port %s", route.command, route.port)
	}
	return nil
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
