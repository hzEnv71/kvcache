# KVCache 学习文档（从 0 到 1）

> 目标：带你从“为什么做”到“怎么跑”，再到“每个模块如何协作”，系统掌握这个项目。
>
> 本文后半部分增加了“按模块逐层拆解函数与调用链”的详细说明，方便你从源码级别理解整个系统。

---

## 1. 项目一句话定位

`KVCache` 是一个用 Go 实现的**分布式缓存系统**，核心能力包含：

- 本地缓存（LRU / LRU-2）
- 缓存过期与淘汰
- 并发防击穿（singleflight）
- 一致性哈希选节点
- gRPC 服务化
- etcd 服务注册与发现

你可以把它理解为：

- 单机层面：像一个“内存 KV 缓存库”
- 多机层面：像一个“简化版缓存集群”

---

## 2. 先理解设计目标（为什么这么设计）

缓存系统通常要解决 4 类核心问题：

1. **快**：读写延迟低，尽量在内存完成。
2. **稳**：高并发下不击穿、不雪崩。
3. **省**：内存有限，必须有淘汰策略。
4. **可扩展**：单机不够时，能横向扩容。

KVCache 对应做法：

- 快：本地内存缓存 + O(1) map 查找
- 稳：singleflight 合并并发 miss 请求
- 省：LRU / LRU-2 + TTL 过期机制
- 扩展：一致性哈希分配 key 到节点 + etcd 发现节点 + gRPC 互访

---

## 3. 架构全景图（逻辑层）

可以把代码按 6 层看：

1. **业务入口层**：`group.go`
  - 面向使用者的核心 API：`Get/Set/Delete/Clear/Stats`
2. **本地缓存层**：`cache.go`
  - 把底层存储封装为统一缓存对象，含命中统计、生命周期管理
3. **存储引擎层**：`store/`
  - `lru.go`：经典 LRU
  - `lru2.go`：两级 LRU（提高热点识别能力）
4. **并发控制层**：`singleflight/`
  - 同一个 key 的并发加载只执行一次
5. **分布式路由层**：`consistenthash/` + `peers.go`
  - 一致性哈希决定 key 应该去哪个节点
  - ClientPicker 通过 etcd 感知节点变化
6. **通信与注册层**：`server.go` + `client.go` + `registry/`
  - gRPC 提供对外服务
  - etcd 负责注册与发现

---

## 4. 项目目录导读（建议阅读顺序）

建议这个顺序最容易上手：

1. `byteview.go`
  - 缓存值抽象，理解“只读视图 + 拷贝语义”
2. `cache.go`
  - 本地缓存壳层，理解 store 抽象
3. `store/store.go` + `store/lru.go`
  - 先掌握标准 LRU 的实现
4. `singleflight/singleflight.go`
  - 看并发请求合并
5. `group.go`
  - 这是核心：把缓存、peer、loader 串起来
6. `consistenthash/con_hash.go`
  - 看 key 如何路由到节点
7. `client.go` / `server.go` / `peers.go` / `registry/register.go`
  - 看分布式如何落地

---

## 5. 核心对象与职责

## 5.1 Group（最核心）

`Group` 是“缓存命名空间”，你可以理解成一个 cache namespace。

关键字段：

- `name`：组名
- `getter`：本地数据源回调（缓存 miss 时兜底）
- `mainCache`：本地缓存对象
- `peers`：节点选择器（分布式模式）
- `loader`：singleflight
- `expiration`：默认过期时间

关键行为：

- `Get`：本地命中 -> 按一致性哈希找 owner -> owner 读取或回源
- `Set/Delete`：先本地执行，再异步同步到 owner（最终一致）
- `Stats`：暴露命中率和加载统计

---

## 5.2 Cache（本地缓存壳层）

`Cache` 是对 `store.Store` 的封装，负责：

- 统一初始化（延迟创建底层 store）
- 统计命中/未命中
- 关闭与资源释放

`Cache` 不直接关心分布式，只处理“本机内存缓存”。

---

## 5.3 Store 接口（可插拔存储引擎）

`store.Store` 定义了最小能力：

- `Get/Set/Delete/Clear/Len/Close`
- `SetWithExpiration`

所以你后续可以继续扩展别的策略（如 LFU、ARC）而不改上层逻辑。

