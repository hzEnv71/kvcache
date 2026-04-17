package kvcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"KVCache/singleflight"

	"github.com/sirupsen/logrus"
)

var (
	groupsMu sync.RWMutex
	groups   = make(map[string]*Group)
)

// ErrKeyRequired 键不能为空错误
var ErrKeyRequired = errors.New("key is required")

// ErrValueRequired 值不能为空错误
var ErrValueRequired = errors.New("value is required")

// ErrGroupClosed 组已关闭错误
var ErrGroupClosed = errors.New("cache group is closed")

// Getter 加载键值的回调函数接口
type Getter interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// GetterFunc 函数类型实现 Getter 接口
type GetterFunc func(ctx context.Context, key string) ([]byte, error)

// Get 实现 Getter 接口
func (f GetterFunc) Get(ctx context.Context, key string) ([]byte, error) {
	return f(ctx, key)
}

// Group 是一个缓存命名空间（业务入口核心对象）。
//
// 设计职责：
// 1) 对外提供 Get/Set/Delete 等缓存能力；
// 2) 维护本地缓存与分布式 peer 路由；
// 3) 在 miss 场景下通过 singleflight 合并并发加载；
// 4) 记录统计信息，便于观察命中率与加载开销。
type Group struct {
	name       string              // 组名
	getter     Getter              // 数据源回调
	mainCache  *Cache              // 本地缓存
	peers      PeerPicker          // 分布式节点选择器
	loader     *singleflight.Group // 并发加载器
	expiration time.Duration       // 缓存过期时间，0表示永不过期
	closed     int32               // 原子变量，标记组是否已关闭
	stats      groupStats          // 统计信息
}

// groupStats 保存组的统计信息
type groupStats struct {
	loads        int64 // 加载次数
	localHits    int64 // 本地缓存命中次数
	localMisses  int64 // 本地缓存未命中次数
	peerHits     int64 // 从对等节点获取成功次数
	peerMisses   int64 // 从对等节点获取失败次数
	loaderHits   int64 // 从加载器获取成功次数
	loaderErrors int64 // 从加载器获取失败次数
	loadDuration int64 // 加载总耗时（纳秒）
}

// GroupOption 定义Group的配置选项
type GroupOption func(*Group)

// WithExpiration 设置缓存过期时间
func WithExpiration(d time.Duration) GroupOption {
	return func(g *Group) {
		g.expiration = d
	}
}

// WithPeers 设置分布式节点
func WithPeers(peers PeerPicker) GroupOption {
	return func(g *Group) {
		g.peers = peers
	}
}

// WithCacheOptions 设置缓存选项
func WithCacheOptions(opts CacheOptions) GroupOption {
	return func(g *Group) {
		g.mainCache = NewCache(opts)
	}
}

// NewGroup 创建一个新的 Group 实例
func NewGroup(name string, cacheBytes int64, getter Getter, opts ...GroupOption) *Group {
	if getter == nil {
		panic("nil Getter")
	}

	// 创建默认缓存选项
	cacheOpts := DefaultCacheOptions()
	cacheOpts.MaxBytes = cacheBytes

	g := &Group{
		name:      name,
		getter:    getter,
		mainCache: NewCache(cacheOpts),
		loader:    &singleflight.Group{},
	}

	// 应用选项
	for _, opt := range opts {
		opt(g)
	}

	// 注册到全局组映射
	groupsMu.Lock()
	defer groupsMu.Unlock()

	if _, exists := groups[name]; exists {
		logrus.Warnf("Group with name %s already exists, will be replaced", name)
	}

	groups[name] = g
	logrus.Infof("Created cache group [%s] with cacheBytes=%d, expiration=%v", name, cacheBytes, g.expiration)

	return g
}

// GetGroup 获取指定名称的组
func GetGroup(name string) *Group {
	groupsMu.RLock()
	defer groupsMu.RUnlock()
	return groups[name]
}

// Get 从缓存获取数据
func (g *Group) Get(ctx context.Context, key string) (ByteView, error) {
	// 检查组是否已关闭
	if atomic.LoadInt32(&g.closed) == 1 {
		return ByteView{}, ErrGroupClosed
	}

	if key == "" {
		return ByteView{}, ErrKeyRequired
	}

	// 从本地缓存获取
	view, ok := g.mainCache.Get(ctx, key)
	if ok {
		atomic.AddInt64(&g.stats.localHits, 1)
		return view, nil
	}

	atomic.AddInt64(&g.stats.localMisses, 1)

	// 尝试从其他节点获取或加载
	return g.load(ctx, key)
}

