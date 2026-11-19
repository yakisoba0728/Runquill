package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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

func (web *setupWizard) prepareNetwork(ctx context.Context, opts *setupOptions) func() {
	noop := func() {}
	web.mu.Lock()
	skip := web.skipTailscale || web.form.LocalOnly || opts.kubernetes || web.command == nil
	web.network = wizardNetwork{}
	web.serveFailed, web.funnelFailed = false, false
	web.mu.Unlock()
	if skip {
		return noop
	}
	network := discoverWizardNetwork(ctx)
	cleanup := noop
	// Saved and recoverable GitHub App callbacks determine the routes on resume.
	if !web.hasSetupConfig() {
		network.serve = network.serve && opts.panelURL == ""
		network.funnel = network.funnel && opts.webhookURL == ""
		if network.serve {
			listen := web.listen
			if listen == nil {
				listen = net.Listen
			}
			attempted, err := network.preparePanel(ctx, listen)
			if attempted {
				cleanup = func() {
					if web.hasSetupConfig() {
						return
					}
					rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
					defer cancel()
					if _, err := network.setRoute(rollback, network.routes()[0], false); err != nil {
						fmt.Fprintf(web.log, "Remove temporary Tailscale Serve route: %v\n", err)
					}
				}
			}
			if err != nil {
				fmt.Fprintf(web.log, "Tailscale Serve unavailable; using the local panel: %v\n", err)
				cleanup()
				cleanup = noop
				network.serve = false
			} else {
				opts.panelURL = network.panelURL()
			}
		}
		if network.funnel {
			opts.webhookURL = network.webhookURL()
		}
	}
	web.mu.Lock()
	web.network = network
	web.mu.Unlock()
	return cleanup
}

func (web *setupWizard) hasSetupConfig() bool {
	for _, file := range []string{setupEnvFile, setupPendingFile} {
		if _, err := os.Lstat(filepath.Join(web.dir, file)); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func discoverWizardNetwork(ctx context.Context) wizardNetwork {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	tailnet, err := readTailnet(ctx)
	if err != nil {
		return wizardNetwork{}
	}
	network := wizardNetwork{host: tailnet.Name}
	if !tailnet.HTTPS || !wizardCanManageTailscale(ctx) {
		return network
	}
	mapping, err := readTailscaleMappings(ctx)
	if err != nil {
		return network
	}
	network.serve, network.funnel = true, tailnet.Funnel
	for _, route := range network.routes() {
		if exists, matches := mapping.routeState(network.host, route); exists && !matches {
			if route.command == "serve" {
				network.serve = false
			} else {
				network.funnel = false
			}
		}
	}
	return network
}

func wizardCanManageTailscale(ctx context.Context) bool {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		return true
	}
	raw, err := wizardTailscale(ctx, "debug", "prefs")
	var prefs struct{ OperatorUser string }
	return err == nil && json.Unmarshal(raw, &prefs) == nil && prefs.OperatorUser == wizardUser()
}

type wizardTailnet struct {
	Name          string
	HTTPS, Funnel bool
}

func readTailnet(ctx context.Context) (wizardTailnet, error) {
	raw, err := wizardTailscale(ctx, "status", "--json")
	if err != nil {
		return wizardTailnet{}, err
	}
	var status struct {
		BackendState string
		Self         struct {
			DNSName      string
			CapMap       map[string]json.RawMessage
			Capabilities []string
		}
		CurrentTailnet struct{ MagicDNSEnabled bool }
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return wizardTailnet{}, fmt.Errorf("read Tailscale connection: %w", err)
	}
	dns := strings.TrimSuffix(strings.ToLower(status.Self.DNSName), ".")
	if status.BackendState != "Running" || !status.CurrentTailnet.MagicDNSEnabled || !wizardTailDNS.MatchString(dns) {
		return wizardTailnet{}, errors.New("connect Tailscale on this host and enable MagicDNS before starting the wizard")
	}
	// Tailscale grants these as node attributes; older clients list them as capability URLs.
	granted := func(name string) bool {
		for _, key := range []string{name, "https://tailscale.com/cap/" + name} {
			if _, ok := status.Self.CapMap[key]; ok || slices.Contains(status.Self.Capabilities, key) {
				return true
			}
		}
		return false
	}
	https := granted("https")
	return wizardTailnet{Name: dns, HTTPS: https, Funnel: https && granted("funnel")}, nil
}

func (network wizardNetwork) panelURL() string {
	return "https://" + net.JoinHostPort(network.host, wizardServePort)
}
func (network wizardNetwork) webhookURL() string {
	return "https://" + net.JoinHostPort(network.host, wizardFunnelPort) + "/webhook"
}

// Connect Serve before choosing the permanent GitHub App OAuth callback.
// Reserve an unused backend port while first exposing it to the tailnet.
func (network wizardNetwork) preparePanel(ctx context.Context, listen func(string, string) (net.Listener, error)) (bool, error) {
	route := network.routes()[0]
	listener, err := listen("tcp4", strings.TrimPrefix(route.target, "http://"))
	if err != nil {
		return false, fmt.Errorf("reserve the Runquill panel port: %w", err)
	}
	defer listener.Close()
	return network.setRoute(ctx, route, true)
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

func (network wizardNetwork) activate(ctx context.Context, log io.Writer) (serveFailed, funnelFailed bool) {
	tailnet, err := readTailnet(ctx)
	if err == nil && tailnet.Name != network.host {
		err = errors.New("the Tailscale host changed after setup")
	}
	for _, route := range network.routes() {
		routeErr := err
		if routeErr == nil && (!tailnet.HTTPS || route.command == "funnel" && !tailnet.Funnel) {
			routeErr = fmt.Errorf("tailnet no longer allows %s", route.command)
		}
		if routeErr == nil {
			_, routeErr = network.setRoute(ctx, route, true)
		}
		if routeErr == nil {
			continue
		}
		fmt.Fprintf(log, "Tailscale %s: %v\n", route.command, routeErr)
		if route.command == "serve" {
			serveFailed = true
		} else {
			funnelFailed = true
		}
	}
	return serveFailed, funnelFailed
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
		args = append(args, "--yes", "--bg", route.target)
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
