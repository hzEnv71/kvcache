# KVCache 学习文档（与 `learn.md` 对齐版）

> 目标：把你当前这个 KVCache 项目，从“整体架构 -> 核心链路 -> 底层机制 -> 常见坑 -> 面试表达”完整串起来。
>
> 本文和 `learn.md` 保持一致，但写得更像“学习笔记 + 图解版源码导读”，方便你边看代码边理解。

---

## 1. 项目一句话定位

`KVCache` 是一个用 Go 实现的**分布式缓存系统**，具备：

- 本地缓存（LRU / LRU-2）
- TTL 过期与后台清理
- `singleflight` 防击穿
- 一致性哈希做 owner 路由
- gRPC 做节点间通信
- etcd 做服务注册与发现

### 你可以这样理解它

```text
      请求
        │
        ▼
   gRPC server
        │
        ▼
     Group 层
   /    │     \
  /     │      \
缓存   owner路由  回源
  │       │       │
  ▼       ▼       ▼
 Store   peers   getter
```

> 核心目标不是“把缓存做大”，而是把 **路由、节点发现、缓存淘汰、并发控制** 这几件事做清楚。

---

## 2. 为什么要这么设计

缓存系统通常要同时满足 4 个目标：

1. **快**：尽量在内存完成读写
2. **稳**：高并发下别击穿、别雪崩
3. **省**：内存有限，要会淘汰
4. **能扩展**：节点增加/下线时，路由还能自动收敛

### 对应到你的项目

```text
快   -> 本地缓存 + O(1) map
稳   -> singleflight
省   -> LRU / LRU-2 + TTL
扩展 -> 一致性哈希 + etcd 服务发现 + gRPC 节点互访
```

---

## 3. 架构全景图（图文并茂）

### 3.1 逻辑分层图

```text
┌────────────────────────────────────────────────────────┐
│                    用户 / client                       │
└───────────────────────────┬────────────────────────────┘
                            │
                            ▼
                    ┌───────────────────┐
                    │   cmd/client      │  命令行测试入口
                    └─────────┬─────────┘
                              │ gRPC
                              ▼
                    ┌───────────────────┐
                    │    server.go      │  RPC 服务端
                    └─────────┬─────────┘
                              │
                              ▼
                    ┌───────────────────┐
                    │    group.go       │  缓存核心入口
                    │  Get/Set/Delete   │
                    └───────┬─────┬─────┘
                            │     │
                            │     │
                            ▼     ▼
                   ┌────────────┐  ┌──────────────────────┐
                   │ cache.go   │  │      peers.go        │
                   │ 本地缓存壳层 │  │ 节点发现 + 路由 + RPC │
                   └─────┬──────┘  └─────────┬────────────┘
                         │                  │
                         ▼                  ▼
               ┌────────────────┐   ┌──────────────────────┐
               │   store/       │   │ consistenthash/      │
               │ LRU / LRU-2    │   │ 一致性哈希环         │
               └────────────────┘   └─────────┬────────────┘
                                              ▼
                                       ┌──────────────┐
                                       │  client.go   │
                                       │ 节点间 RPC   │
                                       └──────────────┘
```

### 3.2 控制面 / 数据面

```text
控制面：etcd + registry + peers watcher
数据面：Group + Cache + Store + gRPC
```

- **控制面**：负责“谁在线、谁下线、拓扑怎么变”
- **数据面**：负责“key/value 真正读写和缓存”

---

## 4. 目录导读（建议阅读顺序）

1. `byteview.go`
  - 先理解缓存值为什么要封装成只读视图
2. `cache.go`
  - 看本地缓存壳层如何调用 store
3. `store/store.go` + `store/lru.go`
  - 理解标准 LRU
4. `singleflight/singleflight.go`
  - 理解并发合并
5. `consistenthash/con_hash.go`
  - 理解 key 如何路由到 owner
6. `peers.go`
  - 理解节点发现、ring、client 管理
7. `group.go`
  - 核心业务入口
8. `client.go` / `server.go` / `registry/register.go`
  - 理解分布式怎么落地

---

## 5. 核心对象和职责

### 5.1 Group

`Group` 就是“缓存命名空间”。

它负责：

- `Get/Set/Delete`
- `load/loadData`
- `Stats`
- `singleflight`
- peer 路由

