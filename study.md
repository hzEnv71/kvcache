# KVCache 学习文档（从 0 到 1）

> 目标：带你从“为什么做”到“怎么跑”，再到“每个模块如何协作”，系统掌握这个项目。

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
- `Set/Delete`：严格路由到 owner（同步执行，失败直接返回）
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

## 6.2 `Set` 流程（已升级为 P0）

1. `Group.Set`
2. 如果是 peer 转发请求（`from_peer`）-> 直接本地写
3. 否则必须 `PickPeer(key)` 找 owner：
   - owner 是远端：同步 `peer.Set`，失败直接返回
   - owner 是本机：本地写

说明：这是“同步 owner 写”语义，避免“写成功但跨节点读不到”的假成功。

---

## 6.3 `Delete` 流程（同 P0）

与 `Set` 对齐：

1. peer 转发请求：本地删
2. 普通请求：必须路由 owner
3. owner 远端则同步删，失败直接返回

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
6. server 侧误标记 `from_peer` 导致绕过 owner 路由
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

## 14. 一份最小可运行心智模型（背下来）

> 一个 key 进来：
>
> 先查本地缓存；
> 本地 miss 时并发合并；
> 然后按一致性哈希找 owner；
> owner 远端则走 peer，owner 本机则回源 getter；
> 拿到值写回本地；
> 写操作必须同步路由到 owner，失败直接返回。

你把这段讲清楚，基本就掌握这个项目主干了。

---

## 15. 附：常用命令（你当前改造后的）

启动节点：

```bash
go run ./cmd/server --addr 127.0.0.1:8001 --svc kv-cache --group test --etcd 127.0.0.1:2379
```

写入：

```bash
go run ./cmd/client --op set --addr 127.0.0.1:8001 --group test --key k1 --value v1
```

读取：

```bash
go run ./cmd/client --op get --addr 127.0.0.1:8002 --group test --key k1
```

删除：

```bash
go run ./cmd/client --op delete --addr 127.0.0.1:8003 --group test --key k1
```

---

如果你愿意，下一步我可以继续给你补两份学习材料：

1. `study-sequence.md`：逐文件精读清单（每个函数要看什么）
2. `interview-qa.md`：基于本项目的高频面试题与标准答案
