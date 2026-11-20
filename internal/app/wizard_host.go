package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// wizardHost is what the options step knows about the machine running the wizard.
type wizardHost struct {
	CPUs      int
	MemoryMiB int64 // 0 when the platform does not expose it
}

func detectWizardHost() wizardHost {
	host := wizardHost{CPUs: runtime.NumCPU()}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		host.MemoryMiB = parseMemInfo(data)
	}
	return host
}

// parseMemInfo returns MemTotal from /proc/meminfo in MiB, or 0 when it is absent.
func parseMemInfo(data []byte) int64 {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || kib <= 0 {
				return 0
			}
			return kib / 1024
		}
	}
	return 0
}

// wizardCapacity is how many runners run at once and what each one may use; 0 leaves a limit unset.
type wizardCapacity struct {
	Runners   int
	CPUs      float64
	MemoryMiB int64
}

const (
	wizardHostCPUs      = 2
	wizardHostMemoryMiB = 4096
	wizardRunnerCPUs    = 2    // what a runner is planned with when choosing a count
	wizardRunnerMemMiB  = 2048 // the least memory a runner is planned with
)

// recommendCapacity leaves about two CPUs and 4 GiB for the host and plans one runner per
// two CPUs and 2 GiB of what remains; memory beyond that minimum is shared among them.
func recommendCapacity(host wizardHost) wizardCapacity {
	cpus := max(host.CPUs, 1)
	spareCPU := max(float64(cpus-wizardHostCPUs), float64(cpus)/2)
	runners := int(spareCPU / wizardRunnerCPUs)
	var spareMem int64
	if host.MemoryMiB > 0 {
		spareMem = max(host.MemoryMiB-wizardHostMemoryMiB, host.MemoryMiB/2)
		runners = min(runners, int(spareMem/wizardRunnerMemMiB))
	}
	runners = max(runners, 1)
	capacity := wizardCapacity{Runners: runners, CPUs: max(math.Floor(spareCPU/float64(runners)*2)/2, 0.5)}
	if spareMem > 0 {
		capacity.MemoryMiB = max(spareMem/int64(runners)/512*512, 512)
	}
	return capacity
}

// wizardGiB formats MiB as GiB for a form field, without a trailing ".0".
func wizardGiB(mib int64) string {
	if mib <= 0 {
		return ""
	}
	return strconv.FormatFloat(math.Round(float64(mib)/1024*100)/100, 'f', -1, 64)
}

func wizardCPUText(cpus float64) string {
	if cpus <= 0 {
		return ""
	}
	return strconv.FormatFloat(cpus, 'f', -1, 64)
}

// parseCapacity reads the runner fields of the first step.
func parseCapacity(form wizardForm) (wizardCapacity, error) {
	var capacity wizardCapacity
	if raw := strings.TrimSpace(form.MaxRunners); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1000 {
			return capacity, newWizardIssue("max_runners", "동시 러너 수는 1부터 1000 사이의 정수여야 합니다.", "Enter a whole number of runners between 1 and 1000.")
		}
		capacity.Runners = value
	}
	if raw := strings.TrimSpace(form.RunnerCPUs); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || value < 0.01 || value > 1_000_000 {
			return capacity, newWizardIssue("runner_cpus", "러너당 CPU는 0.01 이상이어야 합니다. 제한하지 않으려면 비워 두세요.", "Use at least 0.01 CPUs per runner, or leave it empty for no limit.")
		}
		capacity.CPUs = math.Round(value*100) / 100
	}
	if raw := strings.TrimSpace(form.RunnerMemory); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || value < 0.01 || value > 1<<30 {
			return capacity, newWizardIssue("runner_memory", "러너당 메모리는 0.01GiB 이상이어야 합니다. 제한하지 않으려면 비워 두세요.", "Use at least 0.01 GiB of memory per runner, or leave it empty for no limit.")
		}
		capacity.MemoryMiB = int64(math.Round(value * 1024))
	}
	return capacity, nil
}

