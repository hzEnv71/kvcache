package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	pb "KVCache/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	//解析命令行参数
	op := flag.String("op", "get", "operation: get|set|delete")
	addr := flag.String("addr", "127.0.0.1:8001", "target server address")
	group := flag.String("group", "test", "cache group")
	key := flag.String("key", "", "cache key")
	value := flag.String("value", "", "cache value (for set)")
	timeout := flag.Duration("timeout", 3*time.Second, "request timeout")
	flag.Parse()

	if *key == "" {
		fmt.Fprintln(os.Stderr, "--key is required")
		os.Exit(1)
	}

	if *op == "set" && *value == "" {
		fmt.Fprintln(os.Stderr, "--value is required for set")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	//创建gRPC连接
	conn, err := grpc.DialContext(
		ctx,
		*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial failed: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	//创建gRPC客户端
	cli := pb.NewKVCacheClient(conn)
	//创建请求
	req := &pb.Request{Group: *group, Key: *key, Value: []byte(*value)}
	//根据操作类型调用不同的方法
	switch *op {
	case "get":
		resp, err := cli.Get(ctx, req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "get failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("GET ok key=%s value=%s\n", *key, string(resp.GetValue()))
	case "set":
		resp, err := cli.Set(ctx, req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "set failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("SET ok key=%s value=%s\n", *key, string(resp.GetValue()))
	case "delete":
		resp, err := cli.Delete(ctx, req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "delete failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("DELETE ok key=%s deleted=%v\n", *key, resp.GetValue())
	default:
		fmt.Fprintln(os.Stderr, "invalid --op, expected get|set|delete")
		os.Exit(1)
	}
}
