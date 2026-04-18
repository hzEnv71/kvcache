package kvcache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"KVCache/consistenthash"
	registry "KVCache/etcd"

	"github.com/sirupsen/logrus"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const defaultSvcName = "kv-cache"

// Peer 定义了缓存节点的接口 //client.go实现该接口
type Peer interface {
	Get(group string, key string) ([]byte, error)
	Set(ctx context.Context, group string, key string, value []byte) error
	Delete(ctx context.Context, group string, key string) (bool, error)
	Close() error
}

// PeerPicker 定义了peer选择器的接口
type PeerPicker interface {
	PickPeer(key string) (peer Peer, ok bool, self bool)
	Close() error
}

// ClientPicker 实现 PeerPicker，负责“服务发现 + 路由选择 + 连接管理”。
//
// 字段说明：
// - selfAddr: 当前节点地址，用于识别 owner 是否为自己；
// - consHash: 一致性哈希环，决定 key 的 owner 节点；
// - clients: 远端节点地址到 gRPC 客户端连接的映射；
// - etcdCli: etcd 客户端，用于全量拉取和 watch 节点变更；
// - ctx/cancel: 生命周期控制，Close 时统一停止后台协程。
//
// 并发说明：
// - 路由读取（PickPeer）走读锁；
// - 成员增删与连接变更走写锁，保证 clients 与哈希环一致。
type ClientPicker struct {
	selfAddr string
	svcName  string
	mu       sync.RWMutex
	consHash *consistenthash.Map
	clients  map[string]*Client
	etcdCli  *clientv3.Client
	ctx      context.Context
	cancel   context.CancelFunc
}

// PickerOption 定义配置选项
type PickerOption func(*ClientPicker)

// WithServiceName 设置服务名称
func WithServiceName(name string) PickerOption {
	return func(p *ClientPicker) {
		p.svcName = name
	}
}

// PrintPeers 打印当前已发现的节点（仅用于调试）
func (p *ClientPicker) PrintPeers() {
	p.mu.RLock()
	defer p.mu.RUnlock()

	logrus.Printf("当前已发现的节点:")
	for addr := range p.clients {
		logrus.Printf("- %s", addr)
	}
}

// PrintRingState 打印当前哈希环状态（仅用于调试）
func (p *ClientPicker) PrintRingState(key string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	logrus.Printf("[ring] self=%s svc=%s key=%s", p.selfAddr, p.svcName, key)
	logrus.Printf("[ring] ring nodes:")
	for _, node := range p.consHash.DumpNodes() {
		logrus.Printf("  - %s", node)
	}
	logrus.Printf("[ring] clients:")
	for addr := range p.clients {
		logrus.Printf("  - %s", addr)
	}
	owner := p.consHash.Get(key)
	logrus.Printf("[ring] owner=%s", owner)
}

// NewClientPicker 创建新的ClientPicker实例
func NewClientPicker(addr string, opts ...PickerOption) (*ClientPicker, error) {
	ctx, cancel := context.WithCancel(context.Background())
	picker := &ClientPicker{
		selfAddr: addr,
		svcName:  defaultSvcName,
		clients:  make(map[string]*Client),
		consHash: consistenthash.New(),
		ctx:      ctx,
		cancel:   cancel,
	}

	for _, opt := range opts {
		opt(picker)
	}
	//创建etcd客户端
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   registry.DefaultConfig.Endpoints,
		DialTimeout: registry.DefaultConfig.DialTimeout,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create etcd client: %v", err)
	}
	picker.etcdCli = cli

	// 启动服务发现
	if err := picker.startServiceDiscovery(); err != nil {
		cancel()
		cli.Close()
		return nil, err
	}

	return picker, nil
}

// startServiceDiscovery 启动服务发现流程：
// 1) 先全量拉取一次当前在线节点；
// 2) 再开启 watch 订阅增量变更。
//
// 这样可以避免仅依赖 watch 时“错过历史事件”的问题。
func (p *ClientPicker) startServiceDiscovery() error {
	if err := p.fetchAllServices(); err != nil {
		return err
	}

	go p.watchServiceChanges() //异步监听etcd中服务目录变化
	return nil
}

