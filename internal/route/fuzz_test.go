package route

import (
	"strings"
	"testing"
)

func FuzzSplitSuffix(f *testing.F) {
	for _, seed := range []string{
		"", "build", "build__extra", "pool__gpu__build", "os__ubuntu-24-04__build",
		"pool__gpu__os__debian__build", "os__debian__pool__gpu__build", "pool__gpu__pool__cpu__build",
		"pool__GPU__build", "pool__gpu__", "os____build", "pool__gpu__os__Debian__build",
		"os__" + strings.Repeat("a", 40) + "__build", "os__" + strings.Repeat("a", 41) + "__build",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, suffix string) {
		pool, osName, job, err := SplitSuffix(suffix)
		hasPool := strings.HasPrefix(suffix, "pool__")
		if err != nil {
			if pool != "" || osName != "" || job != "" || !hasPool && !strings.HasPrefix(suffix, "os__") {
				t.Fatalf("unexpected split error for %q: %q, %q, %q, %v", suffix, pool, osName, job, err)
			}
			return
		}
		if hasPool != (pool != "") || pool != "" && !ValidPoolName(pool) {
			t.Fatalf("invalid pool %q for %q", pool, suffix)
		}
		prefix := ""
		if pool != "" {
			prefix = "pool__" + pool + "__"
		}
		hasOS := strings.HasPrefix(suffix, prefix+"os__")
		if hasOS != (osName != "") || (hasPool || hasOS) && job == "" {
			t.Fatalf("invalid OS or job for %q: %q, %q", suffix, osName, job)
		}
		if osName != "" {
			if !validSelector(osName, 40) {
				t.Fatalf("invalid OS %q for %q", osName, suffix)
			}
			prefix += "os__" + osName + "__"
		}
		if prefix+job != suffix {
			t.Fatalf("split did not round trip: %q became %q", suffix, prefix+job)
		}
	})
}

func FuzzValidPoolName(f *testing.F) {
	for _, seed := range []string{
		"", DefaultPool, "0", "cpu-2-", "-gpu", "GPU", "gpu_2", "gpu\n", "한글", "\xff",
		strings.Repeat("a", 32), strings.Repeat("a", 33),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		want := validSelector(name, 32)
		if got := ValidPoolName(name); got != want {
			t.Fatalf("ValidPoolName(%q) = %t, want %t", name, got, want)
		}
	})
}

func validSelector(name string, maxLen int) bool {
	const lettersAndDigits = "abcdefghijklmnopqrstuvwxyz0123456789"
	return len(name) >= 1 && len(name) <= maxLen && strings.IndexByte(lettersAndDigits, name[0]) >= 0 &&
		strings.Trim(name, lettersAndDigits+"-") == ""
}