---

## 5.4 PeerPicker / Peer（分布式抽象）

- `PeerPicker`：给定 key 选一个节点
- `Peer`：对选中的节点执行 `Get/Set/Delete`

当前实现：

- `ClientPicker`（`peers.go`）+ `Client`（`client.go`）
- 用 etcd 做节点发现
- 用一致性哈希做 key 路由
- 用 gRPC 做节点通信

---

## 6. 关键执行流程（一定要掌握）

## 6.1 `Get` 流程（最重要）

1. 调用 `Group.Get(ctx, key)`
2. 先查 `mainCache.Get`
3. 命中：直接返回
4. 未命中：进入 `load`
5. `singleflight.Do(key, fn)` 合并并发 miss
6. `loadData`：
  - 先 `PickPeer(key)` 选 owner
  - owner 是远端：走 gRPC `peer.Get`
  - owner 是本机：回源 `getter.Get`
7. 拿到结果后写回本地缓存
8. 返回结果

这条链路是“owner 优先”的一致性路由，不再做随意回退。

---

## 6.2 `Set` 流程（当前实现）

1. `Group.Set`
2. 先写本地缓存（支持 TTL）
3. 如果不是 peer 转发请求（无 `from_peer`）且开启了 peers：
  - 异步 `syncToPeers("set")` 同步到 owner

说明：这是“本地先写 + 异步同步”的最终一致语义，返回成功不代表远端一定已完成。

---

## 6.3 `Delete` 流程（当前实现）

与 `Set` 对齐：

1. 先删本地缓存
2. 非 peer 请求时异步 `syncToPeers("delete")`

说明：删除链路同样是最终一致模型。

---

## 7. 并发与线程安全设计

项目中大量使用：

- `sync.RWMutex`：读多写少
- `sync.Mutex`：关键路径互斥
- `sync.Map`：并发 map 场景
- `atomic`：计数器 / 状态位

你学习时可以重点观察：

- 哪些地方用了读锁但不能写（典型并发坑）
- 哪些地方在锁中调用外部函数（容易死锁）
- 哪些地方 goroutine 异步执行（错误处理会不会丢）

---

## 8. 缓存策略：LRU vs LRU-2

## 8.1 LRU

特征：

- 最近最少使用淘汰
- 结构：双向链表 + map
- 优点：简单高效
- 缺点：偶发扫描流量会污染缓存

## 8.2 LRU-2（这里是两级缓存）

思路：

- 第一次访问先放一级
- 再次访问才提升到二级（更稳定的“热点”）
- 分桶 + 局部锁，降低全局锁竞争

这对区分“偶发访问”与“稳定热点”有帮助。

---

## 9. 分布式部分怎么串起来

## 9.1 服务端

`server.go` 做两件事：

- 启 gRPC 服务（`Get/Set/Delete`）
- 注册自己到 etcd

## 9.2 客户端（节点间）

`peers.go` 里的 `ClientPicker`：

- 启动时全量拉取服务节点
- 持续 watch etcd 增量变化
- 维护成员集合、客户端连接与一致性哈希环
- `PickPeer` 返回 `(peer, ok, isSelf)`，用于 owner 路由判定

## 9.3 请求路由

- 对 key 做 hash
- 在 hash ring 找 owner 节点
- owner 是远端则发 gRPC，owner 是本机则本地处理
- 所有节点应共享同一 ring 视图（这是正确性的前提）

## 9.4 为什么“分布式缓存项目”还要配 etcd（核心认知）

这是很多同学都会问到的点，面试里也很常见。

你要区分两条平面：

1. **数据面（Data Plane）**
  - 放业务缓存数据（热点 key/value）
  - 追求高吞吐、低延迟
  - 对应本项目：`Group + Cache + Store + gRPC 节点`
2. **控制面（Control Plane）**
  - 放服务元数据（节点地址、在线状态、租约、拓扑变更）
  - 追求一致性与可靠感知
  - 对应本项目：`etcd + registry + peers watcher`

所以不是“两个缓存重复造轮子”，而是分工不同：

- `KVCache` 负责存业务数据
- `etcd` 负责管节点与元信息

### 9.4.1 为什么不直接用 etcd 存业务缓存

