package workflow

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const routeBuild = "${{ format('runquill-{0}-{1}-build', github.run_id, github.run_attempt) }}"

var testMapping = []Mapping{{From: []string{"ubuntu-latest"}}, {From: []string{"ubuntu-22.04", "large"}, Pool: "big"}}

func fuzzRunnerOS(labels []string) string {
	for _, label := range labels {
		if strings.EqualFold(label, "ubuntu-22.04") {
			return "ubuntu2204"
		}
	}
	return ""
}

func rewriteWorkflow(src []byte, mapping []Mapping) (RewriteResult, error) {
	return Rewrite(src, mapping, nil)
}

func rewriteForTest(t *testing.T, src string) RewriteResult {
	t.Helper()
	res, err := rewriteWorkflow([]byte(src), testMapping)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRewriteForms(t *testing.T) {
	for _, test := range []struct{ name, in, out string }{
		{"plain", "on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps: []\n",
			"on: push\njobs:\n  build:\n    runs-on: " + routeBuild + "\n    steps: []\n"},
		{"comment and trailing space", "jobs:\n  build:\n    runs-on: ubuntu-latest   # hosted  \n",
			"jobs:\n  build:\n    runs-on: " + routeBuild + "   # hosted  \n"},
		{"double quoted", "jobs:\n  build:\n    runs-on: \"ubuntu-latest\"\n",
			"jobs:\n  build:\n    runs-on: \"" + routeBuild + "\"\n"},
		{"single quoted", "jobs:\n  build:\n    runs-on: 'ubuntu-latest' # c\n",
			"jobs:\n  build:\n    runs-on: '" + strings.ReplaceAll(routeBuild, "'", "''") + "' # c\n"},
		{"flow sequence", "jobs:\n  build:\n    runs-on: [ubuntu-latest]\n",
			"jobs:\n  build:\n    runs-on: " + routeBuild + "\n"},
		{"flow sequence set and pool", "jobs:\n  Build:\n    runs-on: [ large , \"ubuntu-22.04\" ] # x\n",
			"jobs:\n  Build:\n    runs-on: ${{ format('runquill-{0}-{1}-pool__big__build', github.run_id, github.run_attempt) }} # x\n"},
		{"block sequence", "jobs:\n  build:\n    runs-on:\n      - ubuntu-22.04\n      - large\n    steps: []\n",
			"jobs:\n  build:\n    runs-on: ${{ format('runquill-{0}-{1}-pool__big__build', github.run_id, github.run_attempt) }}\n    steps: []\n"},
		{"compact block sequence with comments", "jobs:\n  build:\n    runs-on: # pick\n    - ubuntu-22.04\n    # keep me\n    - 'large'\n",
			"jobs:\n  build:\n    runs-on: ${{ format('runquill-{0}-{1}-pool__big__build', github.run_id, github.run_attempt) }} # pick\n    # keep me\n"},
		{"block sequence at end without newline", "jobs:\n  build:\n    runs-on:\n      - ubuntu-22.04\n      - large",
			"jobs:\n  build:\n    runs-on: ${{ format('runquill-{0}-{1}-pool__big__build', github.run_id, github.run_attempt) }}"},
		{"matrix", "jobs:\n  test:\n    strategy:\n      matrix:\n        go: [1, 2]\n    runs-on: ubuntu-latest\n",
			"jobs:\n  test:\n    strategy:\n      matrix:\n        go: [1, 2]\n    runs-on: ${{ format('runquill-{0}-{1}-test-{2}', github.run_id, github.run_attempt, strategy.job-index) }}\n"},
		{"crlf bom and no final newline", "\xEF\xBB\xBFjobs:\r\n  build:\r\n    runs-on: ubuntu-latest\r\n    steps: []",
			"\xEF\xBB\xBFjobs:\r\n  build:\r\n    runs-on: " + routeBuild + "\r\n    steps: []"},
		{"crlf block sequence", "jobs:\r\n  build:\r\n    runs-on:\r\n      - ubuntu-latest\r\n",
			"jobs:\r\n  build:\r\n    runs-on: " + routeBuild + "\r\n"},
		{"quoted keys and document start", "---\n\"on\": push\n'jobs':\n  \"build\":\n    'runs-on': ubuntu-latest\n",
			"---\n\"on\": push\n'jobs':\n  \"build\":\n    'runs-on': " + routeBuild + "\n"},
		{"block scalars elsewhere", "jobs:\n  build:\n    if: |\n      runs-on: ubuntu-latest\n    runs-on: ubuntu-latest\n    steps:\n    - run: |\n        jobs:\n          x:\n          \truns-on: ubuntu-latest\n",
			"jobs:\n  build:\n    if: |\n      runs-on: ubuntu-latest\n    runs-on: " + routeBuild + "\n    steps:\n    - run: |\n        jobs:\n          x:\n          \truns-on: ubuntu-latest\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			res := rewriteForTest(t, test.in)
			if res.Refused != "" || string(res.Output) != test.out {
				t.Fatalf("refused %q, got\n%q\nwant\n%q", res.Refused, res.Output, test.out)
			}
			if len(res.Jobs) != 1 || !res.Jobs[0].Changed || res.Jobs[0].Code != outcomeChanged {
				t.Fatalf("outcomes: %+v", res.Jobs)
			}
			again := rewriteForTest(t, string(res.Output))
			if !bytes.Equal(again.Output, res.Output) || again.Jobs[0].Code != outcomeRunquill {
				t.Fatalf("second pass changed the file: %+v", again.Jobs)
			}
		})
	}
}

