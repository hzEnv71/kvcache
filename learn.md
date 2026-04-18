# KVCache 学习笔记
> 这份文档是为你当前这个 KVCache 项目重新整理的“实现说明 + 设计说明”。重点只保留**现在代码真实生效的行为**，避免和旧版本语义混在一起。
>
> 当前项目的关键词是：**strict owner-only、gRPC metadata、etcd 服务发现、一致性哈希、singleflight、TTL、LRU/LRU-2**。
---
## 1. 项目一句话概括
KVCache 是一个基于 Go 实现的分布式缓存系统：
- `cmd/server` 启动缓存节点；
- `server.go` 提供 gRPC 服务；
- `etcd/register.go` 负责服务注册、续约、下线；
- `peers.go` 负责服务发现、维护一致性哈希环、管理远端 client；
- `group.go` 是缓存的业务入口，负责 `Get / Set / Delete`；
- `cache.go` 和 `store/` 负责本地缓存与淘汰；
- `singleflight/` 负责并发合并；
- `client.go` 负责节点间 RPC 调用；
- `consistenthash/` 负责 key 到 owner 的映射。
这套系统的核心语义是：
> **所有 key 先通过一致性哈希找 owner，非 owner 只负责转发，owner 负责真正处理。**
---
## 2. 系统分层
可以把系统理解成四层：
### 2.1 接入层
- `cmd/client`
- `server.go`
负责外部请求接入和 gRPC 暴露。
### 2.2 路由协调层
- `group.go`
- `peers.go`
- `consistenthash/`
负责：
- key 属于哪个节点
- 当前节点是 owner 还是非 owner
- 要不要转发到远端 owner
### 2.3 本地缓存层
- `cache.go`
- `store/`
负责：
- 本地读写
- TTL 过期
- 淘汰策略
- 命中率统计
### 2.4 控制面
- `etcd/register.go`
- etcd watch
负责：
- 节点注册
- 节点续约
- 节点下线感知
- 环的成员更新
---
## 3. 当前版本最重要的设计结论
### 3.1 strict owner-only
这是你现在项目最核心的语义：
- `Get`：先找 owner，owner 是远端就去远端读，owner 是本机才查本地缓存
- `Set`：先找 owner，owner 是远端就转发到远端写，owner 是本机才本地写
- `Delete`：先找 owner，owner 是远端就转发到远端删，owner 是本机才本地删
### 3.2 `from_peer` 用 gRPC metadata 传递
你之前踩过一个坑：`context.WithValue` 不会跨 gRPC 传输。
所以现在：
- 发起远端 RPC 时，把 `from_peer=true` 放到 outgoing metadata
- 远端收到后从 incoming metadata 读取
- 识别出这是 peer 转发请求后，只做本地处理，不再继续路由
### 3.3 所有节点都进入 ring，只有远端节点进入 clients
这是你修过的一个关键问题：
- `consHash` 中包含所有节点，包括自己
- `clients` 只保存远端节点的 gRPC client，不保存自己
这样所有节点对同一个 key 计算出来的 owner 才能一致。
---
## 4. 启动流程
### 4.1 `cmd/server/main.go`
启动大致流程：
```text
解析参数
   └─ addr / svc / group / cache-bytes / expiration / etcd
        ↓
创建 Server
        ↓
创建 Group
        ↓
创建 ClientPicker
        ↓
group.RegisterPeers(picker)
        ↓
启动 gRPC server
        ↓
等待退出信号
        ↓
退出前写 status 日志
```
### 4.2 `NewServer(...)`
`server.go` 的 `NewServer` 负责：
1. 克隆默认配置；
2. 应用外部 option；
3. 创建 etcd client；
4. 创建 gRPC server；
5. 注册 KVCache 服务；
6. 注册健康检查服务。
### 4.3 `NewGroup(...)`
`group.go` 的 `NewGroup` 负责：
1. 初始化本地 `Cache`；
2. 初始化 `singleflight.Group`；
3. 应用 `GroupOption`；
4. 注册到全局 `groups` map，供 RPC 层按名字找到。
### 4.4 `NewClientPicker(...)`
`peers.go` 的 `NewClientPicker` 负责：
1. 创建上下文；
2. 初始化一致性哈希环；
3. 创建 etcd client；
4. 启动服务发现：
   - 先全量拉取当前在线节点
   - 再 watch 增量变化
---
## 5. `Get` 的真实执行链路
### 5.1 总流程
```text
server.go:Get
   ↓
group.go:Get
   ↓
判断 group 是否关闭 / key 是否为空
   ↓
如果 peers == nil：退化成单机模式
   ├─ 先查本地缓存
   └─ miss 后走 load(ctx, key)
   ↓
如果启用了 peers：
   ├─ 先 PickPeer(key) 找 owner
   ├─ owner 是远端：直接 peer.Get(group, key)
   └─ owner 是本机：先查本地缓存
        ├─ 命中：直接返回
        └─ miss：走 load(ctx, key)
```
### 5.2 `load(...)`
`load` 的作用是：
- 用 `singleflight` 合并同一个 key 的并发 miss
- 避免热点 key 同时回源导致缓存击穿
### 5.3 `loadData(...)`
你当前的 `loadData` 已经收敛成：
- **只走本地 getter 回源**
- 不再继续去找其他 peer
这点很重要，因为你的设计已经变成 strict owner-only：
> owner 节点 miss 后只回源，不再继续 peer 路由。
### 5.4 `Get` 的理解方式
你可以把 `Get` 理解成三种模式：
1. **单机模式**：
   - 先查本地缓存
   - miss 后回源