因为 etcd 的设计目标不是高频缓存读写：

- etcd 强一致（Raft）带来更高写成本
- 适合小体量元数据、配置、服务发现
- 不适合大规模热点业务 KV 的高 QPS 读写

### 9.4.2 一句话面试表达（建议背）

> 这个项目是分布式缓存的数据面实现，etcd 只是控制面组件，用来做服务注册发现和节点管理；
> 两者不是替代关系，而是互补关系。

---

## 10. 你要知道的“已修复关键风险点”

你前面提的关键风险，已经按工程安全修复过：

1. 组销毁死锁风险（Destroy 中锁顺序问题）
2. Server 默认配置被污染（共享指针问题）
3. singleflight 并发窗口（Load/Store 非原子）
4. consistenthash 读锁下写 map
5. lru2 调试输出污染日志
6. server 侧误标记 `from_peer` 导致错误同步语义
7. 一致性哈希本地自动重平衡导致多节点 ring 不一致

学习意义：

- 这些都是高频面试/实战并发问题
- 你可以把它们写进项目复盘与简历亮点

---

## 11. 从 0 到 1 学习路线（建议一周节奏）

### Day 1：先跑起来

- 启动 etcd
- 启 3 个 server
- client 做 `set/get/delete`
- 观察日志，确认跨节点取值

### Day 2：单机缓存

- 精读 `cache.go` + `store/lru.go`
- 手画 LRU 数据结构变化图（插入、命中、淘汰）

### Day 3：并发控制

- 精读 `singleflight`
- 自己写并发压测同 key，观察只加载一次

### Day 4：Group 主流程

- 精读 `group.go`
- 画 `Get` 时序图

### Day 5：分布式路由

- 精读 `consistenthash` + `peers.go`
- 实测加节点/减节点时请求分布变化

### Day 6：稳定性与故障演练

- 关掉一个节点，观察回退行为
- 人为制造 getter 慢查询，观察 singleflight 效果

### Day 7：总结输出

- 写技术复盘：架构、关键 trade-off、已知边界
- 准备 8~10 个面试题答案

---

## 12. 调试与观察建议

推荐你重点观察这些指标：

- 本地命中率：`local_hits / (local_hits+local_misses)`
- peer 命中率：`peer_hits / (peer_hits+peer_misses)`
- 平均加载耗时：`avg_load_time_ms`
- 缓存大小与淘汰频次

同时做 3 类测试：

1. 功能测试：增删查、过期
2. 并发测试：热点 key 并发读
3. 故障测试：节点下线、etcd 抖动

---

## 13. 项目当前边界与可优化点（进阶）

可继续优化方向：

1. **动态负载均衡控制面**：由外部 controller 统一计算权重并下发，节点仅 watch+apply。
2. **批量操作**：支持批量 Get/Set 减少网络开销。
3. **可观测性**：接入 Prometheus 指标、OpenTelemetry tracing。
4. **配置管理**：统一 config 文件 + 热更新。
5. **服务化部署**：Windows Service / systemd / Docker 镜像。
6. **测试体系**：补齐 race test、integration test、benchmark。

---

# 16. 按模块逐层拆解：函数、职责、调用与执行流程

这一部分用于“从源码层面理解每个模块怎么协作”。如果你要真正读懂项目，建议按下面顺序看：

1. `cmd/server/main.go`
2. `cmd/client/main.go`
3. `server.go`
4. `group.go`
5. `cache.go`
6. `store/store.go`
7. `store/lru.go`
8. `store/lru2.go`
9. `singleflight/singleflight.go`
10. `consistenthash/con_hash.go`
11. `peers.go`
12. `client.go`
13. `registry/register.go`

---

## 16.1 `cmd/server/main.go`：服务启动入口

### 这个文件负责什么

它是“整个服务端程序”的入口，主要工作是：

- 解析命令行参数
- 构造 `Server`
- 构造 `Group`
- 构造 `ClientPicker`
- 把 `ClientPicker` 注册到 `Group`
- 启动 gRPC 服务
- 等待退出信号

### 主要函数

#### `main()`

核心步骤：

1. 读取参数：
  - `--addr`：当前节点地址
  - `--svc`：服务名
  - `--group`：缓存组名
  - `--cache-bytes`：缓存容量
  - `--expiration`：默认 TTL
  - `--etcd`：etcd 地址