func TestRewriteRefusals(t *testing.T) {
	for _, test := range []struct{ name, in, code string }{
		{"expression", "jobs:\n  build:\n    runs-on: ${{ matrix.os }}\n", outcomeExpression},
		{"expression in list", "jobs:\n  build:\n    runs-on: [ubuntu-latest, '${{ inputs.x }}']\n", outcomeExpression},
		{"group mapping", "jobs:\n  build:\n    runs-on:\n      group: big\n      labels: [ubuntu-latest]\n", outcomeMapping},
		{"flow mapping", "jobs:\n  build:\n    runs-on: {group: big}\n", outcomeMapping},
		{"alias", "jobs:\n  build:\n    runs-on: *hosted\n", outcomeAnchor},
		{"anchor", "jobs:\n  build:\n    runs-on: &hosted ubuntu-latest\n", outcomeAnchor},
		{"alias in list", "jobs:\n  build:\n    runs-on: [*hosted]\n", outcomeAnchor},
		{"alias in block list", "jobs:\n  build:\n    runs-on:\n      - *hosted\n", outcomeAnchor},
		{"job anchor", "jobs:\n  build: &base\n    runs-on: ubuntu-latest\n", outcomeAnchor},
		{"merge key", "jobs:\n  build:\n    <<: *base\n    runs-on: ubuntu-latest\n", outcomeAnchor},
		{"self-hosted", "jobs:\n  build:\n    runs-on: [self-hosted, linux]\n", outcomeSelfHosted},
		{"runquill static", "jobs:\n  build:\n    runs-on: runquill-1-1-build\n", outcomeRunquill},
		{"runquill expression", "jobs:\n  build:\n    runs-on: " + routeBuild + "\n", outcomeRunquill},
		{"reusable call", "jobs:\n  build:\n    uses: org/repo/.github/workflows/x.yml@main\n", outcomeReusable},
		{"no runs-on", "jobs:\n  build:\n    steps: []\n", outcomeNoRunsOn},
		{"unmapped", "jobs:\n  build:\n    runs-on: windows-latest\n", outcomeUnmapped},
		{"partial set", "jobs:\n  build:\n    runs-on: [ubuntu-22.04]\n", outcomeUnmapped},
		{"workflow_call", "on:\n  workflow_call:\njobs:\n  build:\n    runs-on: ubuntu-latest\n", outcomeWorkflowCall},
		{"workflow_call list", "on: [push, workflow_call]\njobs:\n  build:\n    runs-on: ubuntu-latest\n", outcomeWorkflowCall},
		{"underscore job", "jobs:\n  _build:\n    runs-on: ubuntu-latest\n", outcomeInvalidJobID},
		{"pool-like job", "jobs:\n  pool__gpu__x:\n    runs-on: ubuntu-latest\n", outcomeInvalidJobID},
		{"long job", "jobs:\n  " + strings.Repeat("b", 70) + ":\n    runs-on: ubuntu-latest\n", outcomeInvalidJobID},
		{"block scalar", "jobs:\n  build:\n    runs-on: >\n      ubuntu-latest\n", outcomeUnparseable},
		{"multi-line plain", "jobs:\n  build:\n    runs-on: ubuntu-latest\n      continued\n", outcomeUnparseable},
		{"multi-line quoted", "jobs:\n  build:\n    runs-on: \"ubuntu-latest\n      \"\n", outcomeUnparseable},
		{"multi-line flow", "jobs:\n  build:\n    runs-on: [ubuntu-latest,\n      large]\n", outcomeUnparseable},
		{"escaped quote", "jobs:\n  build:\n    runs-on: \"ubuntu\\x2dlatest\"\n", outcomeUnparseable},
		{"tag", "jobs:\n  build:\n    runs-on: !!str ubuntu-latest\n", outcomeUnparseable},
		{"item comment", "jobs:\n  build:\n    runs-on:\n      - ubuntu-latest # why\n", outcomeUnparseable},
		{"nested list", "jobs:\n  build:\n    runs-on:\n      - - ubuntu-latest\n", outcomeUnparseable},
		{"duplicate runs-on", "jobs:\n  build:\n    runs-on: ubuntu-latest\n    runs-on: ubuntu-latest\n", outcomeUnparseable},
		{"empty runs-on", "jobs:\n  build:\n    runs-on:\n    steps: []\n", outcomeUnparseable},
		{"flow job", "jobs:\n  build: {runs-on: ubuntu-latest}\n", outcomeUnparseable},
		{"bad indentation", "jobs:\n  build:\n      timeout-minutes: 5\n    runs-on: ubuntu-latest\n", outcomeUnparseable},
	} {
		t.Run(test.name, func(t *testing.T) {
			res := rewriteForTest(t, test.in)
			if res.Refused != "" || string(res.Output) != test.in || len(res.Jobs) != 1 ||
				res.Jobs[0].Changed || res.Jobs[0].Code != test.code || res.Jobs[0].Reason == "" {
				t.Fatalf("refused %q, output changed %t, outcomes %+v", res.Refused, string(res.Output) != test.in, res.Jobs)
			}
		})
	}
}

