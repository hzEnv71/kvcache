package consistenthash

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
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
	keys []int
	// 哈希环到节点的映射
	hashMap map[int]string
	// 节点到虚拟节点数量的映射
	nodeReplicas map[string]int
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

	// m.startBalancer() // 启动负载均衡器
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

	node := m.hashMap[m.keys[idx]]

	// 统计信息（用于可选负载分析）
	m.nodeCounts[node] = m.nodeCounts[node] + 1
	atomic.AddInt64(&m.totalRequests, 1)

	return node
}





// checkAndRebalance 检查并重新平衡虚拟节点
func (m *Map) checkAndRebalance() {
	if atomic.LoadInt64(&m.totalRequests) < 1000 {
		return // 样本太少，不进行调整
	}

	// 计算负载情况
	avgLoad := float64(m.totalRequests) / float64(len(m.nodeReplicas))
	var maxDiff float64

	for _, count := range m.nodeCounts {
		diff := math.Abs(float64(count) - avgLoad)
		if diff/avgLoad > maxDiff {
			maxDiff = diff / avgLoad
		}
	}

	// 如果负载不均衡度超过阈值，调整虚拟节点
	if maxDiff > m.config.LoadBalanceThreshold {
		m.rebalanceNodes()
	}
}

// rebalanceNodes 重新平衡节点
func (m *Map) rebalanceNodes() {
	m.mu.Lock()
	defer m.mu.Unlock()

	avgLoad := float64(m.totalRequests) / float64(len(m.nodeReplicas))

	// 调整每个节点的虚拟节点数量
	for node, count := range m.nodeCounts {
		currentReplicas := m.nodeReplicas[node]
		loadRatio := float64(count) / avgLoad

		var newReplicas int
		if loadRatio > 1 {
			// 负载过高，减少虚拟节点
			newReplicas = int(float64(currentReplicas) / loadRatio)
		} else {
			// 负载过低，增加虚拟节点
			newReplicas = int(float64(currentReplicas) * (2 - loadRatio))
		}

		// 确保在限制范围内
		if newReplicas < m.config.MinReplicas {
			newReplicas = m.config.MinReplicas
		}
		if newReplicas > m.config.MaxReplicas {
			newReplicas = m.config.MaxReplicas
		}

		if newReplicas != currentReplicas {
			// 重新添加节点的虚拟节点
			if err := m.Remove(node); err != nil {
				continue // 如果移除失败，跳过这个节点
			}
			m.addNode(node, newReplicas)
		}
	}

	// 重置计数器
	for node := range m.nodeCounts {
		m.nodeCounts[node] = 0
	}
	atomic.StoreInt64(&m.totalRequests, 0)

	// 重新排序
	sort.Ints(m.keys)
}

// GetStats 获取负载统计信息
func (m *Map) GetStats() map[string]float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := make(map[string]float64)
	total := atomic.LoadInt64(&m.totalRequests)
	if total == 0 {
		return stats
	}

	for node, count := range m.nodeCounts {
		stats[node] = float64(count) / float64(total)
	}
	return stats
}

// 将checkAndRebalance移到单独的goroutine中
func (m *Map) startBalancer() {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for range ticker.C {
			m.checkAndRebalance()
		}
	}()
}