2. 调用 `kvcache.NewServer(...)`
3. 调用 `kvcache.NewGroup(...)`
4. 调用 `kvcache.NewClientPicker(...)`
5. 调用 `group.RegisterPeers(picker)`
6. 启动 `srv.Start()`
7. 等待退出信号后 `picker.Close()`、`srv.Stop()`

### 调用链

```text
main()
 ├─ NewServer(...)
 ├─ NewGroup(...)
 ├─ NewClientPicker(...)
 ├─ group.RegisterPeers(picker)
 └─ srv.Start()
```

### 执行意义

这个入口把“缓存系统需要的所有组件”串了起来：

- `Server` 负责网络入口
- `Group` 负责业务缓存
- `ClientPicker` 负责分布式路由
- etcd 负责服务发现

---

## 16.2 `cmd/client/main.go`：命令行客户端入口

### 这个文件负责什么

它是用来测试和验证集群读写的 CLI 工具。

### 主要流程

1. 解析参数：
  - `--op`：get/set/delete
  - `--addr`：目标节点地址
  - `--group`：组名
  - `--key`：缓存 key
  - `--value`：set 时使用
  - `--timeout`：请求超时
2. 使用 `grpc.DialContext(...)` 连接目标节点
3. 创建 `pb.NewKVCacheClient(conn)`
4. 根据 `--op` 调用：
  - `cli.Get`
  - `cli.Set`
  - `cli.Delete`

### 调用链

```text
main()
 ├─ grpc.DialContext(...)
 ├─ pb.NewKVCacheClient(conn)
 ├─ cli.Get / cli.Set / cli.Delete
 └─ 打印结果
```

### 执行意义

这是你联调整个集群的最小工具：

- 验证节点是否活着
- 验证跨节点读写是否正常
- 验证 owner 路由是否正确

---

## 16.3 `server.go`：gRPC 服务端与服务注册

### 这个文件负责什么

`server.go` 是 KVCache 的 RPC 服务实现层：

- 它定义了 `Server` 结构体
- 它负责启动 gRPC 服务
- 它负责实现 `Get/Set/Delete`
- 它负责把节点注册到 etcd

### 核心结构体：`Server`

字段含义：

- `addr`：本节点地址
- `svcName`：服务名
- `groups`：缓存组集合
- `grpcServer`：gRPC 服务实例
- `etcdCli`：etcd 客户端
- `stopCh`：停止信号
- `opts`：服务器配置

### 核心函数

#### `NewServer(addr, svcName string, opts ...ServerOption)`

作用：构建一个可用的服务端对象。

步骤：

1. 拷贝默认配置
2. 应用 option
3. 创建 etcd client
4. 创建 gRPC server
5. 注册 KVCache 服务
6. 注册 health 服务

#### `Start()`

作用：启动网络监听并注册到 etcd。

步骤：

1. `net.Listen("tcp", s.addr)`
2. `etcd.Register(...)` 异步注册
3. `s.grpcServer.Serve(lis)` 阻塞运行

#### `Stop()`

作用：优雅停止。

步骤：

1. 关闭 stop 信号
2. `GracefulStop()`
3. 关闭 etcd client

#### `Get(ctx, req)`

作用：处理远端读取请求。

流程：

1. 根据 `req.Group` 找 `Group`
2. 调 `group.Get(ctx, req.Key)`
3. 把 `ByteView` 转成响应返回

#### `Set(ctx, req)`

作用：处理远端写请求。

流程：

1. 找 `Group`
2. 调 `group.Set(ctx, req.Key, req.Value)`
3. 返回写入结果

#### `Delete(ctx, req)`

作用：处理远端删除请求。

流程：

1. 找 `Group`
2. 调 `group.Delete(ctx, req.Key)`
3. 返回删除结果

### 调用链

```text
Start()
 ├─ net.Listen
 ├─ registry.Register
 └─ grpcServer.Serve

Get/Set/Delete
 └─ GetGroup(req.Group)
     └─ group.Get / group.Set / group.Delete
```

---

## 16.4 `group.go`：整个缓存系统的核心业务层

### 这个文件负责什么

