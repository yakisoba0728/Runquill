package route

import (
	"errors"
	"strings"
	"testing"
)

func TestSplitSuffix(t *testing.T) {
	pool32, pool33 := "p"+strings.Repeat("a", 31), "p"+strings.Repeat("a", 32)
	os40, os41 := "o"+strings.Repeat("b", 39), "o"+strings.Repeat("b", 40)
	for _, test := range []struct {
		suffix, pool, osName, rest string
		wantErr, wantOSErr         bool
	}{
		// Plain job suffixes are returned unchanged.
		{suffix: "build", rest: "build"},
		{suffix: "", rest: ""},
		{suffix: "build__extra", rest: "build__extra"},
		{suffix: "Pool__gpu__build", rest: "Pool__gpu__build"},
		{suffix: "OS__ubuntu__build", rest: "OS__ubuntu__build"},
		{suffix: "pool_gpu__build", rest: "pool_gpu__build"},

		// Pool selector.
		{suffix: "pool__gpu__build", pool: "gpu", rest: "build"},
		{suffix: "pool__default__build", pool: "default", rest: "build"},
		{suffix: "pool__0-gpu-1__build", pool: "0-gpu-1", rest: "build"},
		{suffix: "pool__gpu__build__extra", pool: "gpu", rest: "build__extra"},
		{suffix: "pool__gpu____build", pool: "gpu", rest: "__build"},
		{suffix: "pool__" + pool32 + "__build", pool: pool32, rest: "build"},
		{suffix: "pool__" + pool33 + "__build", wantErr: true},
		{suffix: "pool__GPU__build", wantErr: true},
		{suffix: "pool__-gpu__build", wantErr: true},
		{suffix: "pool__gpu_1__build", wantErr: true},
		{suffix: "pool____build", wantErr: true},
		{suffix: "pool__gpu", wantErr: true},
		{suffix: "pool__gpu__", wantErr: true},
		{suffix: "pool__", wantErr: true},

		// OS selector.
		{suffix: "os__ubuntu-24-04__build", osName: "ubuntu-24-04", rest: "build"},
		{suffix: "os__" + os40 + "__build", osName: os40, rest: "build"},
		{suffix: "os__" + os41 + "__build", wantErr: true, wantOSErr: true},
		{suffix: "os__Ubuntu__build", wantErr: true, wantOSErr: true},
		{suffix: "os__bad_name__build", wantErr: true, wantOSErr: true},
		{suffix: "os__-ubuntu__build", wantErr: true, wantOSErr: true},
		{suffix: "os____build", wantErr: true, wantOSErr: true},
		{suffix: "os__ubuntu", wantErr: true, wantOSErr: true},
		{suffix: "os__ubuntu__", wantErr: true, wantOSErr: true},

		// Both selectors: the pool comes first, and each is read once.
		{suffix: "pool__gpu__os__debian__build", pool: "gpu", osName: "debian", rest: "build"},
		{suffix: "os__debian__pool__gpu__build", osName: "debian", rest: "pool__gpu__build"},
		{suffix: "pool__gpu__pool__cpu__build", pool: "gpu", rest: "pool__cpu__build"},
		{suffix: "os__debian__os__alpine__build", osName: "debian", rest: "os__alpine__build"},
		{suffix: "pool__gpu__os__Debian__build", wantErr: true, wantOSErr: true},
		{suffix: "pool__gpu__os__debian", wantErr: true, wantOSErr: true},
		{suffix: "pool__GPU__os__debian__build", wantErr: true},
	} {
		t.Run(test.suffix, func(t *testing.T) {
			pool, osName, rest, err := SplitSuffix(test.suffix)
			if test.wantErr {
				if err == nil {
					t.Fatalf("SplitSuffix(%q) = %q, %q, %q, want an error", test.suffix, pool, osName, rest)
				}
				if errors.Is(err, ErrInvalidOS) != test.wantOSErr {
					t.Errorf("SplitSuffix(%q) error = %v, ErrInvalidOS match = %v", test.suffix, err, test.wantOSErr)
				}
				if pool != "" || osName != "" || rest != "" {
					t.Errorf("SplitSuffix(%q) returned %q, %q, %q with an error", test.suffix, pool, osName, rest)
				}
				return
			}
			if err != nil {
				t.Fatalf("SplitSuffix(%q): %v", test.suffix, err)
			}
			if pool != test.pool || osName != test.osName || rest != test.rest {
				t.Errorf("SplitSuffix(%q) = %q, %q, %q, want %q, %q, %q",
					test.suffix, pool, osName, rest, test.pool, test.osName, test.rest)
			}
		})
	}
}

func TestValidPoolName(t *testing.T) {
	for name, want := range map[string]bool{
		DefaultPool:                   true,
		"gpu":                         true,
		"0":                           true,
		"a-b-":                        true,
		"p" + strings.Repeat("a", 31): true,
		"p" + strings.Repeat("a", 32): false,
		"":                            false,
		"-gpu":                        false,
		"GPU":                         false,
		"gpu_1":                       false,
		"gpu.1":                       false,
		"gpu ":                        false,
		"gpu\n":                       false,
		"gpu__os":                     false,
	} {
		if got := ValidPoolName(name); got != want {
			t.Errorf("ValidPoolName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMaxSuffixFitsLabel(t *testing.T) {
	// runquill-RUN-ATTEMPT-SUFFIX with a 19-digit run ID, a 3-digit attempt and
	// room for a "-NNN" matrix index must stay within GitHub's 100 characters.
	label := "runquill-" + strings.Repeat("9", 19) + "-999-" + strings.Repeat("s", MaxSuffix) + "-999"
	if len(label) != 100 {
		t.Fatalf("longest label has %d characters, want 100", len(label))
	}
}