// fetchAllServices 获取所有服务实例
func (p *ClientPicker) fetchAllServices() error {
	ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
	defer cancel()

	resp, err := p.etcdCli.Get(ctx, "/services/"+p.svcName, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("failed to get all services: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, kv := range resp.Kvs {
		addr := string(kv.Value)
		if addr != "" {
			p.addMember(addr)
			if addr != p.selfAddr {
				p.ensureClient(addr)
				logrus.Infof("Discovered service at %s", addr)
			}
		}
	}
	return nil
}

// watchServiceChanges 持续监听 etcd 中服务目录变化。
//
// 监听到事件后交由 handleWatchEvents 统一处理，
// 生命周期由 p.ctx 控制，Close 后会退出协程。
func (p *ClientPicker) watchServiceChanges() {
	watcher := clientv3.NewWatcher(p.etcdCli)
	defer watcher.Close()
	watchChan := watcher.Watch(p.ctx, "/services/"+p.svcName, clientv3.WithPrefix(), clientv3.WithPrevKV())
	for {
		select {
		case <-p.ctx.Done():
			return
		case resp, ok := <-watchChan:
			if !ok {
				time.Sleep(time.Second)
				watchChan = watcher.Watch(p.ctx, "/services/"+p.svcName, clientv3.WithPrefix(), clientv3.WithPrevKV())
				continue
			}
			if resp.Err() != nil {
				time.Sleep(time.Second)
				watchChan = watcher.Watch(p.ctx, "/services/"+p.svcName, clientv3.WithPrefix(), clientv3.WithPrevKV())
				continue
			}
			p.handleWatchEvents(resp.Events)
		}
	}
}

// handleWatchEvents 处理 etcd watch 事件。
//
// 处理策略：
// - Put: 新节点上线，创建 client 并加入哈希环；
// - Delete: 节点下线，关闭连接并从哈希环移除。
//
// 这里持有写锁，保证 clients 与 consHash 的一致更新。
func (p *ClientPicker) handleWatchEvents(events []*clientv3.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, event := range events {
		addr := string(event.Kv.Value)
		if event.Type == clientv3.EventTypeDelete {
			addr = string(event.PrevKv.Value)
		}
		if addr == "" {
			continue
		}

		switch event.Type {
		case clientv3.EventTypePut: //新增节点
			p.addMember(addr)
			if addr != p.selfAddr {
				p.ensureClient(addr)
				logrus.Infof("New service discovered at %s", addr)
			}
		case clientv3.EventTypeDelete: //删除节点
			p.removeMember(addr)
			if client, exists := p.clients[addr]; exists && addr != p.selfAddr { //如果节点存在 则关闭连接 并从哈希环和clients映射中移除
				delete(p.clients, addr)
				client.Close()
				logrus.Infof("Service removed at %s", addr)
			}
		}
	}
}

// addMember 将节点加入哈希环
func (p *ClientPicker) addMember(addr string) {
	if err := p.consHash.Add(addr); err != nil {
		logrus.Errorf("Failed to add member %s to hash ring: %v", addr, err)
	}
}

// ensureClient 为远端节点创建连接并缓存
func (p *ClientPicker) ensureClient(addr string) {
	if _, exists := p.clients[addr]; exists {
		return
	}
	if client, err := NewClient(addr, p.svcName, p.etcdCli); err == nil {
		p.clients[addr] = client
		logrus.Infof("Successfully created client for %s", addr)
	} else {
		logrus.Errorf("Failed to create client for %s: %v", addr, err)
	}
}

// removeMember 从哈希环移除节点
func (p *ClientPicker) removeMember(addr string) {
	if err := p.consHash.Remove(addr); err != nil {
		logrus.Errorf("Failed to remove member %s from hash ring: %v", addr, err)
	}
}

// PickPeer 根据 key 选择 owner 节点。
//
// 返回值语义：
// - peer: 远端节点 client（若 owner 是远端且连接存在）
// - ok:   是否成功选到可处理该 key 的节点
// - self: owner 是否为本机
//
// 上层通常这样使用：
// - ok=false: 认为当前无可用 owner（发现或连接未就绪）；
// - self=true: 本地处理；
// - self=false: 走 peer 远程调用。
func (p *ClientPicker) PickPeer(key string) (Peer, bool, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if addr := p.consHash.Get(key); addr != "" {
		isSelf := addr == p.selfAddr
		logrus.Infof("key=%s owner=%s self=%v", key, addr, isSelf)
		if isSelf {
			return nil, true, true
		}
		if client, ok := p.clients[addr]; ok {
			return client, true, false
		}
	}
	return nil, false, false
}

// Close 关闭所有资源
func (p *ClientPicker) Close() error {
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()

	var errs []error
	for addr, client := range p.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close client %s: %v", addr, err))
		}
	}

	if err := p.etcdCli.Close(); err != nil {
		errs = append(errs, fmt.Errorf("failed to close etcd client: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors while closing: %v", errs)
	}
	return nil
}

// parseAddrFromKey 从etcd key中解析地址
func parseAddrFromKey(key, svcName string) string {
	prefix := fmt.Sprintf("/services/%s/", svcName)
	if strings.HasPrefix(key, prefix) {
		return strings.TrimPrefix(key, prefix)
	}
	return ""
}
