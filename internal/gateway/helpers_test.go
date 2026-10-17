package gateway

import (
	"net/http/httptest"
	"testing"
)

func TestForwardedClient(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		realIP     string
		forwarded  string
		want       string
	}{
		{"loopback real ip", "127.0.0.1:5000", "203.0.113.9", "", "203.0.113.9"},
		{"loopback forwarded for", "127.0.0.1:5000", "", "203.0.113.9, 10.0.0.1", "203.0.113.9"},
		{"direct client cannot spoof", "198.51.100.7:5000", "203.0.113.9", "", ""},
		{"loopback without headers", "127.0.0.1:5000", "", "", ""},
		{"invalid remote address", "not-an-address", "203.0.113.9", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.realIP != "" {
				r.Header.Set("X-Real-IP", tc.realIP)
			}
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if got := forwardedClient(r); got != tc.want {
				t.Fatalf("forwardedClient() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"a\nb", 5, "a"},
		{"a\r\nb", 5, "a"},
		{"你好世界", 2, "你好…"},
		{"", 0, ""},
	}
	for _, tc := range cases {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Fatalf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestTopCounts(t *testing.T) {
	cases := []struct {
		in   map[string]int
		n    int
		want string
	}{
		{map[string]int{"200": 3, "404": 1}, 5, "200×3 404×1"},
		{map[string]int{"a": 1, "b": 1}, 5, "a×1 b×1"},
		{map[string]int{"a": 1, "b": 2, "c": 3}, 2, "c×3 b×2"},
		{map[string]int{}, 5, "无"},
	}
	for _, tc := range cases {
		if got := topCounts(tc.in, tc.n); got != tc.want {
			t.Fatalf("topCounts(%v, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
