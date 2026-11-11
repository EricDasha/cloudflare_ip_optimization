# cfnat 交接单：NAS 出口故障与三项待改

> 接手者：muse。目标：按本单实施代码改动并发 PR。
> 所有结论均有实测证据，证据文件在 `.agent/evidence/`。
> **已上线版本 v4 的工作区改动不在 git 里，见 `.agent/patches/deployed-v4-working-tree.diff`，PR 必须基于它。**

---

## 0. 当前状态（2026-11-12 02:30 UTC+8）

- NAS `192.168.1.50`，容器 `cloudflare-tools`，二进制版本 `b1e5133+thronefix4`。
- 仓库 `g:/EricDasha.github.io/cloudflare`，分支 `main`，HEAD `b1e5133`。
- **工作区有 527 行未提交改动，这些改动已经构建并跑在线上**（日志轮转、空池修复、重启防抖、域名观察期、慢握手不判死）。PR 第一步就是把它们提交。
- 回滚镜像：`local/cloudflare-tools:rollback-20261111-203354`（`0050d1cabc49`）。
- 备份：`/vol1/1000/Docker/CFNat/_backup/pre-thronefix4-20261112-0120/`。

---

## 1. 核心结论：问题不在池子，在 NAS 出口

**P0｜NAS 出口对境外 IP 存在 TCP payload 黑洞。**

证据链（务必按顺序读，这是整份审计最硬的一条）：

1. 同一批 IP、同一时刻，三个出口对照：
   - Windows `192.168.1.11`：全部 OK —— **但这是假数据**。对 TEST-NET 保留地址 `192.0.2.1:443`、`198.51.100.7:443` 也返回 CONNECT_OK 0–1 ms，说明被 ThroneCore TUN 接管，不能作为证据。
   - NAS 宿主 + 容器 netns：CF 官方段、github、shopify、google、twitter 全部间歇超时，结果完全一致 → 排除 docker 网络。
2. 不是 CF 封锁：`23.227.38.74`(shopify)、`140.82.121.4`(github) 同样超时；fastly `151.101.1.140`、`8.8.8.8`、`223.5.5.5` 始终正常。
3. 不是端口封锁：`104.18.42.163` 五端口（443/80/2053/8443/2087）全 6/6；`162.159.36.1` 只通 80；`104.18.33.74` 几乎全灭 → 按 IP 逐个丢。
4. 不是频率触发：gap 0/1/5/15 s 各 5 次，官方段全档 0–1/5，非官方全档 5/5。
5. **决定性证据**：超时从 3 s 放宽到 10 s 后，
   ```
   104.18.42.163   [107, 106, 105, 103, 109]   ← 全部成功
   172.64.147.104  [103, 106, 101, 112, 107]   ← 全部成功
   ```
   建连不慢，只是 3 s 窗口内拿不到 SYN-ACK。建连成功后立刻发数据：
   ```
   conn 成功 → send(TLS ClientHello) → 6.1 s 无任何回应
   ```
   → **SYN 能过，payload 被黑洞。**
6. ICMP 到 `104.18.42.163` 0% 丢包 110 ms、TCP 443 超时；TCP traceroute 第 5 跳后全 `*`。
7. 宿主 `enp1s0`：`rx_dropped 25,181,000 / rx_packets 399,672,850` = **6.3%**，全集中 CPU0 softnet；链路 100 Mbps。

**这一条代码改不了。** 需要在基础设施层处理（查运营商路径、换出口、加隧道、查 QoS）。交给 muse 的代码改动只是为了在出口坏掉时不把系统带崩。

---

## 2. 待实施的代码改动（三项，按优先级）

### 改动 A｜域名体检「通路不可用」与「域名真死」必须区分
**文件**：`web/domain_latency.go`，函数 `recordDomainProbeResults`（约 :490）
**现状**：整轮零测速样本时 `probeHealthy=false` → 连败不累计、不入样本、`recalculate()` 不跑 → 新记录永远停在 `state=""`。日志：
```
domain probe: no domain produced a speed sample this round (30 domains), failures not counted toward dead
```
**后果**：闸门 `domainUsableForForwarding` 对 `state=""` 一律放行，于是 29 个未验证域名（含 4 个已知 403 的 `cf-cname.xingpingcn.top`、`cf.xreak.top`、`cmcc.vvvv.ee`、`www.oopt.eu.cc`）在 17:52:58 被全量塞回池。

