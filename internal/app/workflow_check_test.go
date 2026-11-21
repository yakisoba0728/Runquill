package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yakisoba0728/Runquill/internal/workflow"
)

const routeBuild = "${{ format('runquill-{0}-{1}-build', github.run_id, github.run_attempt) }}"

func codesOf(findings []workflow.Finding) []string {
	var codes []string
	for _, f := range findings {
		codes = append(codes, f.Job+":"+f.Code+":"+f.Severity)
	}
	slices.Sort(codes)
	return codes
}

func TestCheckWorkflowFindings(t *testing.T) {
	pools := workerPools{"default": {}, "lint": {Privileged: new(false)}}
	for _, test := range []struct {
		name, src string
		pools     workerPools
		want      []string
	}{
		{"clean", "on: push\npermissions: {}\njobs:\n  a:\n    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    steps:\n      - uses: actions/checkout@v4\n", nil, nil},
		{"missing permissions and timeout", "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n", nil,
			[]string{":permissions:warning", "a:timeout:warning"}},
		{"unpinned third-party", "permissions: {}\njobs:\n  a:\n    runs-on: ubuntu-latest\n    timeout-minutes: 1\n    steps:\n      - uses: docker/build-push-action@v6 # tag\n      - name: x\n        uses: \"owner/tool@8f4b7f84864484a7bf31766abe9204da3cbe65b3\"\n      - uses: ./local\n  call:\n    uses: other/repo/.github/workflows/x.yml@main\n", nil,
			[]string{"a:unpinned-action:info", "call:unpinned-action:info"}},
		{"pull_request_target on runquill", "on: [pull_request_target]\npermissions: {}\njobs:\n  a:\n    runs-on: " + routeBuild + "\n    timeout-minutes: 1\n", nil,
			[]string{"a:untrusted-trigger:high"}},
		{"workflow_run on self-hosted", "on:\n  workflow_run:\n    workflows: [x]\npermissions: {}\njobs:\n  a:\n    runs-on: [self-hosted, linux]\n    timeout-minutes: 1\n", nil,
			[]string{"a:runs-on:warning", "a:untrusted-trigger:high"}},
		{"pull_request on group", "on:\n  - pull_request\npermissions: {}\njobs:\n  a:\n    runs-on:\n      group: big\n    timeout-minutes: 1\n", nil,
			[]string{"a:fork-trigger:warning"}},
		{"pull_request_target on hosted", "on: pull_request_target\npermissions: {}\njobs:\n  a:\n    runs-on: ubuntu-latest\n    timeout-minutes: 1\n", nil, nil},
		{"static runquill label", "permissions: {}\njobs:\n  a:\n    runs-on: runquill-1-1-a\n    timeout-minutes: 1\n", nil,
			[]string{"a:runs-on:warning"}},
		{"strategy without matrix", "permissions: {}\njobs:\n  a:\n    strategy: {fail-fast: false}\n    runs-on: " + routeBuild + "\n    timeout-minutes: 1\n", nil, nil},
		{"matrix without index","permissions: {}\njobs:\n  a:\n    strategy:\n      matrix: {x: [1]}\n    runs-on: " + routeBuild + "\n    timeout-minutes: 1\n", nil,
			[]string{"a:runs-on:warning"}},
		{"bad pool suffix", "permissions: {}\njobs:\n  a:\n    runs-on: ${{ format('runquill-{0}-{1}-pool__lint', github.run_id, github.run_attempt) }}\n    timeout-minutes: 1\n", nil,
			[]string{"a:runs-on:warning"}},
		{"unknown pool", "permissions: {}\njobs:\n  a:\n    runs-on: ${{ format('runquill-{0}-{1}-pool__gpu__a', github.run_id, github.run_attempt) }}\n    timeout-minutes: 1\n", pools,
			[]string{"a:pool:warning"}},
		{"docker on unprivileged pool", "permissions: {}\njobs:\n  a:\n    runs-on: ${{ format('runquill-{0}-{1}-pool__lint__a', github.run_id, github.run_attempt) }}\n    timeout-minutes: 1\n    steps:\n      - run: |\n          docker build .\n  b:\n    runs-on: ${{ format('runquill-{0}-{1}-pool__lint__b', github.run_id, github.run_attempt) }}\n    timeout-minutes: 1\n    services:\n      db: {image: postgres}\n  c:\n    runs-on: ${{ format('runquill-{0}-{1}-pool__lint__c', github.run_id, github.run_attempt) }}\n    timeout-minutes: 1\n    steps:\n      - run: echo dockerfile\n  d:\n    runs-on: " + routeBuild + "\n    timeout-minutes: 1\n    container: node:22\n", pools,
			[]string{"a:docker:warning", "b:docker:warning"}},
		{"unparsed file", "jobs:\n\ta:\n", nil, []string{":unparsed:warning"}},
		{"unparsed job", "permissions: {}\njobs:\n  a: {runs-on: x}\n", nil, []string{"a:unparsed:info"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := codesOf(workflow.Check("ci.yml", []byte(test.src), workflowCheckOptions(test.pools, nil)))
			want := slices.Clone(test.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

func TestCheckWorkflowLinesAndExamples(t *testing.T) {
	src := "on: pull_request_target\njobs:\n  a:\n    timeout-minutes: 1\n    runs-on: self-hosted\n"
	findings := workflow.Check("ci.yml", []byte(src), workflow.CheckOptions{})
	lines := map[string]int{}
	for _, f := range findings {
		lines[f.Code] = f.Line
	}
	if lines["permissions"] != 1 || lines["untrusted-trigger"] != 5 || lines["runs-on"] != 5 {
		t.Fatalf("line numbers: %+v", findings)
	}
	files, _ := filepath.Glob("../../examples/*.yml")
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range workflow.Check(path, src, workflow.CheckOptions{}) {
			if f.Severity != "info" {
				t.Errorf("%s: shipped example has finding %+v", path, f)
			}
		}
	}
}

func TestWorkflowCheckCommand(t *testing.T) {
	dir := t.TempDir()
	workflows := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(workflows, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workflows, name), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}
	noEnv := func(string) string { return "" }
	run := func(env func(string) string, args ...string) (int, string, string) {
		var out, errs bytes.Buffer
		code := workflowCheckCommand(args, dir, &out, &errs, env)
		return code, out.String(), errs.String()
	}
	if code, _, errs := run(noEnv); code != 0 || errs != "" {
		t.Fatalf("empty directory: %d %s", code, errs)
	}
	write("ok.yml", "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n")
	write("notes.txt", "jobs: [")
	code, out, _ := run(noEnv)
	if code != 0 || !strings.Contains(out, ".github/workflows/ok.yml:1: warning [permissions]") ||
		!strings.Contains(out, "Checked 1 workflow files: 2 findings, 0 high severity.") {
		t.Fatalf("warnings only: %d\n%s", code, out)
	}
	if code, _, _ := run(noEnv, "--strict"); code != 1 {
		t.Fatalf("--strict exit %d", code)
	}
	write("prt.yaml", "on: pull_request_target\npermissions: {}\njobs:\n  a:\n    runs-on: [self-hosted]\n    timeout-minutes: 1\n")
	code, out, _ = run(noEnv, "--json")
	var report struct {
		Files    int
		Findings []workflow.Finding
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || code != 1 || report.Files != 2 {
		t.Fatalf("json: %d %v\n%s", code, err, out)
	}
	if code, _, _ := run(noEnv, ".github/workflows/ok.yml"); code != 0 {
		t.Fatalf("single file: exit %d", code)
	}
	if code, _, _ := run(noEnv, "missing.yml"); code != 2 {
		t.Fatalf("missing file: exit %d", code)
	}
	if code, _, _ := run(noEnv, "--bogus"); code != 2 {
		t.Fatalf("bad flag: exit %d", code)
	}
	badPools := func(key string) string {
		if key == "RUNQUILL_WORKER_POOLS" {
			return "{"
		}
		return ""
	}
	if _, _, errs := run(badPools, ".github/workflows/ok.yml"); !strings.Contains(errs, "pool checks are skipped") {
		t.Fatalf("bad pools: %s", errs)
	}
	if code, _, errs := workflowCheckCommandIn(t.TempDir()); code != 2 || !strings.Contains(errs, "no .github/workflows directory") {
		t.Fatalf("no workflows: %d %s", code, errs)
	}
}

func workflowCheckCommandIn(dir string) (int, string, string) {
	var out, errs bytes.Buffer
	code := workflowCheckCommand(nil, dir, &out, &errs, func(string) string { return "" })
	return code, out.String(), errs.String()
}