`group.go` 是整个项目最核心的业务层，负责把：

- 本地缓存
- 并发控制
- 分布式路由
- 数据源回源

串成一条完整的缓存链路。

### 核心结构体：`Group`

字段含义：

- `name`：组名
- `getter`：回源函数
- `mainCache`：本地缓存
- `peers`：节点选择器
- `loader`：singleflight
- `expiration`：默认过期时间
- `closed`：关闭状态
- `stats`：统计信息

### 关键函数

#### `NewGroup(name, cacheBytes, getter, opts...)`

作用：创建缓存组。

步骤：

1. 创建默认 `CacheOptions`
2. 初始化 `Cache`
3. 初始化 `singleflight.Group`
4. 应用 `GroupOption`
5. 注册到全局 `groups` map

#### `Get(ctx, key)`

作用：读取一个 key。

执行顺序：

1. 检查 `closed`
2. 检查 `key`
3. 调 `mainCache.Get`
4. 命中直接返回
5. miss 后进入 `load(ctx, key)`

#### `load(ctx, key)`

作用：把并发 miss 合并起来。

步骤：

1. 记录开始时间
2. 调 `g.loader.Do(key, func() {...})`
3. 执行 `loadData(ctx, key)`
4. 统计加载耗时和次数
5. 把结果写回 `mainCache`

#### `loadData(ctx, key)`

作用：真正决定去哪里拿数据。

逻辑：

1. 如果启用了 `peers`：
  - `PickPeer(key)` 找 owner
  - owner 是远端：`getFromPeer(ctx, peer, key)`
  - owner 是本机：`getter.Get(ctx, key)`
2. 如果没有 `peers`：直接回源 `getter.Get`

#### `getFromPeer(ctx, peer, key)`

作用：远程拿数据。

流程：

1. 调 `peer.Get(g.name, key)`
2. 转成 `ByteView`
3. 返回给 `loadData`

#### `Set(ctx, key, value)`

作用：写入缓存。

当前语义：

1. 先判断是否来自 peer
2. 本地写入 `mainCache`
3. 如果不是 peer 请求且开启 peers：异步 `syncToPeers("set")`

#### `Delete(ctx, key)`

作用：删除缓存。

当前语义：

1. 本地先删
2. 如果不是 peer 请求且开启 peers：异步 `syncToPeers("delete")`

#### `syncToPeers(ctx, op, key, value)`

作用：把变更同步到 owner 节点。

步骤：

1. `PickPeer(key)`
2. 如果 owner 是自己，直接返回
3. 构造 `from_peer=true`
4. 调 `peer.Set` 或 `peer.Delete`
5. 失败只打日志

#### `Stats()`

作用：输出统计数据。

会返回：

- 命中率
- 加载次数
- peer 命中/失败次数
- 缓存大小

### 调用链

```text
server.go:Get/Set/Delete
 └─ group.Get / group.Set / group.Delete
     ├─ mainCache.Get / mainCache.Add / mainCache.Delete
     ├─ load()
     │   └─ singleflight.Do
     │       └─ loadData()
     │           ├─ peers.PickPeer
     │           ├─ peer.Get
     │           └─ getter.Get
     └─ syncToPeers()
```

---

## 16.5 `cache.go`：本地缓存封装层

### 这个文件负责什么

`cache.go` 是 `group.go` 和 `store/` 之间的桥梁。

它不关心“分布式”，只关心“本机怎么缓存”。

### 核心结构体：`Cache`

典型职责：

- 延迟初始化底层 store
- 统计命中率
- 管理 TTL
- 清理资源

### 核心函数

#### `NewCache(opts)`

作用：创建一个缓存对象。

#### `Get(ctx, key)`

作用：从本地缓存读取。

流程：

1. 调底层 `store.Get`
2. 若命中，统计 hit
3. 若未命中，统计 miss
4. 返回结果

#### `Add / AddWithExpiration`

作用：写入缓存。

#### `Delete`

作用：删除 key。

#### `Clear`

作用：清空缓存。

#### `Close`

作用：关闭底层 store 和后台清理协程。

#### `Stats`

作用：输出命中率等指标。

### 调用链

```text
group.go
 ├─ mainCache.Get
 ├─ mainCache.Add
 ├─ mainCache.AddWithExpiration
 └─ mainCache.Delete
     └─ cache.go 再转到 store/*
```