func TestRewriteCaseCollision(t *testing.T) {
	src := "jobs:\n  Build:\n    runs-on: ubuntu-latest\n  build:\n    runs-on: ubuntu-latest\n  lint:\n    runs-on: ubuntu-latest\n"
	res := rewriteForTest(t, src)
	if len(res.Jobs) != 3 || res.Jobs[0].Code != outcomeInvalidJobID || res.Jobs[1].Code != outcomeInvalidJobID || !res.Jobs[2].Changed {
		t.Fatalf("outcomes: %+v", res.Jobs)
	}
	if res.Jobs[2].Line != 7 || res.Jobs[0].Line != 3 {
		t.Fatalf("line numbers: %+v", res.Jobs)
	}
}

func TestRewriteFileRefusals(t *testing.T) {
	for _, test := range []struct{ name, in string }{
		{"tab indentation", "jobs:\n\tbuild:\n\t\truns-on: ubuntu-latest\n"},
		{"tab job body", "jobs:\n  build:\n\truns-on: ubuntu-latest\n"},
		{"multi-document", "jobs:\n  build:\n    runs-on: ubuntu-latest\n---\njobs: {}\n"},
		{"document end", "jobs:\n  build:\n    runs-on: ubuntu-latest\n...\n"},
		{"directive", "%YAML 1.2\n---\njobs:\n  build:\n    runs-on: ubuntu-latest\n"},
		{"flow jobs", "jobs: {build: {runs-on: ubuntu-latest}}\n"},
		{"jobs alias", "jobs: *all\n"},
		{"jobs list", "jobs:\n  - build\n"},
		{"duplicate jobs", "jobs:\n  a:\n    runs-on: x\njobs:\n  b:\n    runs-on: x\n"},
		{"duplicate job", "jobs:\n  a:\n    runs-on: ubuntu-latest\n  a:\n    runs-on: ubuntu-latest\n"},
		{"lone carriage return", "jobs:\r  build:\n    runs-on: ubuntu-latest\n"},
		{"invalid utf-8", "jobs:\n  build:\n    runs-on: ubuntu-latest\xff\n"},
		{"multi-line top value", "on: [push,\npull_request]\njobs:\n  build:\n    runs-on: ubuntu-latest\n"},
		{"complex key", "? jobs\n: x\n"},
		{"indented first line", "  jobs:\n    build:\n      runs-on: ubuntu-latest\n"},
		{"dedented job", "jobs:\n    a:\n      runs-on: ubuntu-latest\n  b:\n    runs-on: ubuntu-latest\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			res := rewriteForTest(t, test.in)
			if res.Refused == "" || string(res.Output) != test.in || res.Changed() {
				t.Fatalf("expected a file refusal, got %+v", res)
			}
		})
	}
}

