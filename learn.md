# KVCache 全流程执行图与调用关系说明

> 目标：帮助你从“启动程序”一直理解到“请求到达、路由、缓存命中、远端访问、服务发现、写入同步”的完整链路。
>
> 下面按“模块关系图 -> 启动流程 -> 读流程 -> 写流程 -> 服务发现 -> 关键函数调用关系”来讲。

---

## 1. 一句话总览

KVCache 的整体结构可以概括为：

- `cmd/server` 启动一个缓存节点；
- `server.go` 启动 gRPC 服务并注册自己到 etcd；
- `peers.go` 从 etcd 发现其他节点，并维护一致性哈希环和 gRPC client；
- `group.go` 作为缓存核心入口，负责 `Get/Set/Delete`；
- `cache.go` 负责本地缓存读写；
- `store/` 负责底层 LRU / LRU-2 存储；
- `singleflight/` 负责并发合并；
- `client.go` 负责节点间远程调用；
- `consistenthash/` 负责 key 到 owner 节点的路由。

你可以把它理解为：

> **读写请求先进入 `Group`，`Group` 决定查本地还是去远端，`PeerPicker` 决定 owner，`Cache/Store` 负责本地存储，`server/client` 负责节点间 RPC。**

---

## 2. 模块之间的关系图

```text
┌──────────────────────────────────────────────────────────────────────┐
│                        用户 / 业务调用方                              │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                                ▼
                    ┌────────────────────────┐
                    │      cmd/client        │  命令行测试入口
                    └───────────┬────────────┘
                                │ gRPC 请求
                                ▼
                    ┌────────────────────────┐
                    │      server.go         │  gRPC 服务端
                    └───────────┬────────────┘
                                │
                                ▼
                    ┌────────────────────────┐
                    │       group.go         │  缓存核心入口
                    │  Get / Set / Delete    │
                    └───────┬────────┬───────┘
                            │        │
                            │        │
                            ▼        ▼
                ┌────────────────┐   ┌───────────────────────────┐
                │   cache.go     │   │        peers.go           │
                │ 本地缓存封装    │   │ 节点发现 + 路由 + client   │
                └───────┬────────┘   └───────────┬───────────────┘
                        │                        │
                        ▼                        ▼
             ┌──────────────────┐      ┌────────────────────────┐
             │    store/        │      │  consistenthash/       │
             │ LRU / LRU-2      │      │ 一致性哈希环            │
             └──────────────────┘      └────────────────────────┘
                                                │
                                                ▼
                                        ┌──────────────────┐
                                        │   client.go      │
                                        │ 节点间 RPC 调用   │
                                        └──────────────────┘

另一路：

server.go / peers.go ──► registry/register.go ──► etcd
```

---

## 3. 启动流程（从程序启动开始）

这一部分最重要，因为它决定了系统是怎么被组装起来的。

### 3.1 启动入口：`cmd/server/main.go`

你执行：

```bash
go run ./cmd/server --addr 127.0.0.1:8001 --svc kv-cache --group test --etcd 127.0.0.1:2379
```

流程大致是：

```text
cmd/server/main.go
    │
    ├─ 解析命令行参数
    │   ├─ addr
    │   ├─ svc
    │   ├─ group
    │   ├─ cache-bytes
    │   ├─ expiration
    │   └─ etcd
    │
    ├─ 创建 Server
    │   └─ kvcache.NewServer(...)
    │
    ├─ 创建 Group
    │   └─ kvcache.NewGroup(...)
    │
    ├─ 创建 PeerPicker
    │   └─ kvcache.NewClientPicker(...)
    │
    ├─ Group 注册 PeerPicker
    │   └─ group.RegisterPeers(picker)
    │
    ├─ 启动 gRPC 服务
    │   └─ srv.Start()
    │
    └─ 等待退出信号
```

---

### 3.2 `NewServer(...)` 具体做了什么

`server.go` 里的 `NewServer(addr, svcName, opts...)` 负责把一个节点的基础设施搭起来：

1. **构造默认配置副本**
  - 避免多个 `Server` 实例共享同一个默认配置对象
2. **应用外部选项**
  - 比如 etcd 地址、DialTimeout、TLS 等