---

## 16.6 `store/store.go`：存储抽象接口与工厂

### 这个文件负责什么

这里定义了底层缓存存储的统一接口，目的是让上层不用关心具体实现是 LRU 还是 LRU-2。

### 核心内容

#### `Value` 接口

表示一个可以计算长度的缓存值。

#### `Store` 接口

底层存储必须实现：

- `Get`
- `Set`
- `SetWithExpiration`
- `Delete`
- `Clear`
- `Len`
- `Close`

#### `Options` / `NewOptions()`

提供底层默认配置。

#### `NewStore(cacheType, opts)`

根据缓存类型创建对应存储：

- `LRU` -> `newLRUCache(opts)`
- `LRU2` -> `newLRU2Cache(opts)`

### 调用链

```text
cache.go
 └─ NewCache(opts)
     └─ NewStore(cacheType, store.Options)
```

---

## 16.7 `store/lru.go`：标准 LRU 实现

### 这个文件负责什么

实现一个标准的 LRU 缓存：

- 双向链表保存访问顺序
- map 保存 key -> 节点索引
- 容量满时淘汰最旧节点
- 支持 TTL 过期

### 核心结构

#### `lruCache`

典型字段包括：

- `mu`：读写锁
- `items`：key -> 链表节点
- `list`：双向链表
- `expires`：过期时间表
- `usedBytes`：已使用容量
- `maxBytes`：最大容量

### 关键函数

#### `Get(key)`

步骤：

1. 读锁查 key 是否存在
2. 如果不存在，返回 miss
3. 检查是否过期
4. 如果过期，异步删除
5. 如果命中，读取 value
6. 升级到写锁，将该节点移动到最近访问位置
7. 返回 value

#### `Set(key, value)`

本质上调用 `SetWithExpiration(key, value, 0)`。

#### `SetWithExpiration(key, value, expiration)`

步骤：

1. value 为空则删除 key
2. 写锁保护
3. 计算过期时间
4. key 已存在 -> 更新值并刷新位置
5. key 不存在 -> 新建节点插到链表尾
6. 判断是否需要 eviction

#### `Delete(key)`

删除某个 key。

#### `evict()`

当容量不足时触发淘汰。

一般流程：

1. 优先清理过期项
2. 再从链表头淘汰最旧项

#### `cleanupLoop()`

后台定时清理协程：

- 周期性扫描过期 key
- 统一删除

### 调用链

```text
cache.go -> store.Get / store.Set / store.Delete
```

---

## 16.8 `store/lru2.go`：两级 LRU 实现

### 这个文件负责什么

`LRU-2` 的目标不是简单替代 LRU，而是：

> 更好地区分“偶发访问”和“稳定热点”。

### 核心思路

- **一级缓存**：首次访问进入这里
- **二级缓存**：再次访问后提升到这里

这样可以减少偶发访问污染热点区。

### 核心函数

#### `Create(cap)` / 初始化函数

创建双层结构和链表节点空间。

#### `put(key, val, expireAt, onEvicted)`

写入节点。

- key 已存在 -> 更新并移动
- key 不存在 -> 新插入
- 容量满 -> 淘汰尾部节点

#### `get(key)`

读取并刷新节点位置。

#### `del(key)`

删除节点，并把它标记为无效。

#### `walk(walker)`

遍历缓存有效节点，用于清理过期项。

#### `adjust(idx, f, t)`

调整链表指针位置，把节点移动到头部或尾部。

#### `Get(key)`

完整逻辑：

1. 先查一级
2. 一级命中后提升到二级
3. 一级未命中再查二级
4. 命中过期则删除

#### `SetWithExpiration(key, value, expiration)`

写入时只放到一级缓存，等待后续访问再提升。

#### `cleanupLoop()`

后台定时扫描每个 bucket 的过期节点并删除。

### 调用链

```text
cache.go
 └─ NewStore(LRU2)
     └─ newLRU2Cache(opts)
         └─ lru2Store.Get / SetWithExpiration / Delete / cleanupLoop
```

---

## 16.9 `singleflight/singleflight.go`：并发请求合并

### 这个文件负责什么

它解决的是：

