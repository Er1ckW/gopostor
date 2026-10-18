// Package main 是 gopostor 可执行程序的入口。
//
// 对 Go 初学者需要先建立一个概念：Go 用"包（package）"组织代码，
// 同一个目录下的所有 .go 文件必须声明同一个包名。名为 main 的包比较特殊，
// 它必须包含一个 main 函数，go build 才会把它编译成可直接运行的可执行文件。
// 这个目录（cmd/gopostor）就是整个程序唯一的入口。
package main

import (
	// log 是标准库的日志包。默认输出到标准错误（stderr），
	// 正好符合 README 中"所有日志写入 stderr，由 systemd 收集"的部署方式。
	"log"
	// os 提供与操作系统交互的函数；这里只用到 os.Exit 来设置进程退出码。
	"os"

	// 导入本项目的内部实现包。模块名在 go.mod 里声明为 gopostor，
	// 因此这个包的完整导入路径是 gopostor/internal/gateway。
	//
	// internal 是 Go 的特殊目录约定：只有 gopostor 模块内部的代码才能导入它，
	// 外部模块无法依赖，适合放"不打算公开给别人用"的实现。
	"gopostor/internal/gateway"
)

// main 是程序执行的起点，签名为固定的 func main()，没有参数也没有返回值。
//
// 它刻意保持得很薄：只负责调用 gateway.Run()，并根据返回的 error 决定进程退出码。
// 配置加载、HTTP 服务、后台任务、优雅退出等真正的逻辑都在 gateway 包里，
// 这样 gateway 包可以被测试直接调用，而入口文件不需要测试。
func main() {
	// if err := ...; err != nil 是 Go 常见的"声明 + 判断"写法：
	// 短变量声明 err 只在 if 语句内部可见，右侧的 gateway.Run() 先执行。
	// Run() 返回 error 接口；返回 nil 表示正常结束，非 nil 表示启动或运行失败。
	if err := gateway.Run(); err != nil {
		// %v 按默认格式打印错误内容。Run 已经关闭了 log 的时间前缀，
		// 并会在错误里带上中文说明，所以这里只补一个 [ERROR] 标记。
		log.Printf("[ERROR] %v", err)
		// 用非 0 退出码告诉 systemd / 容器 / shell：进程是异常结束的。
		// os.Exit 会立即终止进程，不会执行任何 defer，也不会再运行其他 goroutine，
		// 因此它只放在最后一行。
		os.Exit(1)
	}
}
