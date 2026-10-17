package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// 测试单机真实 Server；不是测试替身，也不代表生产集群部署。
// 逐案例新数据库，明确提高历史事件上限并统一 FULL 同步；这些不是默认配置成绩。
func startServer(ctx context.Context, c *Config, binary string) (func(), error) {
	if c.Address != "" {
		return func() {}, nil
	}
	if binary == "" || c.StorePath == "" {
		return nil, fmt.Errorf("temporal needs -address or both -temporal-bin and -store")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	c.Address = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if err = os.MkdirAll(filepath.Dir(c.StorePath), 0755); err != nil {
		return nil, err
	}
	log, err := os.Create(c.StorePath + ".server.log")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, binary, "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", strconv.Itoa(port), "--db-filename", c.StorePath, "--sqlite-pragma", "journal_mode=WAL", "--sqlite-pragma", "synchronous=FULL", "--dynamic-config-value", "limit.historyCount.error=200000", "--dynamic-config-value", "limit.historyCount.warn=100000")
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stop := func() { _ = cmd.Process.Kill(); <-done; _ = log.Close() }
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, e := net.DialTimeout("tcp", c.Address, 50*time.Millisecond)
		if e == nil {
			conn.Close()
			return stop, nil
		}
		select {
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		case e := <-done:
			log.Close()
			return nil, fmt.Errorf("temporal server exited before ready: %v (see %s.server.log)", e, c.StorePath)
		case <-ticker.C:
		}
	}
}