> 同一个 key 同时有很多请求时，只让一个请求真正执行加载逻辑。

### 核心结构

#### `call`

表示一个正在执行的请求：

- `wg`：等待组
- `val`：结果
- `err`：错误

#### `Group`

管理 key -> call 的映射。

### 核心函数

#### `Do(key, fn)`

步骤：

1. 创建一个 `call`
2. `LoadOrStore` 判断这个 key 是否已有正在执行的请求
3. 如果没有：当前 goroutine 成为 leader，执行 `fn()`
4. 如果有：当前请求成为 follower，等待 leader 结果
5. leader 完成后：
  - 写入结果
  - `wg.Done()`
  - 从 map 中删除 key
6. follower 被唤醒后直接复用结果

### 调用链

```text
group.go:load
 └─ singleflight.Group.Do(key, fn)
     └─ 只执行一次 g.loadData
```

---

## 16.10 `consistenthash/con_hash.go`：一致性哈希环

### 这个文件负责什么

它负责把 `key` 映射到某个 owner 节点。

### 核心结构

#### `Map`

里面主要有：

- `keys`：排序后的虚拟节点 hash 切片
- `hashMap`：hash -> node 映射
- `nodeReplicas`：真实节点对应多少虚拟节点
- `nodeCounts`：负载统计
- `totalRequests`：总请求数

### 核心函数

#### `Add(nodes...)`

把真实节点加入哈希环。

#### `AddWithReplicas(node, replicas)`

按指定副本数加入节点，用于权重配置。

#### `Remove(node)`

从环中移除一个节点及其虚拟节点。

#### `Get(key)`

根据 key 的 hash：

1. 在 `keys` 里二分查找
2. 找到第一个大于等于 hash 的虚拟节点
3. 返回对应真实节点
4. 如果找不到，回绕到环起点

#### `startBalancer()`

原来用于自动重平衡，但当前默认关闭，避免各节点 ring 视图不一致。

### 调用链

```text
peers.go -> consHash.Get(key)
         -> 得到 owner 地址
```

---

## 16.11 `peers.go`：服务发现、客户端管理、owner 路由

### 这个文件负责什么

它是“分布式视图层”核心。

职责：

- 从 etcd 获取节点列表
- 监听节点增删
- 维护 gRPC client 连接池
- 维护一致性哈希环
- 提供 `PickPeer(key)` 路由能力

### 核心结构体：`ClientPicker`

字段：

- `selfAddr`：本机地址
- `svcName`：服务名
- `consHash`：哈希环
- `clients`：addr -> client
- `weights`：节点权重
- `etcdCli`：etcd 客户端
- `ctx/cancel`：生命周期控制

### 核心函数

#### `NewClientPicker(addr, opts...)`

流程：

1. 初始化上下文
2. 初始化 `consHash`
3. 初始化 etcd client
4. 启动服务发现

#### `startServiceDiscovery()`

流程：

1. `fetchAllServices()` 全量同步一次节点信息
2. `go watchServiceChanges()` 后台持续 watch

#### `fetchAllServices()`

用途：

- 启动时把已存在节点都拉下来
- 避免只靠 watch 时错过历史节点

#### `watchServiceChanges()`

用途：

- 持续监听 etcd 节点变化
- 收到事件后交给 `handleWatchEvents()`

#### `handleWatchEvents(events)`

用途：

- Put：节点上线，调用 `set(addr)`
- Delete：节点下线，调用 `remove(addr)`

#### `set(addr)`

作用：

- 创建新的远端 client
- 成功后把该节点加入哈希环和 client map

#### `remove(addr)`

作用：

- 关闭 client
- 从哈希环移除节点

#### `PickPeer(key)`

作用：

- 根据 key 找 owner
- 如果 owner 是自己：返回 `isSelf=true`
- 如果 owner 是远端：返回对应 `Peer`
- 如果节点还没就绪：返回 `ok=false`

### 调用链

```text
registry/register.go -> etcd 注册
peers.go -> watch /services/{svc}
peers.go -> consHash.Get(key)
peers.go -> client.go 远程调用
```

---

## 16.12 `client.go`：节点间 RPC 客户端

### 这个文件负责什么

它把 gRPC 调用封装成 `Peer` 接口实现。

