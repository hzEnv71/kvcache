package kvcache

import (
	"context"
	"fmt"
	"time"

	pb "KVCache/pb"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Client 是节点间通信客户端，封装了对远端 KVCache 的 gRPC 调用。
//
// 典型生命周期：
// NewClient -> Get/Set/Delete 多次调用 -> Close 释放连接。
type Client struct {
	addr    string
	svcName string
	etcdCli *clientv3.Client
	conn    *grpc.ClientConn
	grpcCli pb.KVCacheClient
}

var _ Peer = (*Client)(nil)

// NewClient 创建远端节点客户端。
//
// 说明：
// - 允许复用外部 etcd client；
// - gRPC 连接采用阻塞拨号，确保返回时连接已可用或明确失败。
func NewClient(addr string, svcName string, etcdCli *clientv3.Client) (*Client, error) {
	var err error
	if etcdCli == nil {
		etcdCli, err = clientv3.New(clientv3.Config{
			Endpoints:   []string{"localhost:2379"},
			DialTimeout: 5 * time.Second,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create etcd client: %v", err)
		}
	}

	conn, err := grpc.Dial(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(10*time.Second),
		// grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to dial server: %v", err)
	}

	grpcClient := pb.NewKVCacheClient(conn)

	client := &Client{
		addr:    addr,
		svcName: svcName,
		etcdCli: etcdCli,
		conn:    conn,
		grpcCli: grpcClient,
	}

	return client, nil
}

func (c *Client) Get(group, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ctx = withPeerMetadata(ctx)
	resp, err := c.grpcCli.Get(ctx, &pb.Request{
		Group: group,
		Key:   key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get value from kvcache: %v", err)
	}

	return resp.GetValue(), nil
}

func (c *Client) Delete(ctx context.Context, group, key string) (bool, error) {
	ctx = withPeerMetadata(ctx)
	resp, err := c.grpcCli.Delete(ctx, &pb.Request{
		Group: group,
		Key:   key,
	})
	if err != nil {
		return false, fmt.Errorf("failed to delete value from kvcache: %v", err)
	}

	return resp.GetValue(), nil
}

func withPeerMetadata(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "from_peer", "true")
}

// Set 向远端节点写入 key/value。
//
// ctx 由上层传入，通常用于控制超时与取消。
func (c *Client) Set(ctx context.Context, group, key string, value []byte) error {
	ctx = withPeerMetadata(ctx)
	_, err := c.grpcCli.Set(ctx, &pb.Request{
		Group: group,
		Key:   key,
		Value: value,
	})
	if err != nil {
		return fmt.Errorf("failed to set value to kvcache: %v", err)
	}
	return nil
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
