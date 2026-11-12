# 改动 D 设计：换池抖动与紧急故障切换

> 状态：**只有设计，没有实现**。本文档不包含任何可执行代码改动。
> 线上跑的是 v4（gitCommit 后缀 `thronefix4`），其行为不因本文档而改变。
> 数据取自 2026-11-12 NAS 生产日志。

---

## 0. 先说结论

cfnat 每天重启 30 次以上，每次 stop 都掐断全部在途连接。这不是"池子坏了"：
数据面实测是健康的（池内 10 个 IP 的 TCP:443 全通 58-137 ms，经 1234 端口真实请求
www.cloudflare.com / www.shopify.com / www.aneko.ink 各 3 次全 200，
60 秒内每 2 秒采样 30 次 **30/30 成功**）。**重启是纯损耗**。

v4 已经加了"同参不重启"（`shouldSkipCFnatRestart`），但生产日志里
`proxy active pool applied (unchanged, no restart)` 命中 **0 次**。原因是相邻两次
`applied` 的成员集合**从来没有一次相同**——这个优化瞄准的场景在现实中不存在。

病根有三个，互相独立，必须分开治：

| 编号 | 振荡源 | 代码位置 | 特征 |
|---|---|---|---|
| R1 | 全量重挑无在位粘性 | `web/funnel.go:386` autoApplyProxyPool | IP 侧每 8-13 轮整批换（+5/-5、+6/-6…） |
| R2 | 域名成员每轮重算 | `web/main.go:709` poolDomainMembers | 域名侧 ±1~±2 进出，几乎每轮都变 |
| R3 | 三条后台路径各写各的 -fixed | `web/pool.go:414` applyPoolLocked 无条件 stop+start | 同一秒出现两次 applied，无合并 |

D1 治 R1，D2 治 R2 + R3。第三部分设计紧急故障切换，保证这两项不会把
"该救的火"也一起防抖掉。

---

## 1. 证据

### 1.1 重启频率

cfnat.log 里 `正在监听 0.0.0.0:1234` 行，北京时间 11-12：

| 时段 | 03 | 04 | 05 | 06 | 07 | 08 | 09 | 10 | 11 | 12 |
|---|---|---|---|---|---|---|---|---|---|---|
| 次数 | 2 | 2 | 4 | 3 | 3 | 5 | 5 | 4 | 6 | 2 |

### 1.2 事件统计（docker logs since 9h）

```
proxy active pool applied                36      <- 每次都 stop + start
proxy active pool applied (unchanged...)  0      <- 同参不重启，零命中
domain pool reconciled                   12
throne probe reaped                      13
proxy auto apply skipped                 39
pool switch blocked                      35
```

### 1.3 成员集合从不相同

把相邻两次 `proxy active pool applied` 的成员集合做差：**35 次比较，0 次相同**，
成员数在 32-36 之间跳。

- 小抖动：每 8-13 轮里有一批 ±1~±2 的域名进出
  （cloudflare.468123.xyz、best.hzample.kdns.fr、cloudflare.19931110.xyz、
  cf-cname.xingpingcn.top 反复进出）。
- 大抖动：每 8-13 轮来一次 IP 批量重挑（+5/-5、+6/-6、+7/-7、+8/-5）。

### 1.4 同一秒两次重启

```
15:24:59  proxy active pool applied: ...
15:24:59  throne probe reaped [cdn.aneko.ink]
15:39:23  proxy active pool applied: ...
15:39:23  throne probe reaped [sanwang.cf.3666888.xyz serviceshub.samsclub.com ...]
```

一轮内两条路径各自走完 applyPoolLocked，两次 stop+start，两次断连。

### 1.5 12:03:43 那轮的完整形态

```
12:03:43 domain gate: ping.ctrl.li 体检状态 dead（连败 28），不进转发池
12:03:43 domain gate: cmcc.vvvv.ee 体检状态 dead（连败 31），不进转发池
12:03:43 domain gate: cf.xreak.top 体检状态 dead（连败 31），不进转发池
12:03:43 domain gate: best.hzample.kdns.fr 体检状态 dead（连败 3），不进转发池
12:03:43 domain gate: china-mobile.cname.cloudflare.468123.xyz 体检状态 dead（连败 4），不进转发池
12:03:43 domain pool: cloudflare.468123.xyz 在观察期内，不进自动候选
12:03:43 proxy active pool applied: 172.64.229.143,34.92.1.142,43.155.82.88,...
```

