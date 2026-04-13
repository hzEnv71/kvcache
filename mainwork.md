# KVCache 主要工作实现说明（mainwork）

本文用于解释简历中每一条“主要工作”在项目里的具体实现位置与执行逻辑。

---

## 1. 缓存核心实现

### 简历表述
- 缓存核心实现：封装本地缓存 Cache，支持 LRU/LRU-2 淘汰、TTL 过期清理与命中率统计。

### 代码体现
- `cache.go`
  - `type Cache`：统一封装本地缓存对象，管理底层 `store.Store`
  - `Add / AddWithExpiration / Get / Delete / Clear / Len / Close / Stats`
  - `hits/misses` 统计与命中率计算（`Stats`）
- `store/store.go`
  - 定义 `Store` 抽象接口
  - `NewStore(cacheType, opts)` 根据类型创建 LRU 或 LRU-2
- `store/lru.go`
  - 标准 LRU（双向链表 + map）
  - 过期映射 `expires` + 周期清理协程 `cleanupLoop`
- `store/lru2.go`
  - 两级缓存结构（一级 + 二级）
  - 分桶与哈希定位，减少锁竞争

### 关键机制
- TTL：`SetWithExpiration` + 清理协程定期淘汰过期 key
- 淘汰：
  - LRU：按最近最少使用顺序淘汰
  - LRU-2：通过二级提升识别稳定热点
- 统计：在 `cache.go` 内部做命中/未命中计数，输出 hit rate

---

## 2. 并发击穿防护

### 简历表述
- 并发击穿防护：实现 singleflight 合并并发请求，减少热点 key 的重复回源压力。

### 代码体现
- `singleflight/singleflight.go`
  - `Group.Do(key, fn)`：同 key 并发请求只允许一次 `fn` 执行
  - 其他并发请求等待 leader 执行结果并复用
- `group.go`
  - `load()` 中调用 `g.loader.Do(key, ...)`
  - 将缓存 miss 后的数据加载过程收敛为单飞

### 关键机制
- 热点 key 并发 miss 时避免 N 次回源
- 缓解下游数据源压力与突发抖动

---

## 3. 分布式路由设计

### 简历表述
- 分布式路由设计：基于一致性哈希实现 key 到 owner 节点路由，保证跨节点访问路径稳定。

### 代码体现
- `consistenthash/con_hash.go`
  - `Map`：哈希环实现
  - `Add / AddWithReplicas / Remove / Get`
  - `Get(key)` 返回 owner 节点
- `peers.go`
  - `ClientPicker` 持有 `consHash`
  - `PickPeer(key)` 返回 `(peer, ok, isSelf)`，用于判断 owner 是本机还是远端
- `group.go`
  - `Set/Delete`：根据 `PickPeer(key)` 同步路由到 owner
  - `Get.loadData`：owner 远端走 peer 获取；owner 本机走本地 getter

### 关键机制
- 同 key 稳定映射到固定 owner
- 减少全量重分布（相较普通 hash）
- 支持权重副本（`AddWithReplicas`）

---

## 4. 服务注册发现

### 简历表述
- 服务注册发现：基于 etcd 实现节点注册与动态发现，支持节点上下线感知与路由更新。

### 代码体现
- `registry/register.go`
  - `Register(svcName, addr, stopCh)`
  - 使用 etcd lease + keepalive 维持在线状态
  - 使用 `/services/{svc}/{addr}` 作为注册 key
- `peers.go`
  - `fetchAllServices()`：启动时全量拉取节点
  - `watchServiceChanges()`：持续 watch etcd 变更
  - `handleWatchEvents()`：处理 Put/Delete，更新成员连接与哈希环

### 关键机制
- 节点启动自动注册
- 节点下线（租约失效/删除）自动感知
- 发现变更后路由可更新

---

## 5. 节点通信实现

### 简历表述
- 节点通信实现：基于 gRPC 完成节点间 Get/Set/Delete 远程调用，支持分布式数据访问。

### 代码体现
- `pb/kvcache.proto`（或 `pb/*.pb.go`）
  - 定义 `KVCache` 服务与 `Get/Set/Delete` RPC
- `server.go`
  - gRPC 服务端启动与方法实现：`Get/Set/Delete`
  - 接收远端调用后转到 `Group` 层处理
- `client.go`
  - `Client` 封装 gRPC 客户端
  - `Get/Set/Delete` 调用远端节点接口
- `peers.go`
  - `ClientPicker` 维护 `addr -> Client` 连接池映射

### 关键机制
- 统一通过 proto 定义节点间协议
- 节点间透明远程访问
- 与一致性哈希结合实现“路由 + 调用”闭环

---

## 6. 一条完整请求链路（示例）

以 `Get(key)` 为例：

1. 客户端请求某节点 gRPC `Get`
2. 进入 `Group.Get`
3. 先查本地缓存（命中直接返回）
4. miss 后进入 `load`（singleflight 合并）
5. `PickPeer(key)` 选 owner
   - owner 远端：通过 `client.go` 发起 peer gRPC Get
   - owner 本机：调用本地 `getter.Get` 回源
6. 获取结果后回填本地缓存并返回

---

## 7. 与简历面试表达的对应关系（速记）

- “缓存核心实现” -> `cache.go + store/*`
- “防击穿” -> `singleflight/singleflight.go + group.go.load`
- “一致性哈希路由” -> `consistenthash/* + peers.PickPeer + group Set/Get`
- “服务发现” -> `registry/register.go + peers fetch/watch`
- “节点通信” -> `pb/* + client.go + server.go`

---

如果你后续要把这份文档改成“面试讲稿版”，可以再按“问题-方案-结果”结构补上压测数据（QPS、P95、命中率）。
