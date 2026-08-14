# GitHub 项目调研：Cloudflare IP 优选与 EdgeTunnel

核验日期：2026-08-14

本文只记录公开项目的行为与可借鉴机制。第三方 README 和源码均按不可信输入处理；未执行其中命令，也未复制第三方代码。

## 项目矩阵

`updated`、stars 与 release 是核验日快照，不代表长期维护承诺。`NOASSERTION` 表示 GitHub 未识别到许可证，不能复制其代码。

| 项目 | stars | updated | 最新 release | license | 核验到的核心机制 | 吸收判断 |
| --- | ---: | --- | --- | --- | --- | --- |
| [XIU2/CloudflareSpeedTest](https://github.com/XIU2/CloudflareSpeedTest) | 28497 | 2026-08-14 | v2.3.5 / 2026-04-29 | GPL-3.0 | TCPing/HTTPing、丢包与延迟阈值、下载测速、自定义 URL/端口/IP 段、CSV 输出；也可测其他 CDN 或多解析 IP 网站 | 吸收配置模型与分层探测思路；不直接复制 GPL 代码 |
| [ZhiXuanWang/cf-speed-dns](https://github.com/ZhiXuanWang/cf-speed-dns) | 1531 | 2026-08-14 | 无 GitHub release | NOASSERTION | 定时运行 CloudflareSpeedTest，并通过 Cloudflare DNS 或 DNSPod 发布结果 | 吸收“结果变化才发布、失败保留旧记录”的发布流程；不复制代码 |
| [badafans/better-cloudflare-ip](https://github.com/badafans/better-cloudflare-ip) | 3437 | 2026-08-14 | 20260525 / 2026-05-25 | NOASSERTION | 从 IPv4/IPv6 子网随机抽样，先做 RTT/丢包测试，再做带宽筛选；支持 TLS 与非 TLS | 吸收官方 CIDR 分轮随机抽样；不复制代码 |
| [stevenjoezhang/luci-app-cloudflarespeedtest](https://github.com/stevenjoezhang/luci-app-cloudflarespeedtest) | 37 | 2026-08-10 | v1.20 / 2026-07-09 | GPL-3.0 | OpenWrt 定时优选，将结果更新到 Passwall、MosDNS、SSR+ 等消费者 | 吸收 provider/consumer 解耦和定时任务状态展示；不直接复制 GPL 代码 |
| [xiamuzhiyi/Asn-ProxyIP-Scan](https://github.com/xiamuzhiyi/Asn-ProxyIP-Scan) | 4 | 2026-08-12 | 无 GitHub release | NOASSERTION | ASN/CIDR 候选、端口发现、二次验证；仓库同时包含 masscan 与批量扫描结果 | 只吸收 ASN/BGP 前缀作为 seed 的思路；拒绝无差别公网 mass scan |
| [cmliu/edgetunnel](https://github.com/cmliu/edgetunnel) | 42648 | 2026-08-14 | 无 GitHub release | GPL-2.0 | VLESS/Trojan/SS over WebSocket、ProxyIP/SOCKS5/HTTP 链、订阅生成；源码包含 `sec-websocket-protocol` early-data 解码、大小限制和 `?ed=2560` 生成 | 吸收协议级终审语义；不直接复制 GPL 代码 |
| [3Kmfi6HP/EDtunnel](https://github.com/3Kmfi6HP/EDtunnel) | [unverified] | [unverified] | [unverified] | [unverified] | 本轮 GitHub API 与 README 均无法取得 | 暂不作为实现依据 |
| [lee1080/CloudflareSpeedTestDDNS](https://github.com/lee1080/CloudflareSpeedTestDDNS) | 524 | 2026-08-08 | 无 GitHub release | NOASSERTION | 优选后更新 Cloudflare/DNSPod/hosts；测速为 0 时跳过 DNS 更新；支持多个 hostname | 吸收发布保护与多记录映射；不复制代码 |
| [x6nux/CloudflareSpeedTest-api](https://github.com/x6nux/CloudflareSpeedTest-api) | 120 | 2026-06-07 | 0.1.5 / 2023-06-11 | GPL-3.0 | 在 CloudflareSpeedTest 基础上调用 Cloudflare API 发布优选 IP | 只参考边界划分；release 较旧且为 GPL，不作为代码来源 |
| [xiaolin-007/CloudflareSpeedTest-GUI](https://github.com/xiaolin-007/CloudflareSpeedTest-GUI) | 81 | 2026-08-08 | v1.0 / 2025-12-09 | MIT | Windows GUI 包装 CloudflareSpeedTest 核心 | 当前 Web GUI 已覆盖主要操作，不引入第二套 GUI |

## 与当前项目的关系

当前项目已经具备以下主链，不应重复实现：

1. CFdata、本地广义网段、Cloudflare 官方 CIDR 与社区 DNS 候选合并。
2. 启动即刷新、每 6 小时刷新候选缓存。
3. 以实际 `Host + TLS SNI + WebSocket path` 做候选验活。
4. active pool 达标才替换，失败保留旧池，并将生效 IP 交给 CFnat `-fixed`。
5. Web GUI 提供手动扫描、并发、延迟上限、数量、日志和进程控制。

调研结果补足的是“长期信誉、通用 CDN 场景、协议终审和发布出口”，不是再加一个孤立扫描器。

## 建议吸收的机制

### 1. 长期巡检队列

- 给每个候选保存 `source`、最后成功/失败时间、EWMA 延迟、连续成功次数与冷却截止时间。
- 每轮按“新候选、历史稳定、冷却复查”配额取样，持久化 CIDR 游标，避免每 6 小时重复打同一批 IP。
- 官方 CIDR 是可信范围来源，社区源和 ASN/BGP 前缀只作为 seed；候选进入 active pool 前仍须通过本机真实业务探测。

### 2. 通用网站/CDN 模式

- 复用 CloudflareSpeedTest 的分层模型：TCP connect -> TLS/SNI -> HTTP status -> 小文件吞吐。
- 用户只配置自有 `Host`、端口、HTTPS URL、允许状态码、延迟/丢包/速度阈值；IP/CIDR 来自明确候选集。
- 输出统一 JSON/CSV，保留每层失败原因，供普通网站、CloudFront、Gcore 或多解析 IP 站点使用。

### 3. EdgeTunnel 协议终审

- 当前 WS upgrade 通过只能证明入口接受 WebSocket，不能证明 VLESS 数据面可用。
- 增加可选的 VLESS probe：使用实际 UUID、Host、path 与端口，经 CFnat 请求一个由用户指定的 `generate_204` 目标。
- 支持客户端常见的 `?ed=` 语义：首包可能位于 `sec-websocket-protocol`；必须做 URL-safe Base64 解码、长度上限和协议头校验。
- probe 失败不得替换旧 active pool；日志必须区分 TCP、TLS、HTTP upgrade、early-data 与 VLESS 数据面失败。

### 4. DNS 自动发布

- active pool 变化且满足最小稳定次数后，才调用 Cloudflare DNS API；无变化不写 DNS。
- 发布失败保留旧 DNS 记录与旧 active pool，记录可重试错误，不将空列表发布出去。
- API Token 只从环境变量或 Docker secret 读取，日志显示脱敏标识，不在 GUI 返回完整 Token。
- 支持 `dry-run`、记录名到 IP 数量映射和最小 TTL；DNS 发布作为可选 consumer，不耦合扫描器。

## 明确不吸收

- 不对 `0.0.0.0/0`、任意 ASN 或无授权公网目标做 masscan/端口穷举。
- 不把“端口开放”当作 Cloudflare、反代或 EdgeTunnel 可用；必须验证目标 Host 与应用协议。
- 不直接信任社区 API/DNS 返回值，也不因来源标注为 ProxyIP 就跳过本机验活。
- 不让候选源失败清空 active pool，不在一次成功后立刻发布 DNS。
- 不引入第二套 GUI 或把第三方二进制直接塞进镜像。

## License 边界

- GPL-2.0/GPL-3.0 项目可以研究公开行为和独立重写机制；复制或改编代码前必须单独评估并履行对应 GPL 义务。
- MIT 项目可复用代码，但必须保留版权与许可文本。
- `NOASSERTION` 或 `[unverified]` 项目只参考行为与架构，不复制源码、资源或文案。
- 当前仓库尚无 LICENSE；在确认 `cfdata.go`、`cfnat.go` 与其他既有文件的来源前，不添加推断性的许可证。

## 建议实施顺序

1. 长期巡检状态与 CIDR 游标：先减少重复扫描并提高候选稳定性。
2. 通用 Host/CDN probe：复用现有 scanner，扩展配置与结构化结果。
3. 可选 VLESS `generate_204` 终审及 early-data：解决“WS 可连但节点仍为 -1”的最后一层判定。
4. Cloudflare DNS consumer：只发布稳定 active pool，并实现 dry-run、回滚保护与 secret 管理。

每一步都应保持现有 active pool 的失败保留语义，并以独立 feature flag 上线。