### 5.2 Cache

`Cache` 是本地缓存的壳层，职责是：

- 包装底层 store
- 统一命中率统计
- TTL / 容量控制
- 生命周期管理

### 5.3 Store

`store` 层提供可替换的缓存引擎：

- `LRU`
- `LRU-2`

### 5.4 PeerPicker / Peer

```text
PeerPicker -> 负责找 owner
Peer      -> 负责对选中的 owner 节点发 RPC
```

当前实现：

- `ClientPicker`：服务发现 + 一致性哈希 + 连接管理
- `Client`：gRPC 远程调用

---

## 6. 一句话讲清楚当前系统语义

> 当前版本是 strict owner-only：所有 `Get/Set/Delete` 都先找 owner，非 owner 只负责转发；owner 节点收到转发请求后，通过 gRPC metadata 识别 `from_peer=true`，只做本地处理，不再继续路由。

---

## 7. 读请求全流程（Get）

### 7.1 链路图

```text
cmd/client
   │
   ▼
server.go:Get
   │
   ▼
group.go:Get
   │
   ├─ peers == nil
   │    └─ 单机模式：先查本地缓存，miss 后 load
   │
   └─ peers != nil
        ├─ PickPeer(key)
        ├─ owner 是远端 -> peer.Get(group, key)
        └─ owner 是本机
             ├─ 先查 mainCache.Get
             └─ miss 后 load(ctx, key)
```

### 7.2 Get 详细步骤

1. 检查 group 是否关闭
2. 检查 key 是否为空
3. 如果没启用 peers：
  - 先查本地缓存
  - miss 后走 `load`
4. 如果启用了 peers：
  - `PickPeer(key)` 找 owner
  - owner 是远端：直接远程读
  - owner 是本机：先查本地缓存
  - 本机 miss 后走 `load`
5. `load` 内部使用 `singleflight` 合并并发 miss
6. `loadData` 只走本地 `getter.Get(ctx, key)` 回源
7. 拿到结果后写回本地缓存

### 7.3 为什么这样设计

- 非 owner 不持久保存业务副本
- owner 节点负责真正的数据语义
- owner miss 后只回源，不再继续 peer 路由，避免环路

---

## 8. 写请求全流程（Set）

### 8.1 链路图

```text
cmd/client
   │
   ▼
server.go:Set
   │
   ▼
group.go:Set
   │
   ├─ from_peer=true
   │    └─ owner 收到转发请求，本地直接写
   │
   ├─ peers == nil
   │    └─ 单机模式，本地直接写
   │
   └─ peers != nil
        ├─ PickPeer(key)
        ├─ owner 是远端 -> gRPC 转发到 owner
        └─ owner 是本机 -> 本地写缓存
```

### 8.2 Set 的详细语义

- 如果请求已经带 `from_peer=true`：说明是 owner 收到的转发请求，只做本地写
- 如果没有 peers：单机模式直接写本地
- 如果启用了 peers：
  - 先找 owner
  - owner 是远端：转发给 owner，并把 `from_peer=true` 放到 metadata
  - owner 是本机：直接写本地

### 8.3 为什么要这么做

- 让写语义收敛成“只有 owner 真正持有数据”
- 避免非 owner 节点留下副本
- 删除时也不会有多份残留

---

## 9. 删除请求全流程（Delete）

### 9.1 链路图

```text
cmd/client
   │
   ▼
server.go:Delete
   │
   ▼
group.go:Delete
   │
   ├─ from_peer=true
   │    └─ owner 收到转发删除请求，本地直接删
   │
   ├─ peers == nil
   │    └─ 单机模式，本地直接删
   │
   └─ peers != nil
        ├─ PickPeer(key)
        ├─ owner 是远端 -> gRPC 转发到 owner
        └─ owner 是本机 -> 本地删缓存
```

### 9.2 为什么 Delete 也要 owner-only

因为如果之前是“本地先写再同步”的模型，删除会很复杂：

- 要删本地
- 要删 owner
- 还要考虑历史副本残留

改成 owner-only 之后：

- 只有 owner 真正保存数据
- 删除只需要删 owner
- 语义更简单

---

## 10. 节点发现与路由更新

### 10.1 注册流程图

