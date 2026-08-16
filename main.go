// Package main 是负载均衡调度器的入口。
// 路由：--smoke-test 执行自检；否则以 server 子命令启动 HTTP 服务。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"task028-loadbalancer/internal/httpapi"
	"task028-loadbalancer/internal/selfcheck"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--smoke-test" {
		os.Exit(selfcheck.Run())
	}

	sub := "server"
	if len(args) > 0 {
		sub = args[0]
	}
	if sub != "server" {
		fmt.Fprintf(os.Stderr, "未知命令 %q\n用法: task028-loadbalancer [server|--smoke-test]\n", sub)
		os.Exit(2)
	}

	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	addr := ":8080"
	fs.StringVar(&addr, "addr", ":8080", "HTTP 监听地址")
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(2)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New().Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	log.Printf("负载均衡调度器服务已启动，监听 %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "服务启动失败:", err)
		os.Exit(1)
	}
}
