package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yakisoba0728/Runquill/internal/route"

	"github.com/yakisoba0728/Runquill/internal/workflow"
)

func TestMigrationKeepsHostedUbuntuVersions(t *testing.T) {
	release := testReleaseImages(t, "")
	mapping := []workflow.Mapping{{From: []string{"ubuntu-latest"}}, {From: []string{"ubuntu-22.04"}}, {From: []string{"ubuntu-24.04"}},
		{From: []string{"ubuntu-22.04", "large"}, Pool: "big"}, {From: []string{"ubuntu-20.04"}}, {From: []string{"ubuntu-22.04-arm"}}}
	src := "on: push\njobs:\n  latest:\n    runs-on: ubuntu-latest\n  jammy:\n    runs-on: ubuntu-22.04 # keep\n  noble:\n    runs-on: [ubuntu-24.04]\n" +
		"  big:\n    runs-on: [ubuntu-22.04, large]\n  focal:\n    runs-on: ubuntu-20.04\n  arm:\n    runs-on: ubuntu-22.04-arm\n" +
		"  m:\n    strategy: {matrix: {a: [1]}}\n    runs-on: ubuntu-22.04\n"
	label := func(suffix string) string {
		return "${{ format('runquill-{0}-{1}-" + suffix + "', github.run_id, github.run_attempt) }}"
	}
	want := map[string]string{
		"latest": label("latest"), "jammy": label("os__ubuntu2204__jammy"), "noble": label("os__ubuntu2404__noble"),
		"big": label("pool__big__os__ubuntu2204__big"), "focal": label("focal"), "arm": label("arm"),
		"m": "${{ format('runquill-{0}-{1}-os__ubuntu2204__m-{2}', github.run_id, github.run_attempt, strategy.job-index) }}",
	}
	res, err := workflow.Rewrite([]byte(src), mapping, release.hostedRunnerOS)
	if err != nil || res.Refused != "" {
		t.Fatal(err, res.Refused)
	}
	for _, job := range res.Jobs {
		if !job.Changed || job.RunsOn != want[job.Job] {
			t.Errorf("%s: %+v, want %s", job.Job, job, want[job.Job])
		}
	}
	if !strings.Contains(string(res.Output), "runs-on: "+label("os__ubuntu2204__jammy")+" # keep\n") {
		t.Fatalf("comment not kept:\n%s", res.Output)
	}
	again, err := workflow.Rewrite(res.Output, mapping, release.hostedRunnerOS)
	if err != nil || again.Changed() {
		t.Fatal("rewrite with a catalogue is not idempotent")
	}
	// Without the entry (a development build), the job is only mapped to its pool, as before.
	dev, _ := buildRunnerImages("", "", "dev", "runner:1")
	res, _ = workflow.Rewrite([]byte(src), mapping, dev.hostedRunnerOS)
	for _, job := range res.Jobs {
		if job.Job == "jammy" && job.RunsOn != label("jammy") || job.Job == "noble" && job.RunsOn != label("os__ubuntu2404__noble") {
			t.Errorf("dev catalogue %s: %s", job.Job, job.RunsOn)
		}
	}
	// A configured ubuntu-latest image is kept explicitly.
	res, _ = workflow.Rewrite([]byte(src), mapping, testReleaseImages(t, `{"ubuntu-latest":"r/latest:1"}`).hostedRunnerOS)
	if res.Jobs[0].RunsOn != label("os__ubuntu-latest__latest") {
		t.Errorf("configured ubuntu-latest: %s", res.Jobs[0].RunsOn)
	}
	// A job ID that would read as a selector is refused, and the selector counts against the label limit.
	long := strings.Repeat("a", route.MaxSuffix-10)
	refused, _ := workflow.Rewrite([]byte("jobs:\n  os__x:\n    runs-on: ubuntu-latest\n  "+long+":\n    runs-on: ubuntu-22.04\n"), mapping, release.hostedRunnerOS)
	if refused.Jobs[0].Code != "job-id" || refused.Jobs[1].Code != "job-id" {
		t.Fatalf("refused %+v", refused.Jobs)
	}
	// The manager's catalogue drives panel previews; ubuntu-latest stays a plain route.
	t.Setenv(runnerImagesEnv, `{"ubuntu22.04":"r/jammy:1"}`)
	f, api := newFakeMigrationGitHub(t)
	f.files["jammy.yml"] = "jobs:\n  test:\n    runs-on: ubuntu-22.04\n"
	preview, err := previewMigration(t.Context(), api, "owner/repo", []workflow.Mapping{{From: []string{"ubuntu-latest"}}, {From: []string{"ubuntu-22.04"}}})
	if err != nil {
		t.Fatal(err)
	}
	var diffs string
	for _, file := range preview.Files {
		diffs += file.Diff
	}
	if !strings.Contains(diffs, "+    runs-on: "+routeBuild) || !strings.Contains(diffs, "+    runs-on: "+label("os__ubuntu2204__test")) {
		t.Fatalf("preview diffs:\n%s", diffs)
	}
}

func TestWorkflowCheckRunnerImages(t *testing.T) {
	images := testReleaseImages(t, "")
	pools := workerPools{defaultWorkerPool: {}, "big": {Images: []string{"ubuntu22.04"}}}
	job := func(suffix string) string {
		return "permissions: {}\njobs:\n  a:\n    runs-on: ${{ format('runquill-{0}-{1}-" + suffix + "', github.run_id, github.run_attempt) }}\n    timeout-minutes: 1\n"
	}
	for _, test := range []struct {
		suffix string
		images *runnerImages
		want   []string
	}{
		{"os__ubuntu2204__a", images, nil},
		{"os__ubuntu-22-04__a", images, nil},
		{"pool__big__os__ubuntu2204__a", images, nil},
		{"pool__big__os__ubuntu-latest__a", images, []string{"a:runner-image:warning"}},
		{"os__ubuntu-latest__a", images, nil},
		{"os__plan9__a", images, []string{"a:runner-image:warning"}},
		{"pool__big__os__debian12__a", images, []string{"a:runner-image:warning"}},
		{"os__plan9__a", nil, nil},
		{"os__bad_name__a", images, []string{"a:runs-on:warning"}},
		{"os__debian12__", images, []string{"a:runs-on:warning"}},
	} {
		got := codesOf(workflow.Check("ci.yml", []byte(job(test.suffix)), workflowCheckOptions(pools, test.images)))
		if !slices.Equal(got, test.want) {
			t.Errorf("%s: got %v, want %v", test.suffix, got, test.want)
		}
	}
	// The command reads the catalogue from the environment and reports an invalid one.
	dir := t.TempDir()
	path := filepath.Join(dir, "ci.yml")
	if err := os.WriteFile(path, []byte(job("os__plan9__a")), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs strings.Builder
	workflowCheckCommand([]string{path}, dir, &out, &errs, func(string) string { return "" })
	if !strings.Contains(out.String(), "[runner-image]") {
		t.Fatalf("output %s", out.String())
	}
	out.Reset()
	workflowCheckCommand([]string{path}, dir, &out, &errs, func(name string) string {
		if name == runnerImagesEnv {
			return "{"
		}
		return ""
	})
	if strings.Contains(out.String(), "[runner-image]") || !strings.Contains(errs.String(), "runner image checks are skipped") {
		t.Fatalf("invalid catalogue: %s %s", out.String(), errs.String())
	}
}