### 核心结构体：`Client`

字段：

- `addr`：远端节点地址
- `svcName`：服务名
- `etcdCli`：etcd client
- `conn`：gRPC 连接
- `grpcCli`：gRPC 业务客户端

### 核心函数

#### `NewClient(addr, svcName, etcdCli)`

流程：

1. 如果没有传 etcd client，则创建默认 etcd client
2. `grpc.Dial(..., grpc.WithBlock())`
3. `pb.NewKVCacheClient(conn)`
4. 返回 `Client`

#### `Get(group, key)`

远程读取：

1. 创建超时 context
2. 调 `grpcCli.Get(ctx, req)`
3. 失败返回错误
4. 成功返回 `[]byte`

#### `Set(ctx, group, key, value)`

远程写入：

1. 调 `grpcCli.Set(ctx, req)`
2. 返回写结果

#### `Delete(group, key)`

远程删除：

1. 调 `grpcCli.Delete(ctx, req)`
2. 返回删除状态

#### `Close()`

关闭 gRPC 连接。

### 调用链

```text
peers.go -> NewClient(addr)
         -> client.go:Get/Set/Delete
         -> server.go:Get/Set/Delete
```

---

## 16.13 `registry/register.go`：etcd 注册与续约

### 这个文件负责什么

它负责把服务节点注册到 etcd，并通过 lease 保持在线状态。

### 核心流程

#### `Register(svcName, addr, stopCh)`

1. 创建 etcd client
2. 申请 lease
3. 写入服务 key
4. 开 keepalive
5. 等待 stop 信号
6. revoke lease 并退出

### 调用链

```text
server.go:Start
 └─ registry.Register
     └─ etcd lease + put + keepalive
```

---

## 17. 从一次请求开始，完整串联所有模块

### 17.1 读请求全链路

```text
1. client.go 连接某个 server
2. server.go 收到 gRPC Get 请求
3. server.go 调用 group.Get
4. group.go 先查 mainCache
5. 缓存 miss 后进入 singleflight
6. singleflight 保证同 key 只有一个加载逻辑执行
7. group.go.loadData 调用 peers.PickPeer
8. peers.go 通过 consistenthash 找 owner
9. 如果 owner 是远端，client.go 发起远程 Get
10. 如果 owner 是本机，getter.Get 回源
11. 结果回填 cache.go / store
12. 返回给 server.go，再返回给客户端
```

### 17.2 写请求全链路

```text
1. client.go 发起 Set/Delete
2. server.go 收到请求
3. group.go 先写本地 cache
4. 如果不是 from_peer，则异步 syncToPeers
5. peers.go 通过 hash 找 owner
6. client.go 向 owner 发远程 Set/Delete
7. owner 节点的 server.go 收到请求
8. group.go 因为 from_peer，只做本地写，不再继续同步
```

---

## 18. 你可以直接背的总流程描述

> KVCache 的请求链路是：客户端请求先进入 gRPC server，server 转给 group 处理；group 先查本地缓存，miss 后通过 singleflight 合并并发，再通过一致性哈希找到 owner 节点；如果 owner 是远端，就通过 peers 维护的 gRPC client 发起远程调用；如果 owner 是本机，就回源 getter；节点信息则通过 etcd 注册与 watch 维护，底层缓存由 store 提供 LRU/LRU-2 淘汰能力。

---

## 19. 最终学习顺序建议

如果你要逐步读源码，建议按这个顺序：

1. `cmd/server/main.go`
2. `cmd/client/main.go`
3. `server.go`
4. `group.go`
5. `cache.go`
6. `store/store.go`
7. `store/lru.go`
8. `store/lru2.go`
9. `singleflight/singleflight.go`
10. `consistenthash/con_hash.go`
11. `peers.go`
12. `client.go`
13. `registry/register.go`

这样你会先看见“谁在启动”，再看见“谁在处理请求”，最后看见“数据怎么流动”。

---

## 20. 最小心智模型

你只要记住这条线：

```text
请求进来 -> server -> group -> cache -> store
                │
                ├─ miss -> singleflight -> owner 路由 -> peers/client
                │
                └─ 注册发现 -> etcd -> peers 更新 ring
```

这就是整个 KVCache 的执行骨架。