**要求**：
- 统计本轮 DNS 解析失败数 / TCP 建连失败数 / 测速失败数，三者分开。
- 当「解析失败 + 建连失败」占比超过阈值（建议 ≥70%）且成功数为 0 时，判定为**体检通路故障**，本轮：
  - 不把域名回池（保持 `current.Domains` 不变）
  - 不累计连败、不判死
  - 打明确日志，例如 `domain probe path broken: %d/%d unresolvable, pool unchanged`
- 只有「解析成功 + 建连成功 + 测速失败」才算域名本身有问题。
- 阈值与判定逻辑做成可测的纯函数，补单测。

### 改动 B｜收紧 DEGRADED 降级，VLESS 超时的 IP 不得靠 WS 快速通过进池
**文件**：`web/funnel.go`，`autoApplyProxyPool` 中 `len(passed) < cfg.MinPool` 附近（约 :426），与 `DEGRADED` 日志对应
**现状**：日志 `proxy pool DEGRADED: VLESS 终审仅 1 个通过，低于最小池 3；已降级采用 2 个 WS 已通过但 VLESS 未验的候选` 出现 3 次（17:32:48、18:00:37、18:23:06）。
**问题**：本轮 13 个 VLESS_FAIL 的 IP，WS 握手只有 228–423 ms，数据面却是 `context deadline exceeded` 卡满 15 s。**WS 成功率对数据面毫无预测力**，降级机制正在把死 IP 放进池子。

**要求**：
- 降级候选必须满足：VLESS 阶段有 `dataLatency` 值（即真的连上过），或 errorClass 不是 `timeout`。
- 纯 WS_PASS + VLESS timeout 的候选不得进入降级池。
- 降级时日志列出被拒绝的候选及原因，不要静默丢弃。

### 改动 C｜（可选，低优先）域名体检的通路依赖
**文件**：`web/domain_latency.go` `probeOneDomain`
**现状**：域名体检解析域名后直连得到的 CF 前置 IP，走的正是坏掉的出口，于是体检必然零样本。
**要求**：评估是否可改为经 `PROXY_AUTO_HOST` 的 worker 路径做健康判定，而不是直连域名解析的 IP。**若改动面超过 30 行，放弃，留 TODO。** 不要为此重构。

---

## 3. 硬约束（主席原话）

- 不提高探测并发、不扩大探测压力
- 不修改 Active 池、不删除现有候选、不调整订阅预算
- 不重构调度核心
- 尽量缩小 diff，不顺手重构其他模块
- 改完 `go test ./...` 必须通过，`gofmt -l` 对改动文件干净

注意：以下文件**本来就没格式化**，不要去动：`web/cfranges_test.go`、`web/endorse_test.go`、`web/fileserver_test.go`、`web/segment_test.go`、`web/subscription_inventory.go`。

---

## 4. 已上线的 v4 改动清单（PR 基线）

来自 `.agent/patches/deployed-v4-working-tree.diff`，共 10 文件 527 行：

| 文件 | 改动 |
|---|---|
| `web/process.go` (+152) | `rotatingLogWriter`：100 MB 单文件上限、每日轮转、最多 3 份、启动时裁切超限存量 |
| `web/config.go` (+9) | `maxLogFileBytes = 100*1024*1024`、`maxLogFileBackups = 3` |
| `cfnat.go` (+62) | `countFixedKinds`/`countIPTargets`/`capFixedTargets`：`-ipnum` 只截 IP 不截域名，修「IP 为 0 时域名被丢弃」 |
| `web/pool.go` (+71) | `gatePoolDomains`、`shouldSkipCFnatRestart`、`sameCFnatArgs`：同参不重启 + 空池域名兜底 |
| `web/domain_latency.go` (+8) | `reconcilePoolDomains` 口径与 apply 统一 |
| `web/main.go` (+25) | `poolDomainMembers` 加观察期过滤 |
| `web/throne_probe.go` (+40) | `throneResultIsTimeout`：域名慢握手只记 slowStrike 不记连败；整轮预算动态化 |
| `web/process_test.go` (+101) | 5 条轮转测试 |
| `cfnat_test.go` (+33) | 2 条域名保留测试 |
| `web/throne_probe_test.go` (+47/-2) | 2 条新测试 + 2 条旧测试调整 |

