// 本文件测试进程生命周期：Run 能按配置启动，并在 ctx 取消后正常返回。
package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunServesUntilCanceled 启动真实的 HTTP 服务，确认能访问，再取消 ctx 并等待 Run 返回 nil。
func TestRunServesUntilCanceled(t *testing.T) {
	// 先让系统分配一个空闲端口，再关闭它交给 Run 使用。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	config := fmt.Sprintf(`{"listenPort":%d,"healthCheckIntervalSec":0,"backends":[{"name":"only","publicIp":"127.0.0.1","publicPort":22032,"httpEndpoint":"http://127.0.0.1:1"}]}`, port)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	captureLog(t)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, Options{ConfigPath: path}) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(url)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal(response.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start:", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRunRejectsMissingConfig 验证配置文件不存在时 Run 立即返回错误。
func TestRunRejectsMissingConfig(t *testing.T) {
	err := Run(context.Background(), Options{ConfigPath: filepath.Join(t.TempDir(), "missing.json")})
	if err == nil {
		t.Fatal("expected error")
	}
}
