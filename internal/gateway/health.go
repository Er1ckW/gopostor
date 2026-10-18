// 本文件跟踪每个后端是否可达，并把这些状态提供给 /healthz。
//
// 健康状态有两个来源：
//   - 后台定时检查：定期对后端 GET /；
//   - 真实请求：每次转发请求的结果也会更新状态。
//
// 健康检查不再输出日志；/healthz 仍然返回这些状态。
package gateway

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	stateUnknown = "unknown"
	stateUp      = "up"
	stateDown    = "down"
)

// backendState 是单个后端的健康快照，也是 /healthz 返回数组里的元素。
type backendState struct {
	Name      string    `json:"name"`
	Endpoint  string    `json:"endpoint"`
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
	LastCheck time.Time `json:"last_check"`
	LastError string    `json:"last_error,omitempty"`
	LatencyMS int64     `json:"latency_ms"`
	Failures  int       `json:"consecutive_failures"`
}

// healthMonitor 保存所有后端的状态，由 mutex 保护。
type healthMonitor struct {
	mu       sync.Mutex
	states   map[string]*backendState
	backends []*Backend
	client   *http.Client
}

// newHealthMonitor 为每个后端建立初始状态（unknown）。
func newHealthMonitor(c *Config, client *http.Client) *healthMonitor {
	m := &healthMonitor{states: map[string]*backendState{}, client: client}
	for i := range c.Backends {
		b := &c.Backends[i]
		m.backends = append(m.backends, b)
		m.states[b.Name] = &backendState{Name: b.Name, Endpoint: b.HTTPEndpoint, State: stateUnknown, Since: time.Now()}
	}
	return m
}

// record 用一次检查/请求的结果更新后端状态。
func (m *healthMonitor) record(b *Backend, err error, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.states[b.Name]
	if s == nil {
		return
	}
	now := time.Now()
	s.LastCheck, s.LatencyMS = now, latency.Milliseconds()
	if err != nil {
		s.Failures++
		s.LastError = string(classifyError(err))
		if s.State != stateDown {
			s.State, s.Since = stateDown, now
		}
		return
	}
	s.Failures, s.LastError = 0, ""
	if s.State != stateUp {
		s.State, s.Since = stateUp, now
	}
}

// check 对一个后端执行 GET /。
// 只要返回了任何 HTTP 响应（包括 404）都算可达。
func (m *healthMonitor) check(ctx context.Context, b *Backend) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.HTTPEndpoint+"/", nil)
	if err == nil {
		req.Header.Set("User-Agent", "gopostor-health")
		var response *http.Response
		if response, err = m.client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 65536))
			response.Body.Close()
		}
	}
	if ctx.Err() != nil {
		return
	}
	m.record(b, err, time.Since(start))
}

// checkAll 并发检查所有后端，并等待全部完成。
func (m *healthMonitor) checkAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range m.backends {
		wg.Add(1)
		go func(b *Backend) {
			defer wg.Done()
			m.check(ctx, b)
		}(b)
	}
	wg.Wait()
}

// run 按固定间隔重复检查，直到 context 被取消。
func (m *healthMonitor) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

// snapshot 返回所有后端状态的副本，供 /healthz 使用。
func (m *healthMonitor) snapshot() (states []backendState, healthy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	healthy = true
	for _, b := range m.backends {
		s := *m.states[b.Name]
		states = append(states, s)
		if s.State == stateDown {
			healthy = false
		}
	}
	return states, healthy
}
