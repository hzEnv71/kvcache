package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	kvcache "KVCache"
	"KVCache/etcd"
)

func main() {
	//解析命令行参数
	addr := flag.String("addr", "127.0.0.1:8001", "server listen address")
	svc := flag.String("svc", "kv-cache", "service name in etcd")
	groupName := flag.String("group", "test", "cache group name")
	cacheBytes := flag.Int64("cache-bytes", 2<<20, "cache size in bytes")
	expiration := flag.Duration("expiration", 0, "cache expiration, e.g. 30s, 1m")
	etcd := flag.String("etcd", "127.0.0.1:2379", "comma-separated etcd endpoints")
	flag.Parse()
	//解析etcd端点
	etcdEndpoints := splitAndTrim(*etcd)
	if len(etcdEndpoints) == 0 {
		fmt.Fprintln(os.Stderr, "invalid --etcd endpoints")
		os.Exit(1)
	}
	//设置etcd端点
	registry.DefaultConfig.Endpoints = etcdEndpoints
	//创建服务器
	srv, err := kvcache.NewServer(
		*addr,
		*svc,
		kvcache.WithEtcdEndpoints(etcdEndpoints),
		kvcache.WithDialTimeout(5*time.Second),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create server failed: %v\n", err)
		os.Exit(1)
	}

	options := []kvcache.GroupOption{}
	if *expiration > 0 {
		options = append(options, kvcache.WithExpiration(*expiration))
	}
	//创建缓存组 设置缓存过期时间
	group := kvcache.NewGroup(*groupName, *cacheBytes, kvcache.GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		return nil, fmt.Errorf("key %q not found in source", key)
	}), options...)
	//创建节点选择器
	picker, err := kvcache.NewClientPicker(*addr, kvcache.WithServiceName(*svc))
	if err != nil {
		fmt.Fprintf(os.Stderr, "create client picker failed: %v\n", err)
		os.Exit(1)
	}
	group.RegisterPeers(picker) //注册节点选择器到缓存组

	fmt.Printf("server starting addr=%s svc=%s group=%s etcd=%v\n", *addr, *svc, *groupName, etcdEndpoints)
	// 退出前把 group stats 写入按节点区分的日志文件
	writeStatusLog := func() {
		if err := os.MkdirAll("log", 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "create log dir failed: %v\n", err)
			return
		}
		stats := group.Stats()
		stats["expiration"] = (*expiration).String()
		safeAddr := strings.ReplaceAll(*addr, ":", "-")
		payload := map[string]interface{}{
			"time":  time.Now().Format(time.RFC3339),
			"addr":  *addr,
			"svc":   *svc,
			"group": *groupName,
			"stats": stats,
		}
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal status log failed: %v\n", err)
			return
		}
		path := fmt.Sprintf("log/status-%s.log", safeAddr)
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write status log failed: %v\n", err)
		}
	}
	//异步启动服务器
	go func() {
		if err := srv.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "server stopped with error: %v\n", err)
			os.Exit(1)
		}
	}()
	//监听退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	//关闭节点选择器和服务器
	fmt.Println("shutting down server...")
	writeStatusLog()
	_ = picker.Close()
	srv.Stop()
}

func splitAndTrim(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
