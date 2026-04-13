package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	kvcache "github.com/youngyangyang04/KVCache-Go"
	"github.com/youngyangyang04/KVCache-Go/registry"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8001", "server listen address")
	svc := flag.String("svc", "kv-cache", "service name in etcd")
	groupName := flag.String("group", "test", "cache group name")
	cacheBytes := flag.Int64("cache-bytes", 2<<20, "cache size in bytes")
	expiration := flag.Duration("expiration", 0, "cache expiration, e.g. 30s, 1m")
	etcd := flag.String("etcd", "127.0.0.1:2379", "comma-separated etcd endpoints")
	flag.Parse()

	etcdEndpoints := splitAndTrim(*etcd)
	if len(etcdEndpoints) == 0 {
		fmt.Fprintln(os.Stderr, "invalid --etcd endpoints")
		os.Exit(1)
	}

	registry.DefaultConfig.Endpoints = etcdEndpoints

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

	group := kvcache.NewGroup(*groupName, *cacheBytes, kvcache.GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		return nil, fmt.Errorf("key %q not found in source", key)
	}), options...)

	picker, err := kvcache.NewClientPicker(*addr, kvcache.WithServiceName(*svc))
	if err != nil {
		fmt.Fprintf(os.Stderr, "create client picker failed: %v\n", err)
		os.Exit(1)
	}
	group.RegisterPeers(picker)

	fmt.Printf("server starting addr=%s svc=%s group=%s etcd=%v\n", *addr, *svc, *groupName, etcdEndpoints)

	go func() {
		if err := srv.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "server stopped with error: %v\n", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	fmt.Println("shutting down server...")
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
