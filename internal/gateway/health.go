package gateway

import (
	"context"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	stateUnknown = "unknown"
	stateUp      = "up"
	stateDown    = "down"
)

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

// healthMonitor tracks whether each backend answers HTTP, from periodic checks and from live traffic,
// and logs every change of state once instead of once per failed request.
type healthMonitor struct {
	mu        sync.Mutex
	states    map[string]*backendState
	backends  []*Backend
	client    *http.Client
	timeoutMS int
}

func newHealthMonitor(c *Config, client *http.Client) *healthMonitor {
	m := &healthMonitor{states: map[string]*backendState{}, client: client, timeoutMS: c.BackendTimeoutMS}
	for i := range c.Backends {
		b := &c.Backends[i]
		m.backends = append(m.backends, b)
		m.states[b.Name] = &backendState{Name: b.Name, Endpoint: b.HTTPEndpoint, State: stateUnknown, Since: time.Now()}
	}
	return m
}

// record updates the state of a backend. source names what produced the result (startup, check, request).
func (m *healthMonitor) record(b *Backend, err error, latency time.Duration, source string) {
	m.mu.Lock()
	s := m.states[b.Name]
	if s == nil {
		m.mu.Unlock()
		return
	}
	now := time.Now()
	previous, downFor := s.State, now.Sub(s.Since).Round(time.Second)
	s.LastCheck, s.LatencyMS = now, latency.Milliseconds()
	var kind failureKind
	if err != nil {
		kind = classifyError(err)
		s.Failures++
		s.LastError = string(kind)
		if previous != stateDown {
			s.State, s.Since = stateDown, now
		}
	} else {
		s.Failures, s.LastError = 0, ""
		if previous != stateUp {
			s.State, s.Since = stateUp, now
		}
	}
	failures := s.Failures
	m.mu.Unlock()

	event := map[string]any{"event": "backend_health", "backend": b.Name, "backend_endpoint": b.HTTPEndpoint, "source": source, "latency_ms": latency.Milliseconds()}
	switch {
	case err != nil && previous != stateDown:
		event["level"], event["state"], event["error_kind"] = "error", stateDown, kind
		logEvent(event)
		log.Printf("[ERROR] 后端 %s 不可达（%s，来源 %s）：%s。在恢复前，分配到该后端的创建/加入房间都会失败", b.Name, kind, source, hintBackendUnavailable(b, err, m.timeoutMS))
	case err != nil && failures%20 == 0:
		event["level"], event["state"], event["error_kind"], event["consecutive_failures"] = "error", stateDown, kind, failures
		logEvent(event)
		log.Printf("[ERROR] 后端 %s 仍不可达，已持续 %s、连续失败 %d 次（%s）", b.Name, downFor, failures, kind)
	case err == nil && previous == stateDown:
		event["level"], event["state"], event["down_for_sec"] = "info", stateUp, int(downFor.Seconds())
		logEvent(event)
		log.Printf("[INFO] 后端 %s 已恢复，中断约 %s", b.Name, downFor)
	case err == nil && previous == stateUnknown:
		event["level"], event["state"] = "info", stateUp
		logEvent(event)
		log.Printf("[INFO] 后端 %s（%s）HTTP 可达，耗时 %dms", b.Name, b.HTTPEndpoint, latency.Milliseconds())
	}
}

func (m *healthMonitor) isDown(b *Backend) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[b.Name]
	return s != nil && s.State == stateDown
}

// check sends GET / to the backend. Any HTTP answer counts as reachable: Impostor 1.9.x has no handler there.
func (m *healthMonitor) check(ctx context.Context, b *Backend, source string) {
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
	m.record(b, err, time.Since(start), source)
}

func (m *healthMonitor) checkAll(ctx context.Context, source string) {
	var wg sync.WaitGroup
	for _, b := range m.backends {
		wg.Add(1)
		go func(b *Backend) {
			defer wg.Done()
			m.check(ctx, b, source)
		}(b)
	}
	wg.Wait()
}

func (m *healthMonitor) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx, "check")
		}
	}
}

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
