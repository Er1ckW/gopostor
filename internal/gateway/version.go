package gateway

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const maxVersion = 2147483647

// minRawVersion rejects raw integers that cannot be a real client version (2000.1.1).
const minRawVersion = 2000*25000 + 1800 + 50

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
	n := v[0]*25000 + v[1]*1800 + v[2]*50 + v[3]
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
	year, rest := n/25000, n%25000
	month, rest := rest/1800, rest%1800
	day, revision := rest/50, rest%50
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
	if version%50 >= 25 {
		return version - 25
	}
	return version
}
