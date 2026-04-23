package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "KVCache/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type result struct {
	dur time.Duration
	err error
}

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:8001", "target server address")
		group    = flag.String("group", "test", "cache group")
		op       = flag.String("op", "get", "operation: get|set|delete|mixed")
		keyCount = flag.Int("keys", 1000, "number of unique keys to pre-generate")
		gor      = flag.Int("c", 64, "concurrency")
		n        = flag.Int("n", 10000, "total requests")
		timeout  = flag.Duration("timeout", 3*time.Second, "rpc timeout")
		setPct   = flag.Int("set-pct", 20, "set percentage for mixed")
		delPct   = flag.Int("delete-pct", 10, "delete percentage for mixed")
		warmup   = flag.Bool("warmup", true, "preload keys before benchmark")
	)
	flag.Parse()

	keys := make([]string, *keyCount)
	for i := 0; i < *keyCount; i++ {
		keys[i] = fmt.Sprintf("k%d", i)
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()

	conn, err := grpc.DialContext(
		dialCtx,
		*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial failed: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	cli := pb.NewKVCacheClient(conn)

	if *warmup {
		fmt.Println("warming up...")
		warmCtx, warmCancel := context.WithTimeout(context.Background(), time.Duration(len(keys))*(*timeout))
		defer warmCancel()
		preload(warmCtx, cli, *group, keys)
	}

	fmt.Printf("start bench op=%s addr=%s group=%s c=%d n=%d keys=%d\n", *op, *addr, *group, *gor, *n, *keyCount)
	results := make(chan result, *n)

	var sent int64
	start := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, *gor)

	for i := 0; i < *n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(i)))
			key := keys[r.Intn(len(keys))]
			value := fmt.Sprintf("v-%d-%d", i, r.Intn(1000000))
			bstart := time.Now()

			rpcCtx, rpcCancel := context.WithTimeout(context.Background(), *timeout)
			defer rpcCancel()

			err := doOne(rpcCtx, cli, *group, *op, key, value, *setPct, *delPct, r)
			results <- result{dur: time.Since(bstart), err: err}
			atomic.AddInt64(&sent, 1)
		}(i)
	}

	wg.Wait()
	close(results)

	var (
		success int
		failure int
		durs    []time.Duration
	)
	for r := range results {
		if r.err != nil {
			failure++
			continue
		}
		success++
		durs = append(durs, r.dur)
	}

	elapsed := time.Since(start)
	printReport(success, failure, durStats(durs), elapsed, int(sent))
}

func preload(ctx context.Context, cli pb.KVCacheClient, group string, keys []string) {
	for i, key := range keys {
		_, _ = cli.Set(ctx, &pb.Request{Group: group, Key: key, Value: []byte(fmt.Sprintf("init-%d", i))})
	}
}

func doOne(ctx context.Context, cli pb.KVCacheClient, group, op, key, value string, setPct, delPct int, r *rand.Rand) error {
	switch op {
	case "get":
		_, err := cli.Get(ctx, &pb.Request{Group: group, Key: key})
		return err
	case "set":
		_, err := cli.Set(ctx, &pb.Request{Group: group, Key: key, Value: []byte(value)})
		return err
	case "delete":
		_, err := cli.Delete(ctx, &pb.Request{Group: group, Key: key})
		return err
	case "mixed":
		x := r.Intn(100)
		switch {
		case x < setPct:
			_, err := cli.Set(ctx, &pb.Request{Group: group, Key: key, Value: []byte(value)})
			return err
		case x < setPct+delPct:
			_, err := cli.Delete(ctx, &pb.Request{Group: group, Key: key})
			return err
		default:
			_, err := cli.Get(ctx, &pb.Request{Group: group, Key: key})
			return err
		}
	default:
		return fmt.Errorf("invalid op=%s", op)
	}
}

type stats struct {
	min  time.Duration
	max  time.Duration
	avg  time.Duration
	p50  time.Duration
	p95  time.Duration
	p99  time.Duration
}

func durStats(durs []time.Duration) stats {
	if len(durs) == 0 {
		return stats{}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	var sum time.Duration
	for _, d := range durs {
		sum += d
	}
	return stats{
		min: durs[0],
		max: durs[len(durs)-1],
		avg: sum / time.Duration(len(durs)),
		p50: durs[len(durs)/2],
		p95: durs[int(float64(len(durs))*0.95)],
		p99: durs[int(float64(len(durs))*0.99)],
	}
}

func printReport(success, failure int, s stats, elapsed time.Duration, sent int) {
	tps := float64(success) / elapsed.Seconds()
	fmt.Println(strings.Repeat("-", 72))
	fmt.Printf("总请求数=%d 成功=%d 失败=%d 总耗时=%s 吞吐量TPS=%.2f\n", sent, success, failure, elapsed, tps)
	fmt.Printf("延迟统计 最小=%s 平均=%s P50=%s P95=%s P99=%s 最大=%s\n", s.min, s.avg, s.p50, s.p95, s.p99, s.max)
	fmt.Printf("当前Go协程数=%d\n", runtime.NumGoroutine())
	fmt.Println(strings.Repeat("-", 72))
}