3. **创建 etcd client**
  - 为后续服务注册、发现做准备
4. **创建 gRPC server**
  - 注册 KVCache 服务接口
5. **注册健康检查服务**
  - 供外部探活使用

简化调用图：

```text
NewServer
 ├─ clone DefaultServerOptions
 ├─ apply opts
 ├─ clientv3.New(...)            // etcd client
 ├─ grpc.NewServer(...)          // gRPC server
 ├─ pb.RegisterKVCacheServer(...)// 注册 RPC 服务
 └─ healthpb.RegisterHealthServer(...) // 健康检查
```

---

### 3.3 `NewGroup(...)` 具体做了什么

`group.go` 的 `NewGroup` 是整个缓存系统的“核心对象创建器”：

1. 创建底层 `Cache`
2. 创建 `singleflight.Group`
3. 应用 `GroupOption`
4. 注册到全局 `groups` map，供 RPC 层通过组名查找

简化调用图：

```text
NewGroup
 ├─ DefaultCacheOptions()
 ├─ NewCache(cacheOpts)
 ├─ &singleflight.Group{}
 ├─ apply GroupOption(s)
 ├─ groupsMu.Lock()
 ├─ groups[name] = g
 └─ groupsMu.Unlock()
```

---

### 3.4 `NewClientPicker(...)` 具体做了什么

`peers.go` 的 `NewClientPicker` 负责“分布式节点视图”的建立：

1. 创建本节点上下文 `ctx/cancel`
2. 初始化 `consHash`（一致性哈希环）
3. 建立 etcd client
4. 启动服务发现：
  - 全量拉取一次在线节点
  - 再 watch 增量变化

简化调用图：

```text
NewClientPicker
 ├─ context.WithCancel(...)
 ├─ consistenthash.New()
 ├─ clientv3.New(...)              // etcd client
 ├─ startServiceDiscovery()
 │   ├─ fetchAllServices()
 │   └─ go watchServiceChanges()
 └─ return picker
```

---

## 4. 读请求全流程（Get）

这是你最需要掌握的链路。

### 4.1 外部请求进入

客户端执行：

```bash
go run ./cmd/client --op get --addr 127.0.0.1:8002 --group test --key k1
```

请求进入 `server.go` 的 `Get(...)`：

```text
client.go / 外部 client
        │
        ▼
server.go:Get(ctx, req)
        │
        ▼
group := GetGroup(req.Group)
        │
        ▼
group.Get(ctx, req.Key)
```

---

### 4.2 `group.Get(...)` 的核心流程

`group.go` 里 `Get` 的逻辑可以拆成 4 层：

#### 第 1 层：状态与参数校验

- `g.closed` 是否已关闭
- `key` 是否为空

#### 第 2 层：先查本地缓存

- 调 `g.mainCache.Get(ctx, key)`
- 命中则直接返回

#### 第 3 层：miss 后进入 `load`

- 通过 `singleflight` 合并并发 miss
- 避免多个请求同时回源

#### 第 4 层：`loadData`

- 如果启用 peers：
  - `PickPeer(key)` 找 owner
  - owner 是远端：走远端 `peer.Get(...)`
  - owner 是本机：调用本地 `getter.Get(...)`
- 拿到数据后写回本地缓存

---

### 4.3 `Get` 的完整调用链

```text
server.go:Get
   │
   ▼
group.go:Get
   │
   ├─ mainCache.Get(ctx, key)
   │    ├─ cache.go -> store.Get
   │    └─ 命中则直接返回
   │
   └─ load(ctx, key)
        │
        ├─ singleflight.Group.Do(key, fn)
        │     └─ loadData(ctx, key)
        │          ├─ peers.PickPeer(key)
        │          ├─ 若 owner 为远端：peer.Get(group, key)
        │          └─ 若 owner 为本机：getter.Get(ctx, key)
        │
        └─ load 结果写回 mainCache
```

---

### 4.4 为什么要用 `singleflight`

如果某个热点 key 同时来了 1000 个请求，而本地缓存 miss 了：

- 没有 `singleflight`：1000 次都去回源 / 远端请求
- 有 `singleflight`：只有 1 次真正加载，其余等待结果