---

## 5. 验收标准

1. `go test ./... -count=1` 通过（当前基线 310 passed）。
2. `go vet ./web` 通过。
3. 改动 A 后：出口全坏时手动触发一轮域名体检，池子成员数量**不变**，日志出现 `path broken`。
4. 改动 B 后：构造一个 WS_PASS + VLESS timeout 的候选，确认不进降级池。
5. 每项改动单独可回滚，不打包成一次大提交。

---

## 6. 运维常识（避免踩坑）

- NAS 访问：`plink -pw "$NAS_PW" -hostkey "ssh-ed25519 255 SHA256:EbpCOvbtp5Mg7wijFboZr7HFpHq4GaRhGfgnxYMT/D4" administer@192.168.1.50`
- `sudo` 需 `echo "$NAS_PW" | sudo -S`
- `docker logs` 必须 `2>&1 | grep -v 'password for'`，只用 `2>` 会丢内容
- 容器 `FROM scratch`，无 cat/sh/curl；排查用宿主 `python3` 或 `sudo nsenter -t $(docker inspect -f '{{.State.Pid}}' cloudflare-tools) -n python3`
- 出网必须走 `http://sing-box:39281`
- 日志：`/vol1/1000/Docker/CFNat/data/cfnat.log`（cfnat 自身）、`docker logs cloudflare-tools`（web 层调度日志）
- 构建：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w -X main.gitCommit=<tag> -X main.buildTime=<RFC3339Z>" -o dist/linux-amd64/cloudflare-web ./web`
- `/api/health` 的 gitCommit 只显示短 hash，版本判据看 `cfnat.log` 启动行是否含「N IP + M 域名」

---

## 7. 证据文件索引

| 文件 | 内容 |
|---|---|
| `evidence/status_20261111T1830Z.json` | `/api/status` 全量快照：activeProbe 32 成员、cfnat args、forward 计数 |
| `evidence/weblog_1720-1823.txt` | 17:20–18:23 容器全量日志，含所有调度事件 |
| `evidence/outbound_spacing_broad.txt` | NAS 间隔敏感性测试 + 18 目标 × 3 轮广谱 TCP 对照 |
| `evidence/syn_timing.txt` | 10 s 超时建连测试（全成功）+ 建连后发数据黑洞测试 |
| `evidence/proxy-active.json` | 最近一轮 60 条探测明细：WS_FAIL 25 / WS_PASS 15 / VLESS_FAIL 13 / VLESS_PASS 7 |
| `evidence/domain-quality.json` | 域名账本：26 条空记录、2 testing、1 unstable、1 healthy |
| `patches/deployed-v4-working-tree.diff` | 已上线的 527 行未提交改动 |

---

## 8. 明确不要做的事

- ❌ 不要降级官方 CF IP（已证明失败与官方/非官方无关，是整条出口的问题）
- ❌ 不要提高探测并发（`PROXY_AUTO_PRESCREEN_CONCURRENCY` 保持 100）
- ❌ 不要调整订阅预算（`PROXY_SUBSCRIPTION_BUDGET` 保持 900s、VLESS 并发保持 2）
- ❌ 不要扩大 shortlist（保持 60）
- ❌ 不要改 `cfnat` 的 `-ipnum`、`-num`、`-sticky` 语义
- ❌ 不要为了"看起来干净"而批量清洗候选池

**KPI 是 AI 请求能不能成功，不是探测日志里有多少个绿色勾。**
