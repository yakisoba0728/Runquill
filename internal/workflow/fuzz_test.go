package workflow

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

func FuzzCheck(f *testing.F) {
	for _, seed := range []string{
		"",
		"jobs: \xff",
		"jobs:\n\tbuild:\n",
		"on: push\njobs:\n---\njobs: {}\n",
		"\xEF\xBB\xBFon: [push, pull_request]\r\njobs:\r\n  build:\r\n    runs-on: " + routeBuild,
		"on: {push: {}, pull_request_target: {}}\njobs:\n  build:\n    strategy: {matrix: {go: [1, 2]}}\n    runs-on: " +
			routeFor("pool__gpu__os__ubuntu2204__build", true) + "\n    steps:\n      - uses: example/action@v1\n      - run: docker build .\n",
		"jobs:\n  first:\n    runs-on: " + routeBuild + "\n  second:\n    runs-on: " + routeBuild + "\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		const path = ".github/workflows/ci.yml"
		original := bytes.Clone(src)
		options := CheckOptions{
			Pools:      map[string]bool{"default": true, "gpu": false},
			CheckImage: func(_, os string) (bool, bool) { return os == "ubuntu2204", false },
		}
		findings := Check(path, src, options)
		if !bytes.Equal(src, original) {
			t.Fatal("Check modified its input")
		}
		lastLine, maxLine := 1, bytes.Count(src, []byte("\n"))+1
		for _, finding := range findings {
			if finding.File != path || finding.Line < lastLine || finding.Line > maxLine ||
				finding.Code == "" || finding.Message == "" {
				t.Fatalf("invalid finding: %+v", finding)
			}
			lastLine = finding.Line
		}
		if again := Check(path, src, options); !slices.Equal(findings, again) {
			t.Fatalf("findings changed between calls: %v, then %v", findings, again)
		}
	})
}

func FuzzValidateMappings(f *testing.F) {
	for _, seed := range []string{
		`null`,
		`[{"from":[]}]`,
		`[{"from":["ubuntu-latest"]}]`,
		`[{"from":["Ubuntu-22.04","LARGE","large"],"pool":"gpu"},{"from":["windows-latest"]}]`,
		`[{"from":["a","b"]},{"from":["B","a"]}]`,
		`[{"from":["self-hosted"]}]`,
		`[{"from":["runquill-1-1-build"]}]`,
		`[{"from":["a\u0000b"]}]`,
		`[{"from":["a"],"pool":"GPU"}]`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		var mappings []Mapping
		if err := json.Unmarshal([]byte(input), &mappings); err != nil {
			return
		}
		pools, err := ValidateMappings(mappings)
		if err != nil {
			if pools != nil {
				t.Fatal("invalid mappings produced a partial result")
			}
			return
		}
		if len(pools) == 0 || len(pools) != len(mappings) {
			t.Fatalf("got %d pools for %d mappings", len(pools), len(mappings))
		}
		normalized := make([]Mapping, len(mappings))
		for i, mapping := range mappings {
			labels := slices.Clone(mapping.From)
			for j, label := range labels {
				labels[j] = strings.ToLower(label)
			}
			slices.Sort(labels)
			labels = slices.Compact(labels)
			if pool, ok := pools[strings.Join(labels, "\x00")]; !ok || pool != mapping.Pool {
				t.Fatalf("wrong pool for %q: %q", labels, pool)
			}
			slices.Reverse(labels)
			normalized[i] = Mapping{From: labels, Pool: mapping.Pool}
		}
		if got, err := ValidateMappings(normalized); err != nil || !maps.Equal(pools, got) {
			t.Fatalf("normalization changed the result: %v, %v", got, err)
		}
		duplicate := append(normalized, mappings[0])
		if got, err := ValidateMappings(duplicate); err == nil || got != nil {
			t.Fatalf("duplicate label set was accepted: %v, %v", got, err)
		}
	})
}