先是一串闸门拒绝，然后立刻换池。这是"每轮重算 -> 集合不同 -> 重启"的典型样本。

---

## 2. D1：池成员粘性（在位优先保留）

### 2.1 现状

`web/funnel.go` 的 `autoApplyProxyPool` 每轮从零重挑：

```go
passed := make([]string, 0, cfg.PoolSize)
...
passed = probeVLESSPool(...)
passed = rankVLESSPassesBySpeed(...)
passed = orderPassedByColoPreference(...)
passed = orderPassedBySegmentHealth(...)
passed = spreadBySegment(passed, cfg.PoolSize)
...
passed = admitPassedUnderCeiling(passed, results, ceiling)
passed = filterPoolAdmissibleIPs(passed)  // 过准入闸
```

`passed` 是一个全新的切片，与 `current.IPs` 毫无关系。因此即使所有在位成员
这轮依然通过，只要排序/分段轮转的输入（候选快照、延迟抖动、片区健康度）
有一点不同，输出顺序就会变；再经 `spreadBySegment` 的分段轮转和
`admitPassedUnderCeiling` 的截断，集合就会变。

`sameIPSet(current.IPs, passed)` 这条提前返回因此几乎不可能命中。

### 2.2 设计

在全量重挑**之后**、写 `pool` **之前**，插一步"在位粘性重排"：

```go
// 伪代码，不是最终实现
passed = promoteIncumbents(passed, current.IPs, cfg.PoolSize)
```

`promoteIncumbents(passed, incumbents, poolSize)` 的语义：

1. `passedSet` = passed 的集合；`incumbentSet` = incumbents 的集合。
2. **保留集** = 两者交集，按 `incumbents` 原顺序排列。这些是"在位且本轮依然
   通过"的成员——成绩没退步，没有理由换掉。
3. **补位集** = passed 中不在保留集里的成员，按 passed 原顺序排列。
4. 输出 = 保留集 ++ 补位集，再截 `poolSize`。

关键性质：

- 若本轮通过的集合与上轮完全相同（只是顺序变了），第 2 步就还原出上轮的顺序，
  输出逐字相同 -> 命中 `sameIPSet` 提前返回 -> 不重启。
- 若新增了更好的成员，它们排在被截断的位置，只有当池子有空位时才进来。
  **这正是"延迟更低的候选没被采用"的代价，必须用 2.4 的换代机制补偿。**
- 若某在位成员这轮没通过，它自然不在 passed 里，也就不会进保留集——照常被换掉。

### 2.3 为什么不做成"永远不换"

`spreadBySegment`（`web/funnel.go:181`）的存在理由是段多样性：不让排在前面
的段吃满名额。粘性重排如果无脑保留全部在位成员，就会把段多样性永久冻结在
第一次建池的状态，新的、更健康的段永远进不来。

所以要在**粘性**和**换代**之间设一个明确的闸。

### 2.4 换代条件（防止粘性变成僵化）

| 参数 | 建议默认 | 含义 |
|---|---|---|
| `PROXY_INCUMBENT_STICKY_MINUTES` | 60 | 在位成员保留超过这么久后，允许被更优候选替换 |
| `PROXY_INCUMBENT_MAX_REPLACE_PER_ROUND` | 2 | 单轮最多替换多少个在位成员 |

算法：

1. 计算每个在位成员已连续保留的时长。需要新增 `pool.MemberSince map[string]time.Time`；
   也可以用现有 `proxyActivePool.UpdatedAt` 做粗略近似，但精度差
   （更新它等于"整池都重新计时"，与意图相反）。
2. 若某在位成员已保留 >= `STICKY_MINUTES`，把它从保留集移出，让它按延迟重新竞争。
3. 补位集里按延迟升序，最多取 `MAX_REPLACE_PER_ROUND` 个与"被移出的在位成员"对换。

这样：

- 稳态（成绩不变）下，集合逐字相同，零重启。
- 有更好的候选时，轮次之间慢慢换代（每轮 <=2 个），而不是一次性整批换掉。
- 换代仍会触发重启，但频率从"每 8-13 轮一次批量换"降到"确实有更优候选时换 1-2 个"。

### 2.5 与现有限制的交互（顺序不能错）

现有流水线：

```
probeVLESSPool -> rankVLESSPassesBySpeed -> orderPassedByColoPreference
  -> orderPassedBySegmentHealth -> spreadBySegment
  -> admitPassedUnderCeiling -> filterPoolAdmissibleIPs
  -> [sameIPSet 提前返回] -> carryPinnedIntoRebuild -> build pool
```

