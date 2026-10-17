package gateway

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Run loads the configuration and runs the gateway until shutdown is requested.
func Run() {
	path := flag.String("config", "config.json", "configuration file path")
	flag.Parse()
	config, err := loadConfig(*path)
	if err != nil {
		log.Fatalf("[ERROR] 无法加载配置 %q: %v", *path, err)
	}
	log.Printf("Configuration loaded: path=%q listen=%s:%d backend_timeout_ms=%d reject_unknown_versions=%t probe_backend_on_create=%t health_check_interval_sec=%d stats_interval_sec=%d",
		*path, config.ListenIP, config.ListenPort, config.BackendTimeoutMS, config.RejectUnknownVersions, config.ProbeBackendOnCreate, config.HealthCheckIntervalSec, config.StatsIntervalSec)
	for _, b := range config.Backends {
		log.Printf("Backend configured: name=%q versions=%s encoded=%d..%d http=%q public=%s:%d", b.Name, b.rangeText(), b.min, b.max, b.HTTPEndpoint, b.PublicIP, b.PublicPort)
	}
	warnings, notes := config.review()
	for _, text := range warnings {
		log.Printf("[WARN] 配置检查：%s", text)
	}
	for _, text := range notes {
		log.Printf("[INFO] 配置说明：%s", text)
	}

	g := newGateway(config)
	server := &http.Server{Addr: net.JoinHostPort(config.ListenIP, strconv.Itoa(config.ListenPort)), Handler: g.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		g.health.checkAll(ctx, "startup")
		if config.HealthCheckIntervalSec > 0 {
			g.health.run(ctx, time.Duration(config.HealthCheckIntervalSec)*time.Second)
		}
	}()
	if config.StatsIntervalSec > 0 {
		go func() {
			window := time.Duration(config.StatsIntervalSec) * time.Second
			ticker := time.NewTicker(window)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					stats.flush(window)
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		log.Print("Shutdown requested; draining HTTP requests")
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("Shutdown: %v", err)
			_ = server.Close()
		}
	}()
	log.Printf("gopostor listening on %s; %d backends; config: %s", server.Addr, len(config.Backends), *path)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("[ERROR] 无法监听 %s: %v（端口被占用时，检查旧的 Bun 服务或另一个 gopostor 是否仍在运行）", server.Addr, err)
	}
	<-done
	log.Print("Shutdown complete")
}