func TestRewriteKeepsUnrelatedJobsAndBytes(t *testing.T) {
	src := "# top comment\nname: CI\non:\n  push:\n    branches: [main]\npermissions: {}\njobs:\n\n  lint:   # lint job\n    runs-on: windows-latest\n    steps:\n      - uses: actions/checkout@v4\n\n  build:\n    name: \"Build: all\"\n    runs-on: ubuntu-latest\n    env:\n      A: 'x # y'\n    steps:\n      - run: echo \"runs-on: ubuntu-latest\"   \n  call:\n    uses: ./.github/workflows/x.yml\n# trailing comment\n"
	res := rewriteForTest(t, src)
	want := strings.Replace(src, "    runs-on: ubuntu-latest\n", "    runs-on: "+routeBuild+"\n", 1)
	if string(res.Output) != want {
		t.Fatalf("got\n%s", res.Output)
	}
	codes := []string{}
	for _, job := range res.Jobs {
		codes = append(codes, job.Job+"="+job.Code)
	}
	if strings.Join(codes, ",") != "lint=unmapped,build=changed,call=reusable-call" {
		t.Fatalf("outcomes: %v", codes)
	}
}

func TestRewriteNoJobs(t *testing.T) {
	for _, src := range []string{"", "\n", "# only a comment\n", "name: x\non: push\n"} {
		res := rewriteForTest(t, src)
		if res.Refused != "" || len(res.Jobs) != 0 || string(res.Output) != src {
			t.Fatalf("%q: %+v", src, res)
		}
	}
}

func TestRewriteExamplesAreAlreadyRunquill(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		res := rewriteForTest(t, string(src))
		if res.Refused != "" || !bytes.Equal(res.Output, src) {
			t.Fatalf("%s: %+v", path, res.Refused)
		}
		for _, job := range res.Jobs {
			if job.Changed || job.Code == outcomeUnparseable {
				t.Fatalf("%s: %+v", path, job)
			}
		}
	}
}