粘性重排应插在 `carryPinnedIntoRebuild` **之后**，`sameIPSet` 判断**之前**：

```
... -> admitPassedUnderCeiling -> filterPoolAdmissibleIPs
    -> carryPinnedIntoRebuild -> promoteIncumbents
    -> [sameIPSet 提前返回] -> build pool
```

理由：

- `carryPinnedIntoRebuild`（`web/pool.go:345`）已经把 `current.Priority` 里的成员
  强制带回。粘性重排要在它之后做，否则置顶成员会被当成普通在位成员，丢失置顶顺序。
- `admitPassedUnderCeiling`（800ms 上限）必须在粘性重排**之前**做，否则会把刚
  保留下来但已超限的在位成员又放回池子。`filterPoolAdmissibleIPs` 同理。
- `sameIPSet` 提前返回必须挪到 `promoteIncumbents` 之后，否则粘性重排完全失去
  "让集合相同"的意义。

### 2.6 风险

- **慢 IP 固化**：若换代机制失效（阈值配错、MemberSince 没更新），池子会永久
  停留在建池那一刻。必须靠 2.4 的 `STICKY_MINUTES` 兜底，并加监控日志：
  在位成员连续保留超过 `STICKY_MINUTES` 时打 `incumbent stale: %s held %s`。
- **段多样性退化**：粘性会让新段进不来。验收里要显式检查池内 IP 的段数量
  不少于部署前。

### 2.7 验收

- 部署后 2 小时内 `proxy active pool applied` 的真重启次数 <= 6 次
  （当前基线约 30/天，约每 48 分钟一次）。
- `proxy active pool applied (unchanged, no restart)` 命中次数 > 0。
- 1234 端口连续采样 10 分钟无中断（每 2 秒一次，300/300 成功）。
- 池内 IP 的延迟中位数不劣于部署前（粘性不能变成"永久保留慢 IP"）。
- 池内 IP 的段数量不少于部署前。

---

## 3. D2：应用防抖与合并窗口

### 3.1 现状

三条后台路径各自调用 `applyProxyPool` -> `applyPoolLocked`：

| 路径 | 入口 | 触发频率 |
|---|---|---|
| 自动全量换池 | `web/main.go` refreshAndApplyProxyPool -> `web/funnel.go` autoApplyProxyPool | 每 10-12 分钟 |
| 在位验活剔人 | `web/throne_probe.go:405` reapThroneMembers | 快速复检 120s / 常规 600s |
| 域名体检回池 | `web/domain_latency.go:760` reconcilePoolDomains | 每 30 分钟 |

`applyPoolLocked`（`web/pool.go:414`）拿 `cfnatCtlMu` 后无条件 `stop()` + `start()`，
没有"距离上次重启不足 N 分钟就只落盘"的判断。v4 的 `shouldSkipCFnatRestart`
只在**参数逐字相同**时才跳过，而集合每次都不同，所以从不命中。

### 3.2 设计：两道闸，按变更幅度分级

核心思路：**不是所有池变更都值得断连**。按"这次变更会不会改变实际转发行为"
分级，只对真正改变行为的变更重启。

#### 第一道：变更分类

在 `applyPoolLocked` 算出 `fixedMembers` 之后、决定是否 stop 之前，
把"新 `-fixed` 集合"与"当前运行进程的 `-fixed` 集合"（`a.cfnat.status().Args`）做比较，
分三档：

| 档位 | 判据 | 处置 |
|---|---|---|
| **A. 无变化** | 集合逐字相同（现 v4 的 `sameCFnatArgs`） | 只落盘（`commitPoolWithoutRestart`），不重启 |
| **B. 小幅** | 集合差异 <= `PROXY_POOL_RESTART_MIN_DELTA`（建议 2），**且没有成员被剔除**（纯增） | 先落盘，进入合并窗口延后重启 |
| **C. 大幅** | 差异 > 阈值，**或**有成员被剔除，**或**池规模跌到 `MinPool` 以下 | 立即重启 |

"剔除"为什么单独算严重：`-fixed` 里剔掉一个成员，意味着正在用它的粘性设备
下一跳会失败。小幅纯增（多一个候选）对现有连接无害。

#### 第二道：合并窗口

引入 `pendingRestart` 状态（内存 + 可选落盘），记录"已落盘但进程尚未切换"的池：

```go
type pendingRestart struct {
    Pool      proxyActivePool // 已落盘的目标池
    FirstSeen time.Time       // 第一次被判为 B 档的时间
    LastMerge time.Time       // 最后一次被合并的时间
    Delta     int             // 累计净差异
}
```

