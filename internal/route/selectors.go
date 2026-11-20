// Package route defines the pool and OS selectors in a Runquill job label.
package route

import (
	"errors"
	"regexp"
	"strings"
)

const (
	DefaultPool = "default"
	// MaxSuffix reserves space within the 100 character label for a 19-digit
	// run ID, a 3-digit attempt and a 3-digit matrix index.
	MaxSuffix = 100 - len("runquill-") - 19 - 1 - 3 - 1 - 4
)

var (
	poolNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	osNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	ErrInvalidOS    = errors.New("invalid runner OS label suffix (use os__NAME__JOB or pool__POOL__os__NAME__JOB)")
)

func ValidPoolName(name string) bool {
	return poolNamePattern.MatchString(name)
}

// SplitSuffix reads the selectors at the start of a route suffix (the part after runquill-RUN-ATTEMPT-):
// JOB, pool__POOL__JOB, os__NAME__JOB or pool__POOL__os__NAME__JOB.
func SplitSuffix(suffix string) (pool, osName, rest string, err error) {
	rest = suffix
	if requested, ok := strings.CutPrefix(rest, "pool__"); ok {
		selected, after, found := strings.Cut(requested, "__")
		if !found || after == "" || !ValidPoolName(selected) {
			return "", "", "", errors.New("invalid worker pool label suffix (use pool__POOL__JOB)")
		}
		pool, rest = selected, after
	}
	if requested, ok := strings.CutPrefix(rest, "os__"); ok {
		selected, after, found := strings.Cut(requested, "__")
		if !found || after == "" || !osNamePattern.MatchString(selected) {
			return "", "", "", ErrInvalidOS
		}
		osName, rest = selected, after
	}
	return pool, osName, rest, nil
}
