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
		stats := translateStatsToChinese(group.Stats(), (*expiration).String())
		safeAddr := strings.ReplaceAll(*addr, ":", "-")
		payload := map[string]interface{}{
			"时间":   time.Now().Format(time.RFC3339),
			"地址":   *addr,
			"服务名": *svc,
			"组名":   *groupName,
			"统计信息": stats,
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

func translateStatsToChinese(stats map[string]interface{}, expiration string) map[string]interface{} {
	cn := make(map[string]interface{}, len(stats)+1)
	for k, v := range stats {
		switch {
		case k == "name":
			cn["名称"] = v
		case k == "closed":
			cn["是否关闭"] = v
		case k == "expiration":
			cn["过期时间"] = v
		case k == "loads":
			cn["加载次数"] = v
		case k == "local_hits":
			cn["本地命中"] = v
		case k == "local_misses":
			cn["本地未命中"] = v
		case k == "peer_hits":
			cn["远端命中"] = v
		case k == "peer_misses":
			cn["远端未命中"] = v
		case k == "loader_hits":
			cn["回源命中"] = v
		case k == "loader_errors":
			cn["回源错误"] = v
		case k == "hit_rate":
			cn["命中率"] = v
		case k == "avg_load_time_ms":
			cn["平均加载耗时毫秒"] = v
		case strings.HasPrefix(k, "cache_"):
			cn[translateCacheStatKey(k)] = v
		default:
			cn[k] = v
		}
	}
	cn["过期时间"] = expiration
	return cn
}

func translateCacheStatKey(key string) string {
	switch strings.TrimPrefix(key, "cache_") {
	case "len":
		return "缓存长度"
	case "capacity":
		return "缓存容量"
	case "hits":
		return "缓存命中"
	case "misses":
		return "缓存未命中"
	case "evictions":
		return "缓存淘汰"
	case "expired":
		return "缓存过期"
	default:
		return "缓存_" + strings.TrimPrefix(key, "cache_")
	}
}
