// Command devredis runs an in-process Redis for local development.
//
// It exists so the Redis-backed path can be exercised on a machine with no
// Docker and no Redis installed: the same server the test suite uses
// (miniredis) is exposed on a TCP port, speaking the real Redis protocol.
//
//	go run ./cmd/devredis                 listen on 127.0.0.1:6379
//	go run ./cmd/devredis 127.0.0.1:6399  listen somewhere else
//
// It is a development aid, not a Redis replacement: state lives in memory,
// nothing is persisted, and its Lua interpreter is far slower than the real
// thing. Use a real Redis for anything you intend to measure or ship.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alicebob/miniredis/v2"
)

func main() {
	addr := "127.0.0.1:6379"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	server := miniredis.NewMiniRedis()
	if err := server.StartAddr(addr); err != nil {
		fmt.Fprintf(os.Stderr, "devredis: could not listen on %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer server.Close()

	fmt.Printf("devredis listening on %s\n", server.Addr())
	fmt.Printf("start the proxy with:  REDIS_ADDR=%s go run ./cmd/proxy\n", server.Addr())
	fmt.Println("press Ctrl+C to stop")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("devredis stopped")
}