这就是防止缓存击穿。

---

## 5. 写请求全流程（Set / Delete）

### 5.1 `Set` 的调用链

外部调用：

```text
client -> server.go:Set -> group.go:Set
```

`group.Set` 当前的语义是：

1. 判断是否来自 peer 转发（通过 gRPC metadata 中的 `from_peer=true`）
2. 先按 owner 路由
3. owner 是远端：把 `from_peer=true` 继续通过 metadata 传给远端
4. owner 是本机：直接写本地缓存

所以它是：

> **先路由到 owner，再由 owner 完成写入**

---

### 5.2 `Delete` 的调用链

`Delete` 与 `Set` 类似：

1. 先判断是否为 peer 转发请求（通过 gRPC metadata）
2. 再按 owner 路由到目标节点
3. owner 是远端：把 `from_peer=true` 继续传给远端
4. owner 是本机：直接删本地缓存

---

### 5.3 `syncToPeers(...)` 做了什么

```text
group.go:Set/Delete
   │
   ├─ if metadata 中已有 from_peer=true -> 只做本地处理
   ├─ 否则调用 peers.PickPeer(key)
   ├─ owner 是远端 -> 通过 client.go 发起 gRPC 调用
   └─ client.go 会把 from_peer=true 写入 outgoing metadata
```

这里的关键是 `from_peer=true`：

- 通过 gRPC metadata 在节点间传递
- 避免远端收到后再次同步，形成循环
- peer 请求只做本地处理

---

## 6. 节点发现与路由更新流程

### 6.1 服务注册

`server.go.Start()` 中会异步调用：

```text
registry.Register(s.svcName, s.addr, stopCh)
```

注册逻辑在 `registry/register.go`：

1. 创建 etcd client
2. 给节点申请 lease
3. 在 etcd 写入服务地址
4. 开启 keepalive
5. 节点退出时 revoke lease

目录结构大概是：

```text
/services/kv-cache/127.0.0.1:8001 -> 127.0.0.1:8001
/services/kv-cache/127.0.0.1:8002 -> 127.0.0.1:8002
/services/kv-cache/127.0.0.1:8003 -> 127.0.0.1:8003
```

---

### 6.2 服务发现

`peers.go` 里：

#### `fetchAllServices()`

- 启动时全量拉一次 etcd 上已有节点
- 防止漏掉历史节点

#### `watchServiceChanges()`

- 后台持续 watch `/services/{svc}`
- 监听节点新增 / 删除

#### `handleWatchEvents()`

- Put：新节点上线，创建 client，加入哈希环
- Delete：节点下线，关闭 client，从哈希环移除

---

### 6.3 路由如何决定 owner

核心在 `consistenthash.Map.Get(key)`：

```text
key -> hash -> 在 ring 上找最近的虚拟节点 -> 映射到真实节点地址
```

然后 `peers.PickPeer(key)` 会返回：

- `peer`：远端 client
- `ok`：是否找到了可用 owner
- `self`：owner 是否就是本机

上层据此决定：

- `self=true`：本地处理
- `self=false`：远程调用

---

## 7. 本地缓存层如何工作

`cache.go` 是 `group.go` 下面的第二层。

### 7.1 `Cache` 的职责

- 封装底层 `store.Store`
- 负责命中率统计
- 负责统一 `Add/Get/Delete/Clear/Close`
- 负责 TTL 逻辑与容量控制

### 7.2 `store` 层做什么

`store/store.go` 定义接口，`store/lru.go` 和 `store/lru2.go` 提供具体实现：

- `LRU`：最近最少使用淘汰
- `LRU-2`：两级缓存，区分偶发访问与稳定热点

---

## 8. 详细函数调用关系图

### 8.1 Get 流程

```text
cmd/client
   │
   ▼
server.go:Get
   │
   ▼
group.go:Get
   │
   ├─ mainCache.Get
   │    └─ cache.go
   │         └─ store.Get (LRU / LRU2)
   │
   └─ load
        └─ singleflight.Group.Do
             └─ loadData
                  ├─ peers.PickPeer
                  │    ├─ consistenthash.Map.Get
                  │    └─ client lookup
                  ├─ 如果远端：client.go:Get -> gRPC
                  └─ 如果本机：getter.Get
```

