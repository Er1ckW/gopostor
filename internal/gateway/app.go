// 本文件负责进程生命周期：加载配置、启动 HTTP 服务、
// 启动健康检查，并在 ctx 被取消（通常是收到退出信号）时优雅关闭。
// 命令行参数和信号由 cmd/gopostor/main.go 处理后通过 Options 和 ctx 传进来。
//
// 为了保持日志最小化，这里不再输出配置成功、后端列表、关闭过程等日志；
// 只有致命错误会通过 Run 的返回值交给 main 打印。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Options 是启动网关所需的命令行选项。
type Options struct {
	// ConfigPath 是 JSON 配置文件路径（-config）。
	ConfigPath string
	// Debug 为 true 时（-debug），访问日志记录完整的请求/响应 body 和解码后的 token；
	// 为 false 时只记录脱敏后的内容，见 redact.go。
	Debug bool
}

// Run 加载配置并运行网关，直到 ctx 被取消。
func Run(ctx context.Context, opts Options) error {
	// 关闭标准 log 的时间前缀：访问日志是纯 JSON，需要整行可解析。
	log.SetFlags(0)

	config, err := loadConfig(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("无法加载配置 %q: %w", opts.ConfigPath, err)
	}

	g := newGateway(config)
	g.debug = opts.Debug
	server := &http.Server{
		Addr:              net.JoinHostPort(config.ListenIP, strconv.Itoa(config.ListenPort)),
		Handler:           g.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 函数返回时（包括监听失败）也要停掉后台健康检查。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 后台健康检查：启动时先检查一遍，之后按配置间隔重复。
	go func() {
		g.health.checkAll(ctx)
		if config.HealthCheckIntervalSec > 0 {
			g.health.run(ctx, time.Duration(config.HealthCheckIntervalSec)*time.Second)
		}
	}()

	// ctx 取消后，最多等 10 秒让正在处理的请求完成。
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
