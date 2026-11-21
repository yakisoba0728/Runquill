//go:build unix

package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestRunnerLogsFollowKillsDockerOnDisconnect(t *testing.T) {
	f := newLogFixture(t)
	// The grandchild stands in for the ssh helper docker spawns for ssh:// hosts.
	f.shell = "sleep 60 & echo $! $$; wait"
	server := httptest.NewServer(f.handler)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/runners/runquill-2-abcdef012345/logs?follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(f.cookies["operator"])
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow: HTTP %d", resp.StatusCode)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for field := range strings.FieldsSeq(line) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("pid line %q", line)
		}
		pids = append(pids, pid)
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("process %d not running before disconnect: %v", pid, err)
		}
	}
	cancel()
	for _, pid := range pids {
		waitFor(t, "process "+strconv.Itoa(pid)+" to exit", func() bool { return processExited(pid) })
	}
	waitFor(t, "stream slot release", func() bool { return len(f.logs.streams) == 0 })
}

// processExited reports whether pid is gone or a zombie. The killed grandchild is
// reparented to PID 1, and a container whose PID 1 is not an init, such as
// go test run directly by docker run, never reaps it.
func processExited(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the parenthesised command name.
	end := bytes.LastIndexByte(stat, ')')
	return end >= 0 && len(stat) > end+2 && stat[end+2] == 'Z'
}