func TestValidateRunsOnMapping(t *testing.T) {
	for _, bad := range [][]Mapping{
		nil,
		{{From: nil}},
		{{From: []string{""}}},
		{{From: []string{"${{ x }}"}}},
		{{From: []string{"self-hosted"}}},
		{{From: []string{"runquill-1"}}},
		{{From: []string{" ubuntu"}}},
		{{From: []string{"ubuntu-latest"}, Pool: "Bad Pool"}},
		{{From: []string{"a", "b"}}, {From: []string{"B", "a"}}},
	} {
		if _, err := ValidateMappings(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := rewriteWorkflow([]byte("jobs: {}\n"), nil); err == nil {
		t.Fatal("rewrite accepted an empty mapping")
	}
}

func TestUnifiedDiff(t *testing.T) {
	before := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\n"
	after := "a\nB\nc\nd\ne\nf\ng\nh\ni\nj\nk\nL\nm"
	got := UnifiedDiff(".github/workflows/ci.yml", []byte(before), []byte(after))
	want := "--- a/.github/workflows/ci.yml\n+++ b/.github/workflows/ci.yml\n" +
		"@@ -1,5 +1,5 @@\n a\n-b\n+B\n c\n d\n e\n" +
		"@@ -9,5 +9,5 @@\n i\n j\n k\n-l\n-m\n+L\n+m\n\\ No newline at end of file\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if UnifiedDiff("x", []byte("same"), []byte("same")) != "" {
		t.Fatal("identical files produced a diff")
	}
	if got := UnifiedDiff("x", nil, []byte("new\n")); got != "--- a/x\n+++ b/x\n@@ -0,0 +1 @@\n+new\n" {
		t.Fatalf("insertion diff: %q", got)
	}
	src := "jobs:\r\n  build:\r\n    runs-on: ubuntu-latest\r\n"
	res := rewriteForTest(t, src)
	if got := UnifiedDiff("ci.yml", []byte(src), res.Output); !strings.Contains(got, "-    runs-on: ubuntu-latest\n+    runs-on: "+routeBuild+"\n") {
		t.Fatalf("rewrite diff: %q", got)
	}
}

// The fuzz target checks the properties the panel relies on: no panic, nothing
// changes without a match, rewriting is idempotent, and only reported spans move.
func FuzzRewriteWorkflow(f *testing.F) {
	for _, seed := range []string{
		"jobs:\n  build:\n    runs-on: ubuntu-latest\n",
		"jobs:\r\n  build:\r\n    runs-on: [ubuntu-latest] # c\r\n",
		"\xEF\xBB\xBFjobs:\n  a:\n    runs-on:\n    - ubuntu-22.04\n    - large",
		"on: push\njobs:\n  test:\n    strategy: {matrix: {a: [1]}}\n    runs-on: 'ubuntu-latest'\n  b:\n    runs-on: \"ubuntu-latest\"\n",
		"jobs:\n  build:\n    runs-on:\n      group: x\n",
		"jobs:\n  build:\n    uses: a/b/.github/workflows/c.yml@v1\n",
		"jobs:\n\tbuild:\n",
	} {
		f.Add([]byte(seed))
	}
	files, _ := filepath.Glob("../../examples/*.yml")
	for _, path := range files {
		if src, err := os.ReadFile(path); err == nil {
			f.Add(src)
		}
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 64<<10 {
			return
		}
		res, err := rewriteWorkflow(src, testMapping)
		if err != nil {
			t.Fatal(err)
		}
		if res.Refused != "" && !bytes.Equal(res.Output, src) {
			t.Fatal("a refused file changed")
		}
		unmapped, err := rewriteWorkflow(src, []Mapping{{From: []string{"never-used-label"}}})
		if err != nil || !bytes.Equal(unmapped.Output, src) {
			t.Fatal("a mapping that matches nothing changed the file")
		}
		again, err := rewriteWorkflow(res.Output, testMapping)
		if err != nil || !bytes.Equal(again.Output, res.Output) {
			t.Fatalf("rewrite is not idempotent:\n%q\n%q", res.Output, again.Output)
		}
		// Keeping hosted Ubuntu versions as os__ selectors keeps the same guarantees.
		images, err := Rewrite(src, testMapping, fuzzRunnerOS)
		if err != nil || images.Refused != "" && !bytes.Equal(images.Output, src) {
			t.Fatalf("rewrite with a catalogue: %v", err)
		}
		if again, err := Rewrite(images.Output, testMapping, fuzzRunnerOS); err != nil || !bytes.Equal(again.Output, images.Output) {
			t.Fatalf("rewrite with a catalogue is not idempotent:\n%q\n%q", images.Output, again.Output)
		}
		// Bytes outside the edited spans keep their order and value.
		at, out := 0, res.Output
		for _, e := range res.edits {
			n := e.start - at
			if !bytes.Equal(out[:n], src[at:e.start]) || !strings.HasPrefix(string(out[n:]), e.text) {
				t.Fatalf("bytes outside an edit changed")
			}
			out, at = out[n+len(e.text):], e.end
		}
		if !bytes.Equal(out, src[at:]) {
			t.Fatal("bytes after the last edit changed")
		}
		_ = UnifiedDiff("f.yml", src, res.Output)
	})
}
