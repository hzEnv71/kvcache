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

## 2. 目录结构

```text
.
├── cache.go
├── group.go
├── peers.go
├── server.go
├── client.go
├── consistenthash/
├── store/
├── singleflight/
├── registry/
├── pb/
├── cmd/
│   ├── server/
│   └── client/
└── scripts/
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
go run ./cmd/server --addr 127.0.0.1:8001 --svc kv-cache --group test --etcd 127.0.0.1:2379
go run ./cmd/server --addr 127.0.0.1:8002 --svc kv-cache --group test --etcd 127.0.0.1:2379
go run ./cmd/server --addr 127.0.0.1:8003 --svc kv-cache --group test --etcd 127.0.0.1:2379
```

### 4.3 客户端读写验证

```bash
go run ./cmd/client --op set --addr 127.0.0.1:8001 --group test --key k1 --value v1 --timeout 20s
go run ./cmd/client --op get --addr 127.0.0.1:8002 --group test --key k1 --timeout 20s
go run ./cmd/client --op delete --addr 127.0.0.1:8003 --group test --key k1 --timeout 20s
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

- 学习文档：`study.md`
- 主要工作说明：`mainwork.md`

---

## 10. License

MIT