### 8.2 Set 流程

```text
cmd/client
   │
   ▼
server.go:Set
   │
   ▼
group.go:Set
   │
   ├─ 先写本地缓存
   │    └─ cache.go -> store.SetWithExpiration
   │
   └─ if !from_peer && peers != nil
        └─ go syncToPeers
             └─ peers.PickPeer
                  └─ peer.Set -> 远端 server.go:Set -> group.go:Set
```

### 8.3 Delete 流程

```text
cmd/client
   │
   ▼
server.go:Delete
   │
   ▼
group.go:Delete
   │
   ├─ 本地删除
   │    └─ cache.go -> store.Delete
   │
   └─ if !from_peer && peers != nil
        └─ go syncToPeers
             └─ peer.Delete -> 远端 server.go:Delete -> group.go:Delete
```

---

## 9. 每个核心文件的职责一览

### 9.1 `group.go`

核心业务入口：

- `Get`
- `Set`
- `Delete`
- `syncToPeers`
- `load`
- `loadData`

### 9.2 `cache.go`

本地缓存封装：

- 连接 `group` 和 `store`
- 统计命中率
- TTL/容量管理

### 9.3 `store/lru.go`

标准 LRU：

- 双向链表 + map
- 过期清理
- 容量淘汰

### 9.4 `store/lru2.go`

两级缓存：

- 一级：首次访问
- 二级：热点提升区
- 分桶锁

### 9.5 `singleflight/singleflight.go`

并发合并：

- 同 key 只允许一个加载函数执行

### 9.6 `consistenthash/con_hash.go`

一致性哈希环：

- 虚拟节点副本
- key -> owner 映射
- 节点增删

### 9.7 `peers.go`

分布式节点视图：

- 服务发现
- client 管理
- owner 路由

### 9.8 `client.go`

节点间 RPC 客户端：

- `Get/Set/Delete`
- 连接管理

### 9.9 `server.go`

RPC 服务端：

- `Get/Set/Delete`
- 服务启动
- etcd 注册

### 9.10 `registry/register.go`

etcd 注册与续约：

- lease
- keepalive
- revoke

---

## 10. 读写时最容易混淆的几个点

### 10.1 `from_peer` 是什么

它是一个上下文标记，表示：

> 这个请求是另一个节点转发过来的，不要再同步出去，避免环路。

### 10.2 `owner` 是什么

owner 就是“某个 key 应该归属哪个节点”。

由：

```text
consistenthash.Map.Get(key)
```

决定。

### 10.3 为什么要有 `PickPeer`

因为它把“路由判断”和“连接取用”合在一起：

- 路由先找 owner
- 再决定是否远程调用

### 10.4 为什么要先写本地再同步

这是为了让当前节点读路径立即可见，但代价是最终一致而不是强一致。

---

## 11. 你可以用这一段话串起来讲整个项目

> KVCache 的整体链路是：客户端请求先到 gRPC server，server 转给 group 层处理；group 先通过一致性哈希找到 owner 节点；如果 owner 是远端，就用 peers 维护的 gRPC client 发起远程调用，并通过 gRPC metadata 传递 `from_peer=true`；如果 owner 是本机，就在本地缓存中读写；节点信息由 etcd 注册和 watch 维护，底层缓存由 store 提供 LRU/LRU-2 淘汰能力。

---

## 12. 建议你怎么学这份图

按这个顺序最容易掌握：

1. 先看 `cmd/server/main.go` 和 `cmd/client/main.go`
2. 再看 `server.go`
3. 再看 `group.go`
4. 再看 `peers.go`
5. 再看 `consistenthash/con_hash.go`
6. 最后补 `cache.go`、`store/`、`singleflight/`、`registry/`

---

## 13. 最终心智模型

你只要记住这条线：

```text
请求进来 -> server -> group -> cache -> store
                │
                ├─ miss -> singleflight -> owner 路由 -> peers/client
                │
                └─ 注册发现 -> etcd -> peers 更新 ring
```

这就是整个 KVCache 的执行骨架。