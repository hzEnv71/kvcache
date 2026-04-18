package consistenthash

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// Map 一致性哈希实现。
//
// 数据结构说明：
// - keys:     排序后的虚拟节点哈希值切片（形成环）
// - hashMap:  虚拟节点哈希值 -> 真实节点地址
// - nodeReplicas: 真实节点 -> 虚拟节点副本数（权重）
// - nodeCounts/totalRequests: 负载统计（用于可选重平衡）
//
// 并发模型：
// - 所有结构性变更（Add/Remove）走写锁；
// - Get 为了更新统计计数，当前也使用写锁路径保证一致性。
type Map struct {
	mu sync.RWMutex
	// 配置信息
	config *Config
	// 哈希环
	keys []int // 排序后的虚拟节点哈希值切片（形成环）
	// 哈希环到节点的映射
	hashMap map[int]string // 虚拟节点哈希值 -> 真实节点地址
	// 节点到虚拟节点数量的映射
	nodeReplicas map[string]int // 真实节点 -> 虚拟节点副本数
	// 节点负载统计
	nodeCounts map[string]int64
	// 总请求数
	totalRequests int64
}

// New 创建一致性哈希实例
func New(opts ...Option) *Map {
	m := &Map{
		config:       DefaultConfig,
		hashMap:      make(map[int]string),
		nodeReplicas: make(map[string]int),
		nodeCounts:   make(map[string]int64),
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// Option 配置选项
type Option func(*Map)

// WithConfig 设置配置
func WithConfig(config *Config) Option {
	return func(m *Map) {
		m.config = config
	}
}

// Add 添加真实节点。
//
// 每个真实节点会展开为若干虚拟节点写入哈希环，
// 副本数由 DefaultReplicas 决定。
func (m *Map) Add(nodes ...string) error {
	if len(nodes) == 0 {
		return errors.New("no nodes provided")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, node := range nodes {
		if node == "" {
			continue
		}

		// 为节点添加虚拟节点
		m.addNode(node, m.config.DefaultReplicas)
	}
	// 哈希环需要保持有序，Get 时才能二分查找
	sort.Ints(m.keys)
	return nil
}

// addNode 把真实节点展开为 replicas 个虚拟节点并写入环。
// 约定虚拟节点命名为 node-i，确保可重建与可删除。
func (m *Map) addNode(node string, replicas int) {
	for i := 0; i < replicas; i++ {
		virtualNode := fmt.Sprintf("%s-%d", node, i)
		// 计算虚拟节点的哈希值
		hash := int(m.config.HashFunc([]byte(virtualNode)))
		// 将虚拟节点哈希值添加到环中
		m.keys = append(m.keys, hash)
		// 将虚拟节点哈希值到真实节点的映射
		m.hashMap[hash] = node
	}
	m.nodeReplicas[node] = replicas
}

// Remove 移除真实节点及其所有虚拟节点。
func (m *Map) Remove(node string) error {
	if node == "" {
		return errors.New("invalid node")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	replicas := m.nodeReplicas[node]
	if replicas == 0 {
		return fmt.Errorf("node %s not found", node)
	}

	// 逐个删除该真实节点对应的虚拟节点
	for i := 0; i < replicas; i++ {
		virtualNode := fmt.Sprintf("%s-%d", node, i)
		hash := int(m.config.HashFunc([]byte(virtualNode)))
		// 删除虚拟节点哈希值到真实节点的映射
		delete(m.hashMap, hash)
		// 删除虚拟节点哈希值到环中的映射
		for j := 0; j < len(m.keys); j++ {
			if m.keys[j] == hash {
				m.keys = append(m.keys[:j], m.keys[j+1:]...)
				break
			}
		}
	}

	delete(m.nodeReplicas, node)
	delete(m.nodeCounts, node)
	return nil
}

// Get 根据 key 选择 owner 节点。
//
// 算法步骤：
// 1) 对 key 计算哈希值；
// 2) 在有序 keys 中二分找到第一个 >= keyHash 的虚拟节点；
// 3) 若越界则回绕到 0（环结构）；
// 4) 通过 hashMap 映射回真实节点。
func (m *Map) Get(key string) string {
	if key == "" {
		return ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.keys) == 0 {
		return ""
	}

	hash := int(m.config.HashFunc([]byte(key)))
	idx := sort.Search(len(m.keys), func(i int) bool {
		return m.keys[i] >= hash
	})

	// 回绕：命中到环尾之后回到环头
	if idx == len(m.keys) {
		idx = 0
	}
	//虚拟节点哈希值 通过hashMap 映射回真实节点
	node := m.hashMap[m.keys[idx]]

	// 统计信息（用于可选负载分析）
	m.nodeCounts[node] = m.nodeCounts[node] + 1
	atomic.AddInt64(&m.totalRequests, 1)

	return node
}

// DumpNodes 返回当前哈希环中的真实节点列表（去重、排序）。
func (m *Map) DumpNodes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]struct{})
	for _, node := range m.hashMap { //遍历hashMap 将节点加入seen
		seen[node] = struct{}{}
	}

	nodes := make([]string, 0, len(seen))
	for node := range seen { //遍历seen 将节点加入nodes
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return nodes
}
