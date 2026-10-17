package gateway

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// Protocol versions are encoded as year*25000 + month*1800 + day*50 + revision.
	yearFactor  = 25000
	monthFactor = 1800
	dayFactor   = 50
	// The client may set +25 to disable server authority; routing ignores that flag.
	authorityFlag = 25

	maxVersion = 2147483647

	// minRawVersion rejects raw integers that cannot be a real client version (2000.1.1).
	minRawVersion = 2000*yearFactor + monthFactor + dayFactor
)

var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:\.(\d+))?$`)

func encodeVersion(s string) (int64, error) {
	parts := versionPattern.FindStringSubmatch(s)
	if parts == nil {
		return 0, fmt.Errorf("invalid protocol version: %q", s)
	}
	var v [4]int64
	for i := range v {
		if parts[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return 0, err
		}
		v[i] = n
	}
	n := v[0]*yearFactor + v[1]*monthFactor + v[2]*dayFactor + v[3]
	if v[0] < 1 || v[0] > 85899 || v[1] < 1 || v[1] > 12 || v[2] < 1 || v[2] > 31 || v[3] > 24 || n > maxVersion {
		return 0, fmt.Errorf("invalid protocol version: %q", s)
	}
	return n, nil
}

// parseVersionBound accepts a readable version, the raw integer a client sends, or an empty value.
func parseVersionBound(s string, empty int64) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return empty, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < minRawVersion || n > maxVersion {
			return 0, fmt.Errorf("invalid protocol version: %q", s)
		}
		return n, nil
	}
	return encodeVersion(s)
}

func formatVersion(n int64) string {
	year, rest := n/yearFactor, n%yearFactor
	month, rest := rest/monthFactor, rest%monthFactor
	day, revision := rest/dayFactor, rest%dayFactor
	if revision == 0 {
		return fmt.Sprintf("%d.%d.%d", year, month, day)
	}
	return fmt.Sprintf("%d.%d.%d.%d", year, month, day, revision)
}

// versionText formats a version for messages, keeping values that cannot be a real version as raw numbers.
func versionText(n int64) string {
	if n < minRawVersion {
		return fmt.Sprint(n)
	}
	return formatVersion(n)
}

func normalizeVersion(version int64) int64 {
	if version%dayFactor >= authorityFlag {
		return version - authorityFlag
	}
	return version
}