```text
server.go.Start()
   │
   └─ registry.Register(svcName, addr, stopCh)
        │
        ├─ etcd Grant lease
        ├─ Put /services/{svc}/{addr}
        ├─ KeepAlive
        └─ 正常退出时 Revoke lease
```

### 10.2 etcd 里的服务目录

```text
/services/kv-cache/127.0.0.1:8001
/services/kv-cache/127.0.0.1:8002
/services/kv-cache/127.0.0.1:8003
```

### 10.3 节点新增为什么感知快

因为新增是 `Put` 事件，watch 一收到就能加进 ring。

### 10.4 节点宕机为什么感知慢

因为宕机时节点不能主动 revoke，只能依赖 lease TTL 到期后 etcd 自动删除 key，才会触发 `Delete` 事件。

### 10.5 你踩过的关键坑

你之前遇到的 bug 是：

- Delete 事件里读了 `event.Kv.Value`
- 但真正被删掉的旧值在 `event.PrevKv.Value`

所以需要：

- watch 时加 `WithPrevKV()`
- Delete 时读 `event.PrevKv.Value`

这样下线节点才会被正确移出 ring。

---

## 11. 一致性哈希的底层机制

### 11.1 工作流程图

```text
key
 │
 ▼
hash(key)
 │
 ▼
sort.Search(keys, hash)
 │
 ├─ 找到第一个 >= hash 的虚拟节点
 ├─ 越界则回绕到 0
 └─ hashMap[virtualHash] -> real node
```

### 11.2 为什么要虚拟节点

- 真实节点太少时，分布会不均匀
- 虚拟节点能让 key 更均匀地分布到环上
- 后续还能通过虚拟节点数量实现权重

### 11.3 为什么 `keys` 必须有序

因为 `Get(key)` 用的是二分查找。

`Add()` 或 `rebalance()` 结束后都必须 `sort.Ints(m.keys)`。

---

## 12. gRPC metadata：为什么必须用它

### 12.1 为什么不能用 `context.WithValue`

因为 `context.WithValue` 不会自动跨网络传输。

### 12.2 正确做法

```text
客户端侧 -> outgoing metadata: from_peer=true
服务端侧 -> incoming metadata: 读取 from_peer=true
```

### 12.3 它解决什么问题

它能让 owner 节点知道：

> 这个请求已经经过一次路由，不要再继续转发了。

如果不用 metadata，远端会把转发请求误判成普通请求，导致重复路由、超时或错误。

---

## 13. singleflight：防击穿的底层机制

### 13.1 它的作用

同一个 key 在短时间内有很多请求时，只允许一次真正回源，其余请求等待结果。

### 13.2 流程图

```text
多个 goroutine 同时 miss 同一个 key
                │
                ▼
         singleflight.Do(key)
                │
        ┌───────┴────────┐
        │                │
   只有一个真正执行      其他等待结果
```

### 13.3 它解决不了什么

- 不能解决跨节点重复回源
- 不能替代副本
- 不能替代锁

它只解决**同一节点、同一 key、并发 miss** 这个问题。

---

## 14. TTL 与过期清理

### 14.1 目前支持什么

你当前项目支持：

- Group 默认 TTL
- 惰性过期
- 后台定时清理
- 命中率统计

### 14.2 TTL 怎么配置

`Group` 级别通过 `WithExpiration(...)` 配置：

```text
NewGroup(..., WithExpiration(30s))
```

### 14.3 过期是怎么生效的

```text
写入时：带 expiration
读取时：如果发现过期，视为 miss
后台清理：周期性扫描并清理过期项
```

### 14.4 为什么同一个 group 不建议不同节点 TTL 不一样

因为 TTL 是 Group 级语义，不是节点私有语义。

如果不同节点上同名 group 的 expiration 不一致，那么 owner 在哪台机器上，TTL 行为就可能不同，导致语义混乱。

---

## 15. LRU / LRU-2

### 15.1 LRU 是什么

- 最近最少使用淘汰
- 结构通常是：map + 双向链表
- 特点是实现简单

### 15.2 LRU-2 是什么

你这里的 LRU-2 更像“两级缓存”模型：

- 第一次访问：先进入一级
- 再次命中：提升到二级热点区
- 分桶 + 局部锁降低竞争

### 15.3 为什么要有两种策略

因为不同场景下：