// Set 设置缓存值。
//
// 当前语义：
// 1) 先写本地缓存；
// 2) 若请求不是来自 peer 转发，则异步向 owner 节点同步写（尽力同步）。
//
// 注意：这是“最终一致”写路径，返回成功并不代表远端已完成同步。
func (g *Group) Set(ctx context.Context, key string, value []byte) error {
	// 基础状态与参数校验
	if atomic.LoadInt32(&g.closed) == 1 {
		return ErrGroupClosed
	}
	if key == "" {
		return ErrKeyRequired
	}
	if len(value) == 0 {
		return ErrValueRequired
	}

	// 标记来源：若来自 peer，同步逻辑不再继续转发，避免环路
	isPeerRequest := isFromPeer(ctx)

	// 本地写入缓存（写路径优先保证本机可读）
	view := ByteView{b: cloneBytes(value)}
	if g.expiration > 0 {
		g.mainCache.AddWithExpiration(key, view, time.Now().Add(g.expiration))
	} else {
		g.mainCache.Add(key, view)
	}

	// 非 peer 请求时，异步同步到 owner 节点（不阻塞当前请求）
	if !isPeerRequest && g.peers != nil {
		go g.syncToPeers(ctx, "set", key, value)
	}

	return nil
}

// Delete 删除缓存值。
//
// 严格 owner-only：
// - 必须先路由到 owner；
// - owner 是远端：直接删远端；
// - owner 是本机：删本地缓存；
// - peer 转发请求：只做本地删，不再继续路由。
func (g *Group) Delete(ctx context.Context, key string) error {
	if atomic.LoadInt32(&g.closed) == 1 {
		return ErrGroupClosed
	}
	if key == "" {
		return ErrKeyRequired
	}

	if isFromPeer(ctx) {
		g.mainCache.Delete(key)
		return nil
	}

	if g.peers == nil {
		g.mainCache.Delete(key)
		return nil
	}

	peer, ok, isSelf := g.peers.PickPeer(key)
	if !ok {
		return fmt.Errorf("owner peer unavailable for key=%s", key)
	}

	if !isSelf {
		syncCtx := peerContext(context.Background())
		if _, err := peer.Delete(syncCtx, g.name, key); err != nil {
			return fmt.Errorf("failed to delete from owner peer: %w", err)
		}
		return nil
	}

	g.mainCache.Delete(key)
	return nil
}

// syncToPeers 旧的异步同步路径已不再作为主流程使用。
// 当前 Set/Delete 直接路由 owner，因此这里保留为兼容占位。
func (g *Group) syncToPeers(ctx context.Context, op string, key string, value []byte) {
	if g.peers == nil {
		return
	}

	peer, ok, isSelf := g.peers.PickPeer(key)
	if !ok || isSelf {
		return
	}

	syncCtx := peerContext(context.Background())//设置from_peer=true

	var err error
	switch op {
	case "set":
		err = peer.Set(syncCtx, g.name, key, value)
	case "delete":
		_, err = peer.Delete(syncCtx, g.name, key)
	}

	if err != nil {
		logrus.Errorf("[KVCache] failed to sync %s to peer: %v", op, err)
	}
}

// Clear 清空缓存
func (g *Group) Clear() {
	// 检查组是否已关闭
	if atomic.LoadInt32(&g.closed) == 1 {
		return
	}

	g.mainCache.Clear()
	logrus.Infof("[KVCache] cleared cache for group [%s]", g.name)
}

// Close 关闭组并释放资源
func (g *Group) Close() error {
	// 如果已经关闭，直接返回
	if !atomic.CompareAndSwapInt32(&g.closed, 0, 1) {
		return nil
	}

	// 关闭本地缓存
	if g.mainCache != nil {
		g.mainCache.Close()
	}

	// 从全局组映射中移除
	groupsMu.Lock()
	delete(groups, g.name)
	groupsMu.Unlock()

	logrus.Infof("[KVCache] closed cache group [%s]", g.name)
	return nil
}

// load 加载数据
func (g *Group) load(ctx context.Context, key string) (value ByteView, err error) {
	// 使用 singleflight 确保并发请求只加载一次
	startTime := time.Now()
	viewi, err := g.loader.Do(key, func() (interface{}, error) {
		return g.loadData(ctx, key)
	})

	// 记录加载时间
	loadDuration := time.Since(startTime).Nanoseconds()
	atomic.AddInt64(&g.stats.loadDuration, loadDuration)
	atomic.AddInt64(&g.stats.loads, 1)

	if err != nil {
		atomic.AddInt64(&g.stats.loaderErrors, 1)
		return ByteView{}, err
	}

	view := viewi.(ByteView)

	// 设置到本地缓存
	if g.expiration > 0 {
		g.mainCache.AddWithExpiration(key, view, time.Now().Add(g.expiration))
	} else {
		g.mainCache.Add(key, view)
	}

	return view, nil
}