// capacityEnv lists the .env lines that carry the capacity to the manager, skipping keys
// the file already sets so a resumed setup keeps the earlier answer.
func capacityEnv(values map[string]string, kubernetes bool, capacity wizardCapacity) string {
	var lines []string
	add := func(key, value string) {
		if _, exists := values[key]; !exists && value != "" {
			lines = append(lines, key+"="+value)
		}
	}
	if capacity.Runners > 0 {
		add("RUNQUILL_MAX_RUNNERS", strconv.Itoa(capacity.Runners))
	}
	if kubernetes {
		add("RUNQUILL_K8S_JOB_CPU", wizardCPUText(capacity.CPUs))
		if capacity.MemoryMiB > 0 {
			add("RUNQUILL_K8S_JOB_MEMORY", strconv.FormatInt(capacity.MemoryMiB, 10)+"Mi")
		}
	} else if capacity.CPUs > 0 || capacity.MemoryMiB > 0 {
		pool := struct {
			CPUs      float64 `json:"cpus,omitempty"`
			MemoryMiB int64   `json:"memory_mib,omitempty"`
		}{capacity.CPUs, capacity.MemoryMiB}
		data, _ := json.Marshal(map[string]any{defaultWorkerPool: pool})
		// Compose reads the single-quoted value literally, as the README's examples do.
		add("RUNQUILL_WORKER_POOLS", "'"+string(data)+"'")
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// saveCapacity appends the capacity to the generated .env, which must stay private and unchanged meanwhile.
func saveCapacity(dir string, kubernetes bool, capacity wizardCapacity) error {
	path := filepath.Join(dir, setupEnvFile)
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	values, err := setupValues(original)
	if err != nil {
		return err
	}
	appended := capacityEnv(values, kubernetes, capacity)
	if appended == "" {
		return nil
	}
	if !bytes.HasSuffix(original, []byte("\n")) {
		appended = "\n" + appended
	}
	return prepareSetupFile(path, append(bytes.Clone(original), appended...), func(tmp string) error {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New(".env changed while saving runner limits; run the wizard again")
		}
		current, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(original, current) {
			return errors.New(".env changed while saving runner limits; run the wizard again")
		}
		return os.Rename(tmp, path)
	})
}

// wizardDocker is the result of the options step's Docker access check.
type wizardDocker struct {
	State  string // "", "ok", "missing", "permission" or "unreachable"
	Detail string
}

func (d wizardDocker) Blocked() bool { return d.State != "" && d.State != "ok" }

// wizardCommand runs a command and returns its combined output.
type wizardCommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execWizardCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// probeWizardDocker asks the daemon for its version, the cheapest call that needs the same
// socket access as the deployment's docker pull and docker compose.
func probeWizardDocker(ctx context.Context, run wizardCommandRunner) wizardDocker {
	if run == nil {
		return wizardDocker{}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	full := strings.TrimSpace(string(output))
	// Show one line: the one that names the problem, else the last one the CLI printed.
	var detail string
	for line := range strings.SplitSeq(full, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if detail = line; strings.Contains(strings.ToLower(line), "permission denied") || strings.Contains(strings.ToLower(line), "daemon") {
			break
		}
	}
	if len(detail) > 240 {
		detail = strings.ToValidUTF8(detail[:240], "") + "…"
	}
	if err == nil {
		if len(detail) > 40 || strings.ContainsAny(detail, " \t") {
			detail = ""
		}
		return wizardDocker{State: "ok", Detail: detail}
	}
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return wizardDocker{State: "missing"}
	case strings.Contains(strings.ToLower(full), "permission denied"):
		return wizardDocker{State: "permission", Detail: detail}
	default:
		if detail == "" {
			detail = err.Error()
		}
		return wizardDocker{State: "unreachable", Detail: detail}
	}
}

func wizardUser() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		return current.Username
	}
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	return "USER"
}

// wizardSSHTarget turns SSH_CONNECTION ("client port server port") into the ssh arguments
// that reach this server from the user's own computer, or "" outside an SSH session.
func wizardSSHTarget(sshConnection, login string) string {
	fields := strings.Fields(sshConnection)
	if len(fields) != 4 || net.ParseIP(fields[2]) == nil {
		return ""
	}
	if login == "" || strings.ContainsAny(login, " \t@'\"$`\\") {
		login = "USER"
	}
	target := login + "@" + fields[2]
	if sshPort, err := strconv.Atoi(fields[3]); err == nil && sshPort > 0 && sshPort != 22 && sshPort <= 65535 {
		target = "-p " + strconv.Itoa(sshPort) + " " + target
	}
	return target
}

// wizardSSHForward returns the ssh command that forwards port from the user's own computer
// to this server, or "" when the wizard was not started from an SSH session.
func wizardSSHForward(port int, sshConnection, login string) string {
	target := wizardSSHTarget(sshConnection, login)
	if target == "" {
		return ""
	}
	return fmt.Sprintf("ssh -L %[1]d:127.0.0.1:%[1]d %[2]s", port, target)
}

// wizardStartMessage is what the terminal shows when the wizard starts.
func wizardStartMessage(origin, key string, port int, sshConnection, login string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Open %s/?key=%s in your browser.\n", origin, key)
	if forward := wizardSSHForward(port, sshConnection, login); forward != "" {
		fmt.Fprintf(&b, "You are connected over SSH. On your own computer, run this first, then open the URL there:\n  %s\n", forward)
	} else {
		fmt.Fprintf(&b, "On a remote server, forward port %d over SSH before opening the URL.\n", port)
	}
	b.WriteString("The key in the URL is a secret for this setup session. Do not share it.\n")
	return b.String()
}