2. **分布式模式，owner 是远端**：
   - 直接远端读
3. **分布式模式，owner 是本机**：
   - 先查本地缓存
   - miss 后回源
---
## 6. `Set` 的真实执行链路
### 6.1 总流程
```text
server.go:Set
   ↓
group.go:Set
   ↓
如果 metadata 中有 from_peer=true：
   └─ 说明这是 owner 收到的转发请求，只做本地写
   ↓
如果 peers == nil：
   └─ 单机模式，直接本地写
   ↓
如果启用了 peers：
   ├─ 先 PickPeer(key) 找 owner
   ├─ owner 是远端：通过 gRPC 转发，并写入 from_peer=true metadata
   └─ owner 是本机：直接本地写
```
### 6.2 你当前的写语义
- 非 owner 不保存业务副本
- owner 收到请求后只负责真正落缓存
- 转发请求通过 `from_peer=true` 避免二次路由
### 6.3 为什么不再用“本地先写再同步”
因为之前那种方式会导致：
- 一个 key 在多个节点上短暂或长期存在副本
- 删除语义复杂
- 节点故障后残留副本难处理
现在改成 strict owner-only，语义更简单。
---
## 7. `Delete` 的真实执行链路
### 7.1 总流程
```text
server.go:Delete
   ↓
group.go:Delete
   ↓
如果 metadata 中有 from_peer=true：
   └─ 这是 owner 收到的转发请求，只做本地删
   ↓
如果 peers == nil：
   └─ 单机模式，直接本地删
   ↓
如果启用了 peers：
   ├─ 先 PickPeer(key) 找 owner
   ├─ owner 是远端：通过 gRPC 转发删除，并写入 from_peer=true
   └─ owner 是本机：直接本地删
```
### 7.2 Delete 不会再删多份副本
当前版本里，Delete 已经不是“删自己再同步别人”了，而是：
> 先找 owner，owner 负责唯一删除。
这也是 strict owner-only 的一部分。
---
## 8. `routeToOwner` 的角色
你当前把 `Set/Delete` 里的 owner 判断抽成了一个 helper。
### 它的职责
- 根据 key 找 owner
- 如果 owner 是本机，告诉调用方“你继续做本地操作”
- 如果 owner 是远端，负责把请求转发过去
- 如果转发成功，告诉调用方“已经处理完了，你可以直接返回”
### 它为什么有意义
因为 `Set` 和 `Delete` 都要做同样的路由判断，抽出来以后：
- 主流程更短
- 代码重复更少
- 以后修改路由逻辑只改一处
---
## 9. `Peers` / `ClientPicker` 的真实职责
### 9.1 `consHash`
一致性哈希负责：
- 真实节点 -> 虚拟节点
- key -> owner 节点
它的前提是：
> 所有节点看到的成员视图必须一致。
### 9.2 `clients`
`clients` 负责：
- 保存远端 gRPC client
- 只保存远端，不保存自己
### 9.3 `PickPeer(key)`
你现在的语义是：
- owner 是本机：`ok=true, self=true`
- owner 是远端且 client 存在：`ok=true, self=false`
- owner 不可用：`ok=false`
这个逻辑必须和 `consHash` / `clients` 的设计配套，否则就会报 `owner peer unavailable`。
---
## 10. etcd 服务发现
### 10.1 注册
`etcd/register.go` 做的事：
1. 创建 etcd client；
2. 申请 lease；
3. 把服务地址写到 `/services/{svc}/{addr}`；
4. 用 KeepAlive 持续续约；
5. 正常退出时 revoke lease。
### 10.2 新节点为什么感知快
因为新节点是 `Put` 事件，watch 能立刻收到。
### 10.3 下线节点为什么之前感知慢
之前是因为：
- 宕机依赖 lease TTL 到期
- 但 Delete 事件处理里读错了字段
- 你一开始只读 `event.Kv.Value`
现在应该是：
- watch 使用 `WithPrevKV()`
- Delete 事件读取 `event.PrevKv.Value`
这样才能正确拿到被删节点地址。
### 10.4 节点下线感知链路
```text
节点宕机 / lease 不再续约
   ↓
etcd lease 过期
   ↓
key 被删除
   ↓
watch 收到 Delete 事件
   ↓
读取 event.PrevKv.Value
   ↓
removeMember(addr)
   ↓
delete client / close client
   ↓
ring 更新
```
---
## 11. TTL 和过期
### 11.1 当前支持什么
支持：
- Group 级默认 expiration
- 写入时设置过期时间
- 存储层惰性过期
- 存储层后台定时清理
### 11.2 需要注意什么
同名 Group 在不同节点上的 TTL 配置应该一致，否则会导致语义混乱。
### 11.3 如果想做 key 级 TTL
需要增加：
- `SetWithExpiration(...)`
- RPC 协议携带 expiration
- owner 节点按请求里的 TTL 写入
---
## 12. 命中率统计
`Group` 里已经有 stats：
- `loads`
- `local_hits`
- `local_misses`
- `peer_hits`
- `peer_misses`
- `loader_hits`
- `loader_errors`
- `loadDuration`
并且有：
```go
func (g *Group) Stats() map[string]interface{}
```
### 它的作用
- 统计本地命中率
- 统计 peer 命中情况
- 统计加载耗时
- 统计底层 cache 指标
### 你现在的调试方式
你已经把退出时的 `Stats()` 写到节点自己的日志文件了，便于离线查看。
---
## 13. 本地缓存和 store
### 13.1 `cache.go`
负责：
- 包一层本地缓存读写
- 暴露统一的 `Add/Get/Delete/Clear/Stats`
- 统计命中率
### 13.2 `store/lru.go`
标准 LRU：
- map + 双向链表
- 容量满了淘汰最久未使用项
- 支持 TTL
- 有 cleanup loop
### 13.3 `store/lru2.go`
两级缓存：
- 一层存首次访问
- 二层存热点数据
- 更适合区分偶发访问和稳定热点
---
## 14. 你最近踩过的几个关键坑
这部分特别适合面试时讲“我怎么调过系统”。
### 14.1 owner 在不同节点算不一致
原因：
- self 节点没进 ring
- 每台机器看到的 ring 不一样
修复：
- 所有节点都进 `consHash`
- 只有远端节点进 `clients`
### 14.2 `from_peer` 不生效
原因：
- 一开始用 `context.WithValue`
- 它不会跨 gRPC 传输
修复：
- 改成 gRPC metadata
### 14.3 owner 是自己却报 `owner peer unavailable`
原因：
- `PickPeer` 把 self 也当成要查 `clients`
- 但 `clients` 本来就不存自己
修复：
- self 时直接 `ok=true, self=true`
### 14.4 节点宕机后仍然路由到死节点
原因：
- watch Delete 事件读错字段
- 没用 `WithPrevKV()`
修复：
- Delete 时读 `event.PrevKv.Value`
### 14.5 owner 节点 miss 后还继续找 peer
原因：
- `loadData()` 里保留了“先找 peer”的逻辑
修复：
- `loadData()` 只走本地 getter 回源
---
## 15. 你可以怎么给面试官讲这个项目
### 15.1 一句话版本
> 我做的是一个基于 Go 的分布式缓存系统，使用 etcd 做服务发现和节点状态管理，用一致性哈希做 owner 路由，用 strict owner-only 统一 Get/Set/Delete 语义，底层有 singleflight 防击穿、TTL 过期和 LRU/LRU-2 淘汰。
### 15.2 30 秒版本
> 这个项目可以分成控制面和数据面。控制面由 etcd 负责节点注册、续约和下线感知；数据面由 group、cache、store、peer client 和一致性哈希组成。请求进来后先找 owner，非 owner 只转发，owner 才真正处理。这样路由、过期、并发控制、节点发现都串起来了。
### 15.3 2 分钟版本
你可以按这个顺序讲：
1. 项目目标是什么
2. 为什么要用 etcd
3. 为什么要用一致性哈希
4. Get/Set/Delete 怎么走
5. 为什么改成 strict owner-only
6. 你踩过哪些坑，怎么修的
---
## 16. 面试官可能追问的“底层机制”
### 16.1 etcd lease 是怎么工作的？
- 节点申请 lease
- Put key 绑定 lease
- KeepAlive 持续续约
- lease 过期后 key 自动删除
- watcher 收到 Delete 事件
### 16.2 一致性哈希为什么能减少迁移？
- 节点变化只影响环上一段区间
- 不会像取模那样全量重映射
### 16.3 singleflight 怎么防击穿？
- 同一 key 的并发请求只让一个真正执行加载函数
- 其他 goroutine 等待结果
### 16.4 gRPC metadata 为什么能传 `from_peer`？
- metadata 是协议的一部分
- 会随 RPC 一起传输
- `context.Value` 不会自动跨进程传递
---
## 17. 这份文档你应该怎么用
建议你按这个顺序读：
1. 先读 1~3 章，理解整体结构
2. 再读 5~10 章，理解 Get/Set/Delete、etcd、ring、metadata
3. 再读 11~14 章，理解 TTL、统计、存储、踩坑
4. 最后背 15~16 章，准备面试表达
---
## 18. 最后给你的核心心智模型
你只要记住这条链路：
```text
请求进来 -> server -> group -> owner 路由 ->
owner 是远端就转发 -> owner 是本机就本地处理 ->
本地 miss 再单飞回源 -> store 做 TTL / LRU / LRU-2
```
以及这条控制面链路：
```text
节点启动 -> 注册到 etcd -> keepalive 续约 ->
watch 收到变更 -> 更新 ring / clients -> 重新路由
```
这就是你当前这个 KVCache 项目的真实执行骨架。