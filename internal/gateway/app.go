// 本文件负责进程生命周期：解析参数、加载配置、启动 HTTP 服务、
// 启动健康检查，并在收到退出信号时优雅关闭。
//
// 为了保持日志最小化，这里不再输出配置成功、后端列表、关闭过程等日志；
// 只有致命错误会通过 Run 的返回值交给 main 打印。
package gateway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Run 加载配置并运行网关，直到收到退出信号。
func Run() error {
	// 关闭标准 log 的时间前缀：访问日志是纯 JSON，需要整行可解析。
	log.SetFlags(0)

	path := flag.String("config", "config.json", "configuration file path")
	flag.Parse()

	config, err := loadConfig(*path)
	if err != nil {
		return fmt.Errorf("无法加载配置 %q: %w", *path, err)
	}

	g := newGateway(config)
	server := &http.Server{
		Addr:              net.JoinHostPort(config.ListenIP, strconv.Itoa(config.ListenPort)),
		Handler:           g.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 收到 Ctrl+C 或 SIGTERM 时取消 ctx。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 后台健康检查：启动时先检查一遍，之后按配置间隔重复。
	go func() {
		g.health.checkAll(ctx)
		if config.HealthCheckIntervalSec > 0 {
			g.health.run(ctx, time.Duration(config.HealthCheckIntervalSec)*time.Second)
		}
	}()

	// 收到退出信号后，最多等 10 秒让正在处理的请求完成。
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}()

	// ListenAndServe 阻塞运行，直到服务器被关闭或监听失败。
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("无法监听 %s: %w（端口被占用时，检查旧的 Bun 服务或另一个 gopostor 是否仍在运行）", server.Addr, err)
	}

	<-done
	return nil
}