规则：

1. B 档变更：`commitPoolWithoutRestart(pool)` 落盘，记 `FirstSeen`（若已存在则只更新
   `LastMerge` 与 `Delta`）。日志 `proxy pool restart deferred: +N/-M members, window ...`。
2. 窗口内（`PROXY_POOL_MERGE_WINDOW_SECONDS`，建议 300 = 5 分钟）再来的 B 档变更
   继续合并，不重启。
3. 窗口到期，或来了 C 档变更，或累计 `Delta` 超过 `PROXY_POOL_RESTART_MIN_DELTA`
   -> 执行真重启，`-fixed` 用**当前最新落盘池**（不是第一次的池）。
4. 后台 goroutine（挂到 `runBackgroundOptimizerLoop` 或新增 30s ticker）检查窗口到期。

窗口 5 分钟的理由：自动换池每 10-12 分钟一轮，域名体检 30 分钟一轮，
在位验活 2-10 分钟一轮。5 分钟窗口能把"一轮内多路径各改一次"合并成一次重启，
又不会让 C 档等太久（C 档本来就不走窗口）。

#### 第三道：进程状态保护（必须有）

窗口里若 cfnat 进程意外退出，**必须立刻用落盘池启动**，
不能让"等窗口"变成"等一个已经死掉的进程"。

判据：窗口检查时若 `a.cfnat.status().Running == false`，不管窗口是否到期，
立刻 `start()` 并清空 pending。

### 3.3 为什么不用简单的"最小重启间隔"

最简单的方案是"两次重启间隔至少 10 分钟"。它的问题：

- **饿死 C 档**：整池判死救援被间隔挡住 = 断网。要开豁免就得再写一套分类逻辑，
  等于绕回本文档的方案。
- **掩盖真实问题**：间隔内多次变更被丢弃，日志里看不出"池子其实改了很多次"。
- 合并窗口天然带了分类，日志也更诚实（`deferred: +3/-1` 能看出改动幅度）。

### 3.4 与冷却闸的关系

`allowPoolSwitch`（`web/pool.go:794`）是**换池准入**闸，防的是"自动换池互相顶池"。
D2 是**重启时机**闸，防的是"改了就要断连"。两者职责不同，不能合并：

- 冷却闸挡住的是"这次换池该不该发生"。
- 合并窗口管的是"该发生的换池什么时候落到进程上"。

叠加是安全的：冷却闸先拒 -> 根本走不到窗口。

### 3.5 验收

- 部署后 2 小时内真重启次数 <= 4 次。
- `proxy pool restart deferred` 日志出现，且每次合并至少省下 1 次重启。
- 窗口内 cfnat 退出时，能在 <= 30 秒内用落盘池重新启动（有测试覆盖）。
- C 档变更不被窗口延迟。

---

## 4. 紧急故障切换（不能被 D1/D2 拖慢）

D1 的粘性和 D2 的窗口都会引入"延迟响应"。必须有一条**无条件优先**的通道。

### 4.1 已有的救援机制

`web/pool.go:794` `allowPoolSwitch` 第一步就是整池判死救援：

```go
if allDead, deadCount := a.activePoolAllDead(forwarding); allDead {
    log.Printf("pool switch RESCUE: all %d members reached failure threshold, bypassing cooldown/health gate (%s)", deadCount, reason)
    return poolSwitchVerdict{Allowed: true, Reason: "rescue", Rescue: true}
}
```

`activePoolAllDead`（`web/pool.go:693`）的判据是每个在位 IP 的
`ConsecutiveFailures >= throneProbeFailThreshold()`。

这个机制**只覆盖"全死"**，不覆盖"死了一半"。

### 4.2 缺口：部分死亡时的响应

现实形态：10 个 IP 里 6 个死了、4 个还活着。此时：

- `activePoolAllDead` 返回 false（不是全死）-> 不走救援。
- `activePoolHealthy` 返回 false（有失败）-> 走不到健康闸放行。
- 但冷却闸（`poolSwitchCooldown` 默认 30 分钟）会拦住自动换池
  -> **带着 6 个死成员继续跑 30 分钟**。
- 同时 D1 的粘性对这些死成员没影响（它们不会通过探测、进不了 `passed`），
  但 `MAX_REPLACE_PER_ROUND=2` 会让补位速度变慢。

### 4.3 设计：分级故障切换

引入 `poolHealthLevel`（纯计算，不新增持久状态）：

