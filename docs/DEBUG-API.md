# 调试接口（仅测试构建）

`/api/debug/*` 是一组可以随意改写池、候选和账本的测试工具。它**不在生产二进制里**：相关代码用 Go 构建标签隔离，不加标签编译时整段代码不参与构建，路由是 404。

字段的权威定义以 `web/debugapi_on.go` 为准，本文只给可复制的用法。

## 构建与访问

```bash
# 测试构建（带调试接口）
go build -tags debugapi -o /tmp/cfnat-web-debug ./web

# 生产构建（不带，接口不存在）
go build -o /tmp/cfnat-web ./web
```

默认只接受回环来源（`127.0.0.1` / `::1`），其它来源一律 403。在 NAS 上通过 SSH 转发使用：

```bash
ssh -N -L 8080:127.0.0.1:8080 administer@192.168.1.50
```

隔离测试网络里确实需要远程访问时，才设 `DEBUG_API_ALLOW_REMOTE=1`。

## 常用操作

下面的 `B=http://127.0.0.1:8080` 按实际监听地址替换。

### 看当前状态

```bash
curl -s $B/api/debug/state | jq .
```

返回池、候选、逐源健康、渠道汇总、质量账、在位探测账、两个动作的进度、`poolBlock`、`poolGuard` 和 cfnat 状态。排查「为什么没换池」先看这里。

### 看运行参数

```bash
curl -s $B/api/debug/env | jq .
```

只返回白名单内的候选、漏斗、池大小、探针开关等参数；订阅令牌一类密钥不在白名单内，永远不会返回。

### 直接写池

```bash
# 覆盖并立即应用（会重启 cfnat）
curl -s -X POST $B/api/debug/pool -d '{
  "ips": ["203.0.113.10", "203.0.113.11"],
  "apply": true
}'

# 只写内存与磁盘，不重启 cfnat
curl -s -X POST $B/api/debug/pool -d '{"ips": ["203.0.113.10"], "apply": false}'
```

可带字段：`ips`、`domains`、`priority`、`results`、`lock`、`apply`（缺省 `true`）。这个接口绕过换池闸、冷却、健康检查和片区配额——测试「池子变成这样系统怎么反应」时就靠它。

### 换候选快照

```bash
curl -s -X POST $B/api/debug/candidates -d '{
  "ips": ["203.0.113.10", "203.0.113.11"],
  "sourceByIp": {"203.0.113.10": "github", "203.0.113.11": "sources"},
  "save": true
}'
```

没有显式给来源的 IP 记为 `user`，避免环形图把调试数据误读成某个真实渠道。`save:false` 只改内存。

### 清冷却与拦截记录

```bash
curl -s -X POST $B/api/debug/pool/reset-cooldown
```

清零 `lastAutoSwitchAt`、`lastPoolSwitchAt`、`poolBlock*`，并清掉池上的 `error/errorAt`。手动优选被 30 分钟冷却挡住时用这个立刻解锁。

### 踢人

```bash
curl -s -X POST $B/api/debug/kick -d '{"member": "203.0.113.10"}'
curl -s -X POST $B/api/debug/kick -d '{"member": "203.0.113.10", "force": true}'
```

`force` 允许踢掉最后一个成员（生产路径会拒绝）。空池无法交给 cfnat，所以这一脚只写入内存与磁盘、不重启 cfnat，返回 `applied:false`——它用来模拟空池，不是让 cfnat 真跑空池。成员必须在当前池里。

### 池锁

```bash
curl -s -X POST $B/api/debug/lock -d '{"locked": true}'
```

### 单 IP 探测

```bash
curl -s -X POST $B/api/debug/probe -d '{"ip": "203.0.113.10", "timeoutMs": 6000}'
```

返回 `latencyMs`、`colo`、`ok`；失败时附 `error` 与 `errorClass`。只接受公网 IPv4。

### 触发两个动作

```bash
# 当前测试：验证在位成员，同步等结果
curl -s -X POST "$B/api/debug/revalidate?wait=1" | jq .

# 立即优选：刷新候选 → 漏斗 → 换池，同步等报告
curl -s -X POST "$B/api/debug/optimize?wait=1" | jq .
```

不带 `wait=1` 时立即回 202，进度看 `/api/debug/state` 或 `/api/status`。两个动作互斥：另一个在跑时返回 409（对方动作在跑）或 429（自己已经在跑）。

### 清账本

```bash
curl -s -X POST $B/api/debug/ledger -d '{"what": "sources"}'
```

`what` 可选 `sources` / `throne` / `domain` / `all`。`throne` 会同时清在位健康记录，`domain` 会同时清域名隔离。

## 注意

- 调试接口能改写生产数据结构。即使只是误发了带标签的构建，也要靠回环限制兜底；不要把 `DEBUG_API_ALLOW_REMOTE=1` 带进任何共享网络。
- 改完 `web/debugapi_on.go` 之后至少跑一次 `go test -tags debugapi ./web/...`，同时跑一次不带标签的 `go test ./web/...`，确认生产世界里的路由仍然不存在。
