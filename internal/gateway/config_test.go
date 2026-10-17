package gateway

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const exampleConfigPath = "../../config.example.json"

func TestReleaseRanges(t *testing.T) {
	c, err := loadConfig(exampleConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Backends) != 2 {
		t.Fatal("expected two backends")
	}
	for i, release := range []string{"1.9.3", "1.10.9"} {
		if c.Backends[i].Name != release {
			t.Fatalf("expected backend %s, got %s", release, c.Backends[i].Name)
		}
		for _, bound := range []string{c.Backends[i].MinVersion, c.Backends[i].MaxVersion} {
			n, err := encodeVersion(bound)
			if err != nil {
				t.Fatal(err)
			}
			for _, flag := range []int64{0, 25} {
				b := c.findMatchingBackend(n + flag)
				if b == nil || b.Name != release {
					t.Fatalf("wrong route for %s: %v", release, bound)
				}
			}
		}
	}
	for _, s := range []string{"2023.10.2", "2024.2.2", "2025.1.1"} {
		v, _ := encodeVersion(s)
		if c.findMatchingBackend(v) == nil {
			t.Fatal("range gap rejected")
		}
	}
	for _, s := range []string{"2023.9.30", "2024.2.4", "2026.7.22"} {
		v, _ := encodeVersion(s)
		if c.findMatchingBackend(v) != nil {
			t.Fatal("outside range accepted")
		}
	}
}

// TestReleaseCompatibility optionally checks the example ranges against external Impostor sources.
func TestReleaseCompatibility(t *testing.T) {
	sourceRoot := os.Getenv("IMPOSTOR_SOURCE_ROOT")
	if sourceRoot == "" {
		t.Skip("set IMPOSTOR_SOURCE_ROOT to the directory containing Impostor-1.9.3 and Impostor-1.10.9")
	}
	c, err := loadConfig(exampleConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for i, release := range []string{"1.9.3", "1.10.9"} {
		source, err := os.ReadFile(filepath.Join(sourceRoot, "Impostor-"+release, "src", "Impostor.Server", "Net", "Manager", "CompatibilityManager.cs"))
		if err != nil {
			t.Fatal(err)
		}
		block := strings.Split(string(source), "private readonly")[0]
		versions := regexp.MustCompile(`new GameVersion\((\d+),\s*(\d+),\s*(\d+)\)`).FindAllStringSubmatch(block, -1)
		expected := 4
		if i == 1 {
			expected = 22
		}
		if len(versions) != expected {
			t.Fatal(len(versions))
		}
		for _, v := range versions {
			n, err := encodeVersion(v[1] + "." + v[2] + "." + v[3])
			if err != nil {
				t.Fatal(err)
			}
			for _, flag := range []int64{0, 25} {
				b := c.findMatchingBackend(n + flag)
				if b == nil || b.Name != release {
					t.Fatalf("wrong route: %v", v)
				}
			}
		}
	}
}

func TestOpenRangesAndFallback(t *testing.T) {
	c, err := parseConfig([]byte(`{"backends":[
		{"name":"a","minVersion":"50593050","maxVersion":"2024.2.3","publicIp":"203.0.113.1","publicPort":1,"httpEndpoint":"http://127.0.0.1:1"},
		{"name":"b","minVersion":"2024.6.1","maxVersion":"","publicIp":"203.0.113.2","publicPort":2,"httpEndpoint":"http://127.0.0.1:2"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backends[0].min != 50593050 || c.Backends[1].max != maxVersion || c.Backends[1].rangeText() != "2024.6.1..any" {
		t.Fatal(c.Backends)
	}
	for _, row := range []struct{ version, backend, kind string }{
		{"2021.6.30", "a", routeBelow}, {"2023.10.1", "a", routeExact}, {"2024.4.1", "a", routeBetween},
		{"2024.6.1", "b", routeExact}, {"2099.1.1", "b", routeExact},
	} {
		v, _ := encodeVersion(row.version)
		r := c.route(v + 25)
		if r.backend == nil || r.backend.Name != row.backend || r.kind != row.kind || r.normalized != v {
			t.Fatalf("%s: %+v", row.version, r)
		}
	}
	if r := c.route(0); r.backend != nil || r.kind != routeInvalid {
		t.Fatal(r)
	}
	warnings, notes := c.review()
	if len(warnings) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "2024.2.3") {
		t.Fatal(warnings, notes)
	}
	loaded, err := loadConfig(exampleConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if warnings, _ := loaded.review(); len(warnings) != 2 {
		t.Fatal("loopback publicIp should warn", warnings)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, v := range []string{"2026", "-1", "99999999999"} {
		if _, err := parseVersionBound(v, 0); err == nil {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"19.0", "2026.13.1", "2026.7.20.25", "99999999999999999999.1.1"} {
		if _, err := encodeVersion(v); err == nil {
			t.Fatal(v)
		}
	}
	c := testConfig(t, "http://localhost:23001")
	if c.Backends[1].encodedIP != 16777343 {
		t.Fatal("incorrect IPv4 byte order")
	}
	data, err := os.ReadFile(exampleConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{`"2024.3.1"`, `"2024.2.3"`}, {`22031`, `0`}, {`"127.0.0.1"`, `"::1"`}, {`"http://127.0.0.1:23001"`, `"http://localhost/path"`}, {`"1.10.9"`, `"1.9.3"`}} {
		modified := strings.ReplaceAll(string(data), pair[0], pair[1])
		if modified == string(data) {
			t.Fatalf("test replacement did not match: %v", pair)
		}
		if _, err := parseConfig([]byte(modified)); err == nil {
			t.Fatalf("accepted invalid config: %v", pair)
		}
	}
}