| 级别 | 判据 | 处置 |
|---|---|---|
| **HEALTHY** | 所有在位成员近 N 次无失败 | 正常：冷却闸 + D2 合并窗口 |
| **DEGRADED** | 存活率 < 100% 但 >= 50% | 窗口缩到 60s；`MAX_REPLACE_PER_ROUND` 提到 `MinPool/2` |
| **CRITICAL** | 存活率 < 50% | 跳过冷却闸与窗口，立即全量重挑并重启；粘性权重降为 0 |
| **DEAD** | 存活率 = 0 | 现成的 RESCUE 通道，不变 |

"存活率"用什么算：

- 首选 `a.throneProbe` 里的 `ConsecutiveFailures` 分布（`throneProbeFailThreshold`
  作为"死"的判据）。它是主动探测，快速复检 120s，延迟低。
- 次选 `a.activeHealth` 的被动计数（`web/pool.go:837`）。依赖真实流量，
  池子冷的时候没有样本。

### 4.4 与 D1/D2 的优先级

`CRITICAL` 与 `DEAD` 必须**完全绕过** D1 和 D2：

```go
// 伪代码
level := a.poolHealthLevel(forwardingIPs(current))
if level >= levelCritical {
    // 跳过粘性：直接采用本轮全量重挑的结果
    // 跳过合并窗口：立即 apply
}
```

这条优先级要写进 `applyPoolLocked` 的最前面，而不是散落在各调用方——
否则新增一条后台路径就会忘掉，重演"救援被冷却挡住"的老问题。

### 4.5 必须避免的反模式

不要在 `CRITICAL` 时"清空池子再从零建"。`planFixedMembers` 对空集合会报错
（`没有可转发的成员：生效池成员已全部关闭`），而 `applyPoolLocked` 失败会回滚到
`rollbackArgs`（旧参数）——等于什么都没做。

正确做法：**新池算出来之前不动旧池**，算出来之后一次性替换。

### 4.6 验收

- 模拟 6/10 成员死亡：60 秒内触发全量重挑并重启（当前行为是等 30 分钟冷却）。
- 模拟 2/10 成员死亡：走 DEGRADED，窗口 60s，一轮补位 >= 1 个。
- 模拟 10/10 全死：走现成 RESCUE，行为不变（回归测试）。
- 所有级别切换都有日志，能看出当前是哪一档。

---

## 5. 实施顺序建议

三项不能一起上：

1. **D1（池成员粘性）先上**。它单独就能消掉大部分重启（IP 侧整批换是最大振荡源），
   且改动局限在 `autoApplyProxyPool` 一处，风险最低。
2. **观察 24 小时**：`applied` 次数、`unchanged` 命中次数、池内 IP 延迟分布、段数量。
3. **D2（防抖窗口）再上**。它依赖 D1 已降低变更频率，否则窗口会被 C 档频繁打断。
4. **紧急故障切换**与 D2 同期上，因为 D2 引入了"延迟响应"，必须同时提供优先通道。

每一项独立可回滚：D1 是一个函数调用，D2 是一个状态机，故障切换是一个判据函数。

---

## 6. 明确不做的事

- 不改 `cfnat.go` 的转发逻辑。它是数据面，动它风险最高、收益最小。
- 不改探测预算与并发（第三项，主席明确暂缓，需先拿数据）。
- 不降级官方 CF IP（主席明确不批准）。
- 不用"清空池子重建"做故障切换。
- 不把 D1/D2 的阈值做成运行时热调。先给固定默认值，观察数据后再定。

---

## 7. 待定参数汇总

| 参数 | 建议默认 | 依据 | 需实验确认 |
|---|---|---|---|
| `PROXY_INCUMBENT_STICKY_MINUTES` | 60 | 池子换代不宜快于 1 小时 | 是 |
| `PROXY_INCUMBENT_MAX_REPLACE_PER_ROUND` | 2 | 单轮 <=2 个，避免整批换 | 是 |
| `PROXY_POOL_MERGE_WINDOW_SECONDS` | 300 | 覆盖自动换池 10-12 分钟周期内的一轮 | 是 |
| `PROXY_POOL_RESTART_MIN_DELTA` | 2 | 差异 <=2 个不值得断连 | 是 |
| DEGRADED 窗口 | 60 | 存活率 50-99% 时加快响应 | 是 |
| CRITICAL 阈值 | 存活率 < 50% | 半数已死，等不起 | 是 |

全部先给固定值，不做热调。等生产数据出来再定。