- LRU 简单好用
- LRU-2 更擅长区分偶发流量和稳定热点

---

## 16. 统计信息 Stats

### 16.1 Group 里统计了什么

你现在 `Group.Stats()` 会返回：

- `loads`
- `local_hits`
- `local_misses`
- `peer_hits`
- `peer_misses`
- `loader_hits`
- `loader_errors`
- `hit_rate`
- `avg_load_time_ms`
- 以及 `cache_...` 前缀的 cache 统计

### 16.2 这些统计有什么用

- 看本地命中率
- 看 peer 命中率
- 看回源成本
- 看是否频繁 miss
- 看 TTL / 淘汰是否合理

### 16.3 你现在如何导出

你已经在 `cmd/server/main.go` 里加了退出前写 `status.log` 的逻辑，现在会按节点写：

```text
log/status-127.0.0.1-8001.log
log/status-127.0.0.1-8002.log
log/status-127.0.0.1-8003.log
```

并且会把 `expiration` 写成字符串，比如：

```json
"expiration": "30s"
```

---

## 17. 你踩过的关键坑（这部分面试非常加分）

### 坑 1：owner 在不同节点算出来不一样

原因：

- 一开始把 self 排除在 ring 外
- 每个节点看到的成员集不一致

修复：

- 所有节点都进 `consHash`
- 只有远端才进 `clients`

### 坑 2：peer 标记传不过去

原因：

- 用了 `context.WithValue`
- 它不会跨 gRPC 传输

修复：

- 改成 gRPC metadata

### 坑 3：owner 是自己却报 owner unavailable

原因：

- PickPeer 在 self 场景还去 `clients` 里找连接

修复：

- self 直接返回 `ok=true, self=true`

### 坑 4：节点宕机后仍然路由到死节点

原因：

- watch Delete 事件读了 `event.Kv.Value`
- 实际应该读 `event.PrevKv.Value`

修复：

- watch 加 `WithPrevKV()`
- Delete 读取 `PrevKv.Value`

### 坑 5：owner 节点 miss 后又继续 peer 路由

原因：

- `loadData()` 里还保留“先找 peer”的逻辑

修复：

- strict owner-only 下，`loadData()` 只回源 getter，不再继续找别的 peer

---

## 18. 面试官可能追问的问题

### 18.1 为什么选择 owner-only，不做多副本？

答：

> 因为这个项目定位是缓存，不是强一致数据库。owner-only 可以把路由、写入、删除和故障恢复逻辑收敛得更清楚，先保证系统语义清晰和可解释性。多副本会引入同步复杂度，不适合当前阶段。

### 18.2 节点挂了会不会丢缓存？

答：

> 会。缓存本来就是可丢的。节点挂了后，通过 etcd lease 过期感知并摘除 ring，之后请求会重新路由到新的 owner，再通过 getter 回源恢复。

### 18.3 如果想提高可用性怎么办？

答：

> 可以在 owner-only 基础上增加副本或者主备机制，或者在控制面加更快的健康检查和更快的故障摘除。当前版本先追求语义清晰和可维护性。

---

## 19. 你可以直接背的项目总结

> 这个 KVCache 项目我主要做了三件事：第一，把单机缓存抽象成了分布式缓存架构，拆成 Group、Cache、Store、PeerPicker、server/client 这几层；第二，把读写链路收敛成 strict owner-only，让 Get/Set/Delete 都先找 owner，非 owner 只负责转发，owner 负责真正处理；第三，在服务发现和故障摘除上用 etcd lease + watch 做节点管理，并修复了多个分布式 bug，比如 ring 不一致、peer metadata 传递问题、Delete 事件读错字段等。

---

## 20. 你可以怎么讲这个项目

建议按照这个顺序：

1. **先讲整体架构**
2. **再讲 Get/Set/Delete 主流程**
3. **再讲一致性哈希和 etcd 服务发现**
4. **最后讲你排查过的坑和 trade-off**

这是最像“做过系统”的讲法。

---

## 21. 最后给你一个心智模型

```text
请求进来 -> server -> group -> cache/store
                   │
                   ├─ miss -> singleflight -> owner 路由 -> peers/client
                   │
                   └─ 节点变化 -> etcd -> watch -> ring 更新
```

记住这条线，你基本就能把整个项目讲明白。