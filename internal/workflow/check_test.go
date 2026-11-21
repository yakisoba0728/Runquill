package workflow

import (
	"slices"
	"testing"
)

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
