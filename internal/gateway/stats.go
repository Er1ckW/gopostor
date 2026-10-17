package gateway

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// statsCollector aggregates requests between two summary lines.
type statsCollector struct {
	mu       sync.Mutex
	total    int
	status   map[string]int
	backends map[string]int
	issues   map[string]int
	versions map[string]int
}

func newStats() *statsCollector {
	return &statsCollector{status: map[string]int{}, backends: map[string]int{}, issues: map[string]int{}, versions: map[string]int{}}
}

func (s *statsCollector) record(rw *requestLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	s.status[fmt.Sprintf("%dxx", rw.status/100)]++
	if b, ok := rw.fields["backend"].(string); ok {
		s.backends[b]++
	}
	for _, code := range rw.issues {
		s.issues[code]++
	}
	if v, ok := rw.fields["client_version_text"].(string); ok && rw.fields["path"] == "/api/user" {
		s.versions[v]++
	}
}

func topCounts(m map[string]int, n int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || (m[keys[i]] == m[keys[j]] && keys[i] < keys[j]) })
	if len(keys) > n {
		keys = keys[:n]
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s×%d", k, m[k])
	}
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, " ")
}

// flush prints one summary of the past window and starts a new one. Nothing is printed without traffic.
func (s *statsCollector) flush(window time.Duration) {
	s.mu.Lock()
	total, status, backends, issues, versions := s.total, s.status, s.backends, s.issues, s.versions
	s.total, s.status, s.backends, s.issues, s.versions = 0, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	s.mu.Unlock()
	if total == 0 {
		return
	}
	logEvent(map[string]any{
		"event": "stats", "window_sec": int(window.Seconds()), "requests": total,
		"status": status, "backends": backends, "issues": issues, "token_versions": versions,
	})
	log.Printf("[STATS] 最近 %s：请求 %d（%s）；后端 %s；登录版本 %s；问题 %s", window, total,
		topCounts(status, 5), topCounts(backends, 5), topCounts(versions, 8), topCounts(issues, 8))
}