// loadData 实际加载数据的方法
func (g *Group) loadData(ctx context.Context, key string) (value ByteView, err error) {
	// 尝试从远程节点获取
	if g.peers != nil {
		peer, ok, isSelf := g.peers.PickPeer(key)
		if ok && !isSelf {
			value, err := g.getFromPeer(ctx, peer, key)
			if err == nil {
				atomic.AddInt64(&g.stats.peerHits, 1)
				return value, nil
			}

			atomic.AddInt64(&g.stats.peerMisses, 1)
			logrus.Warnf("[KVCache] failed to get from peer: %v", err)
		}
	}

	// 从数据源加载
	bytes, err := g.getter.Get(ctx, key) //目前是做空处理 打印找不到日志 后面可以扩展成从数据库中获取
	if err != nil {
		return ByteView{}, fmt.Errorf("failed to get data: %w", err)
	}

	atomic.AddInt64(&g.stats.loaderHits, 1)
	return ByteView{b: cloneBytes(bytes)}, nil
}

// getFromPeer 从其他节点获取数据
func (g *Group) getFromPeer(ctx context.Context, peer Peer, key string) (ByteView, error) {
	bytes, err := peer.Get(g.name, key)
	if err != nil {
		return ByteView{}, fmt.Errorf("failed to get from peer: %w", err)
	}
	return ByteView{b: bytes}, nil
}

// RegisterPeers 注册PeerPicker
func (g *Group) RegisterPeers(peers PeerPicker) {
	if g.peers != nil {
		panic("RegisterPeers called more than once")
	}
	g.peers = peers
	logrus.Infof("[KVCache] registered peers for group [%s]", g.name)
}

// Stats 返回缓存统计信息
func (g *Group) Stats() map[string]interface{} {
	stats := map[string]interface{}{
		"name":          g.name,
		"closed":        atomic.LoadInt32(&g.closed) == 1,
		"expiration":    g.expiration,
		"loads":         atomic.LoadInt64(&g.stats.loads),
		"local_hits":    atomic.LoadInt64(&g.stats.localHits),
		"local_misses":  atomic.LoadInt64(&g.stats.localMisses),
		"peer_hits":     atomic.LoadInt64(&g.stats.peerHits),
		"peer_misses":   atomic.LoadInt64(&g.stats.peerMisses),
		"loader_hits":   atomic.LoadInt64(&g.stats.loaderHits),
		"loader_errors": atomic.LoadInt64(&g.stats.loaderErrors),
	}

	// 计算各种命中率
	totalGets := stats["local_hits"].(int64) + stats["local_misses"].(int64)
	if totalGets > 0 {
		stats["hit_rate"] = float64(stats["local_hits"].(int64)) / float64(totalGets)
	}

	totalLoads := stats["loads"].(int64)
	if totalLoads > 0 {
		stats["avg_load_time_ms"] = float64(atomic.LoadInt64(&g.stats.loadDuration)) / float64(totalLoads) / float64(time.Millisecond)
	}

	// 添加缓存大小
	if g.mainCache != nil {
		cacheStats := g.mainCache.Stats()
		for k, v := range cacheStats {
			stats["cache_"+k] = v
		}
	}

	return stats
}

// ListGroups 返回所有缓存组的名称
func ListGroups() []string {
	groupsMu.RLock()
	defer groupsMu.RUnlock()

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}

	return names
}

// DestroyGroup 销毁指定名称的缓存组
func DestroyGroup(name string) bool {
	groupsMu.Lock()
	g, exists := groups[name]
	if exists {
		delete(groups, name)
	}
	groupsMu.Unlock()

	if exists {
		_ = g.Close()
		logrus.Infof("[KVCache] destroyed cache group [%s]", name)
		return true
	}

	return false
}

// DestroyAllGroups 销毁所有缓存组
func DestroyAllGroups() {
	groupsMu.Lock()
	all := make([]*Group, 0, len(groups))
	names := make([]string, 0, len(groups))
	for name, g := range groups {
		all = append(all, g)
		names = append(names, name)
		delete(groups, name)
	}
	groupsMu.Unlock()

	for i, g := range all {
		_ = g.Close()
		logrus.Infof("[KVCache] destroyed cache group [%s]", names[i])
	}
}
