# KVCache

一个基于 Go 实现的分布式缓存系统，支持：

- 本地缓存（LRU / LRU-2）
- TTL 过期与淘汰
- singleflight 并发防击穿
- 一致性哈希路由（owner 路由）
- gRPC 节点通信
- etcd 服务注册与发现

---

## 1. 项目特性

- **缓存核心**：统一 `Cache` 封装，支持命中率统计
- **多策略存储**：`store` 层支持 LRU、LRU-2
- **并发保护**：同 key 并发 miss 请求合并
- **分布式路由**：按 key 选择 owner 节点
- **服务发现**：基于 etcd 动态感知节点上下线
- **远程访问**：节点间通过 gRPC 执行 `Get/Set/Delete`

---

## 2. 目录结构（含功能说明）

```text
.
├── byteview.go                 # 缓存值只读视图封装（避免外部直接改底层字节）
├── utils.go                    # 通用工具函数（如字节拷贝）
├── cache.go                    # 本地缓存统一封装：初始化、读写、TTL、命中统计
├── group.go                    # 核心业务入口：Get/Set/Delete、singleflight、owner 路由
├── peers.go                    # 节点选择与服务发现：维护成员、client 连接、PickPeer
├── client.go                   # 节点间 gRPC 客户端实现（Peer 接口实现）
├── server.go                   # gRPC 服务端实现，承接远端 Get/Set/Delete 请求
├── balancer.go                 # ring 配置发布与动态配置辅助（控制面预留）
├── consistenthash/
│   ├── config.go               # 一致性哈希配置（副本数、哈希函数等）
│   └── con_hash.go             # 哈希环实现：Add/Remove/Get、权重副本
├── store/
│   ├── store.go                # Store 接口定义与工厂
│   ├── lru.go                  # LRU 缓存实现（链表 + map + 过期清理）
│   ├── lru2.go                 # LRU-2 缓存实现（两级缓存 + 分桶）
│   └── lru2_test.go            # LRU-2 相关测试
├── singleflight/
│   └── singleflight.go         # 同 key 并发请求合并
├── registry/
│   └── register.go             # etcd 注册（租约、续约、注销）
├── pb/
│   ├── kvcache.proto           # gRPC 协议定义
│   └── *.pb.go                 # protobuf / gRPC 生成代码
├── cmd/
│   ├── server/
│   │   └── main.go             # 服务启动入口（参数解析、组装 server + picker）
│   └── client/
│       └── main.go             # 命令行客户端入口（get/set/delete）
├── scripts/
│   └── test.ps1                # PowerShell 快速联调脚本（多节点启动与验证）
├── study.md                    # 系统学习文档（架构、流程、设计思想）
├── mainwork.md                 # 简历工作项与代码实现映射说明
├── go.mod / go.sum             # Go 依赖管理
└── README.md                   # 项目说明文档
```

---

## 3. 运行环境

- Go 1.22+
- etcd 3.5+
- Windows / Linux / macOS

---

## 4. 快速开始

### 4.1 启动 etcd（本地）

确保 etcd 可访问 `127.0.0.1:2379`。

### 4.2 启动 3 个缓存节点

分别在 3 个终端执行：

```bash
go run ./cmd/server --addr 127.0.0.1:8003 --svc kv-cache --group test --etcd 127.0.0.1:2379 --expiration 30s
go run ./cmd/server --addr 127.0.0.1:8002 --svc kv-cache --group test --etcd 127.0.0.1:2379 --expiration 30s
go run ./cmd/server --addr 127.0.0.1:8003 --svc kv-cache --group test --etcd 127.0.0.1:2379 --expiration 30s
```

### 4.3 客户端读写验证

```bash
go run ./cmd/client --op set --addr 127.0.0.1:8001 --group test --key k1 --value v1 --timeout 20s
go run ./cmd/client --op set --addr 127.0.0.1:8002 --group test --key k2 --value v2 --timeout 20s
go run ./cmd/client --op set --addr 127.0.0.1:8003 --group test --key k3 --value v3 --timeout 20s
go run ./cmd/client --op get --addr 127.0.0.1:8001 --group test --key k1  --timeout 20s
go run ./cmd/client --op get --addr 127.0.0.1:8002 --group test --key k2  --timeout 20s
go run ./cmd/client --op get --addr 127.0.0.1:8003 --group test --key k3  --timeout 20s
go run ./cmd/client --op delete --addr 127.0.0.1:8003 --group test --key k1 --timeout 20s
go run ./cmd/client --op delete --addr 127.0.0.1:8003 --group test --key k2 --timeout 20s
go run ./cmd/client --op delete --addr 127.0.0.1:8003 --group test --key k3 --timeout 20s

```

### 4.4 脚本快速验证（PowerShell）

```powershell
.\scripts\test.ps1
```

---

## 5. 一致性与路由语义

当前项目采用 **owner 路由语义**：

- `Set/Delete`：必须路由到 key 的 owner 节点，同步执行，失败直接返回
- `Get`：本地命中优先；miss 后按 owner 路由读取

这能避免“写成功但跨节点读不到”的不一致问题。

---

## 6. 负载均衡说明

### 6.1 项目内负载策略

- 支持一致性哈希与权重副本（replicas）
- 默认关闭“每节点本地自动重平衡”，避免多节点 ring 视图不一致

### 6.2 生产建议

入口流量建议交给外部负载均衡（如 Nginx / Envoy / 云 LB），
KVCache 内部负责数据分片路由与缓存逻辑。

---

## 7. 常用参数

### `cmd/server`

- `--addr`：当前节点地址（如 `127.0.0.1:8001`）
- `--svc`：etcd 注册服务名（默认 `kv-cache`）
- `--group`：缓存组名（默认 `test`）
- `--cache-bytes`：缓存容量（字节）
- `--expiration`：默认 TTL（如 `30s`）
- `--etcd`：etcd 地址，支持逗号分隔
- `--weights`：静态权重（`addr=replicas,...`）
- `--dynamic-ring`：是否启用动态 ring 配置监听（预留）
- `--publish-ring`：启动时发布 ring 配置（预留）

### `cmd/client`

- `--op`：`get | set | delete`
- `--addr`：目标节点地址
- `--group`：组名
- `--key`：缓存 key
- `--value`：写入值（set 必填）
- `--timeout`：请求超时（建议 `10s~20s`）

---

## 8. 开发与测试

```bash
go mod tidy
go test ./cmd/...
```

说明：`store` 下部分测试用例可能与当前实现策略不一致，建议先以命令入口与集成链路验证为主。

---

## 9. 文档

### `learn.md`

更偏：

- 全流程执行图
- 调用关系
- 源码阅读路线

### `study.md`

更偏：

- 学习笔记
- 架构图
- 机制解释
- 面试复述素材

---

## 10. License

MIT