package workflow

import (
	"slices"
	"strings"
	"testing"
)

func routeFor(suffix string, indexed bool) string {
	if indexed {
		return "${{ format('runquill-{0}-{1}-" + suffix + "-{2}', github.run_id, github.run_attempt, strategy.job-index) }}"
	}
	return "${{ format('runquill-{0}-{1}-" + suffix + "', github.run_id, github.run_attempt) }}"
}

func checkCodes(t *testing.T, src string) []string {
	t.Helper()
	var codes []string
	for _, f := range Check("ci.yml", []byte(src), CheckOptions{}) {
		codes = append(codes, f.Job+":"+f.Code)
	}
	slices.Sort(codes)
	return codes
}

func TestCheckMatrixDetection(t *testing.T) {
	for _, test := range []struct {
		name, strategy string
		matrix         bool
	}{
		{"block matrix", "    strategy:\n      fail-fast: false\n      matrix:\n        go: [1, 2]\n", true},
		{"flow matrix", "    strategy: {fail-fast: false, matrix: {go: [1, 2]}}\n", true},
		{"quoted flow matrix", "    strategy: {\"matrix\": {go: [1]}}\n", true},
		{"block without matrix", "    strategy:\n      fail-fast: false\n      max-parallel: 2\n", false},
		{"flow without matrix", "    strategy: {fail-fast: false}\n", false},
		{"nested matrix key", "    strategy: {fail-fast: false, x: {matrix: 1}}\n", false},
		{"no strategy", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			src := "permissions: {}\njobs:\n  a:\n" + test.strategy + "    runs-on: " + routeBuild + "\n    timeout-minutes: 1\n"
			got := checkCodes(t, src)
			if want := slices.Contains(got, "a:runs-on"); want != test.matrix {
				t.Fatalf("matrix %v, findings %v", test.matrix, got)
			}
		})
	}
}

func TestCheckDuplicateSuffix(t *testing.T) {
	job := func(id, runsOn, extra string) string {
		return "  " + id + ":\n" + extra + "    runs-on: " + runsOn + "\n    timeout-minutes: 1\n"
	}
	matrix := "    strategy:\n      matrix: {x: [1, 2]}\n"
	for _, test := range []struct {
		name string
		jobs string
		want []string
	}{
		{"same suffix", job("a", routeBuild, "") + job("b", routeBuild, ""), []string{"b:duplicate-suffix"}},
		{"case differs", job("a", routeBuild, "") + job("b", routeFor("BUILD", false), ""), []string{"b:duplicate-suffix"}},
		{"three jobs", job("a", routeBuild, "") + job("b", routeBuild, "") + job("c", routeBuild, ""),
			[]string{"b:duplicate-suffix", "c:duplicate-suffix"}},
		{"unique suffixes", job("a", routeFor("a", false), "") + job("b", routeFor("b", false), ""), nil},
		{"different pool", job("a", routeBuild, "") + job("b", routeFor("pool__big__build", false), ""), nil},
		{"different os", job("a", routeFor("os__ubuntu2204__build", false), "") + job("b", routeFor("os__debian12__build", false), ""), nil},
		{"same selectors", job("a", routeFor("pool__big__build", false), "") + job("b", routeFor("pool__big__build", false), ""),
			[]string{"b:duplicate-suffix"}},
		{"one matrix job", job("test", routeFor("test", true), matrix), nil},
		{"matrix jobs with own suffixes", job("a", routeFor("a", true), matrix) + job("b", routeFor("b", true), matrix), nil},
		{"matrix jobs share a suffix", job("a", routeFor("test", true), matrix) + job("b", routeFor("test", true), matrix),
			[]string{"b:duplicate-suffix"}},
		{"matrix index meets a literal suffix", job("a", routeFor("test", true), matrix) + job("b", routeFor("test-1", false), ""),
			[]string{"b:duplicate-suffix"}},
		{"matrix index and a different literal", job("a", routeFor("test", true), matrix) + job("b", routeFor("test-x", false), "") +
			job("c", routeFor("test-01", false), "") + job("d", routeFor("test-1-2", false), ""), nil},
		{"matrix and plain job with the base suffix", job("a", routeFor("test", true), matrix) + job("b", routeFor("test", false), ""), nil},
		{"hosted jobs", job("a", "ubuntu-latest", "") + job("b", "ubuntu-latest", ""), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := checkCodes(t, "permissions: {}\njobs:\n"+test.jobs)
			if !slices.Equal(got, test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
	src := "permissions: {}\njobs:\n  a:\n    timeout-minutes: 1\n    runs-on: " + routeBuild + "\n  b:\n    timeout-minutes: 1\n    runs-on: " + routeBuild + "\n"
	findings := Check("ci.yml", []byte(src), CheckOptions{})
	if len(findings) != 1 || findings[0].Line != 8 || findings[0].Severity != severityWarning ||
		!strings.HasPrefix(findings[0].Message, `Suffix "build" is also used by job a (line 5)`) {
		t.Fatalf("finding: %+v", findings)
	}
}
