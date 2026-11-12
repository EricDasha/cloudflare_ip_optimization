# CI 落地报告（2026-11-12）

分支：`ci/github-actions-20261112`　PR：**#3**　base：`audit/round2-20261112`
最新提交：`7788681`　CI run：**43230103416**（6 job 全绿）

---

## 1. 做了什么

三件事，全部在 PR #3 内，**不含任何功能代码改动**：

1. **新增 GitHub Actions Linux CI**（`.github/workflows/ci.yml`，208 行）。
2. **跨平台修复 22 项测试失败**（`web/fakebin_test.go` 新增 218 行 + 9 个测试文件改夹具）。
3. **改动 D 设计文档**（`.agent/CHANGE-D-DESIGN.md`，434 行，只有设计，没有实现）。

分支起点是 `origin/audit/round2-20261112`（PR2 tip），因为 PR2 依赖 PR1，
必须叠着测才覆盖真实最终状态。

---

## 2. CI 结构

| job | 内容 | 时长 |
|---|---|---|
| `go test` | `go test ./... -count=1` | 约 20s |
| `go test -race` | `go test -race ./... -count=1` | 约 3–4 min |
| `go vet` | `go vet ./...` | 约 20s |
| `linux build` | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 构建 cloudflare-web / cfnat / cfdata 三个产物，并 `file` 校验 | 约 30s |
| `pr head raw / handoff/20261112` | checkout PR1 **原始 head**，跑全套测试，断言失败集合 | 约 25s |
| `pr head raw / audit/round2-20261112` | checkout PR2 **原始 head**，同上 | 约 25s |

`pr-baseline` 两个 job 是**特征化断言**，不是放水：

- 从 `go test -v` 输出抽 `^--- FAIL: ` 的测试名，`sort -u`，与已知基线 `diff -u`。
- 集合**逐字相同** → 通过（step summary 注明「已知失败，无新增」）。
- 集合**多一项** → 失败（原始 head 上出现了新问题）。
- 集合**少一项** → 也失败（基线漂了，或有人改了 raw head，需要人看一眼）。

> 中间试过 `continue-on-error: true`，实测 **GitHub 会忽略 job 级 `continue-on-error`
> 对 run 结论的影响**，run 仍然记 failure。也试过 `set +e` + `exit 0`，那等于把失败
> 变成假通过，正是主席明令禁止的做法，已废弃。

---

## 3. 22 项失败的根因与修法

### 根因：一个，不是 22 个

所有失败都是同一句报错：

```
fork/exec ...\fake-cfnat.sh: %1 is not a valid Win32 application.
fork/exec ...\fake-sing-box.sh: %1 is not a valid Win32 application.
fork/exec ...\fake-sing-box.py: %1 is not a valid Win32 application.
exec: "/bin/true": executable file not found in %PATH%
```

测试用 `#!/bin/sh` 的 shell 脚本和 `#!/usr/bin/env python3` 的 python 脚本当假二进制
（`fake-cfnat.sh`、`fake-sing-box.sh`、`fake-sing-box.py`）。Windows 上 `.sh`/`.py`
不是合法可执行文件，`fork/exec` 直接失败。

这是**测试夹具设计错误**，不是被测代码的缺陷，也不是"只能在 Linux 上跑"的合法平台依赖
——夹具完全可以写成跨平台的。

### 分布

| 分支 | 失败项 | 说明 |
|---|---|---|
| PR1 `handoff/20261112` | 7 | `TestHandleActiveEndorsePinsToFront`、`TestApplyPoolLockedClearsStalePoolError`、`TestApplyPoolLockedRollbackRestoresRunningArgs`、`TestApplyPoolLockedPrunesStaleActiveHealth`、`TestManagedProcessFailedStartDoesNotPoisonState`、`TestProbeVLESSCandidateMetricsRetriesOnBindConflict`、`TestProbeVLESSCandidateMetricsFailsFastOnBadStatus` |
| PR2 `audit/round2-20261112` | 15 | 上面 7 项 + 8 项：`TestApplyPoolRebasedAbortedIsNoop`、`TestApplyPoolRebasedAbortsWhenPoolChangedDuringSubmit`、`TestApplyPoolRebasedKeepsConcurrentChange`、`TestProbeVLESSCandidateMetricsNotProvenOnProxyError`、`TestProbeVLESSCandidateMetricsOnceDetectsDeadProcessOnDial`、`TestProbeVLESSCandidateMetricsProvenOnStatusMismatch`、`TestProbeVLESSCandidateMetricsProvenOnSuccess`、`TestVLESSProbeCandidateLimit` |

合计 22 项，与主席指出的数字一致。

### 修法：测试二进制复执行自身

新增 `web/fakebin_test.go`（218 行）。核心思路：假二进制不用外部脚本，
用**测试二进制自己**，通过环境变量认领身份。

```go
func installFakeBinary(t *testing.T, mode, argFile string) string {
    exe, err := os.Executable()   // 失败退回 os.Args[0]
    ...
    t.Setenv("GO_FAKE_BIN", mode)
    return exe
}

func TestMain(m *testing.M) {   // 在 web/segment_health_test.go
    runFakeBinaryIfChild()      // 环境变量为空则直接返回
    os.Exit(m.Run())
}
```

`runFakeBinaryIfChild()` 按 `GO_FAKE_BIN` 分派后 `os.Exit`，支持的模式：

| 模式 | 行为 | 替代的原夹具 |
|---|---|---|
| `cfnat-sleep` | 记录 argv 到 `$GO_FAKE_BIN_ARGFILE`，然后挂起等 Kill | `fake-cfnat.sh` |
| `singbox-bind-conflict` | 记录 argv，往 stderr 打 `FATAL: listen tcp 127.0.0.1: bind: address already in use`，退出码 1 | `fake-sing-box.sh`（bind 冲突） |
| `singbox-dead` | 睡 60ms 后打同样的 bind 报错退出 | `fake-sing-box-dead.sh` |
| `singbox-proxy` | 解析 `-c` 配置取端口，起真 HTTP server，状态码/body 由 `FAKE_PROXY_STATUS`/`FAKE_PROXY_BODY` 控制 | `fake-sing-box.py` |
| `singbox-slow-body` | 先发 5 MB 的 `Content-Length` 头再 flush，然后挂 10s | `fake-sing-box.py`（慢 body 变体） |
| `exit-zero` | 立即正常退出 | `/bin/true` |

**关键点：断言一行没改。** 变的只是"假二进制是什么"，不是"它应该表现成什么"。
`pool_restart_test.go` 里原来靠 `echo "$@" >> logfile` 记录参数，现在由夹具直接写文件，
断言语义完全相同。

### 一个真实的 Linux 缺陷

`fakeCFnatSleep` 最初用 `select {}` 挂起。Linux 上 Go 运行时会判定
`fatal error: all goroutines are asleep - deadlock!`，进程自己退出（退出码 2），
根本等不到被测代码去 Kill 它，导致 `TestApplyPoolLockedRollbackRestoresRunningArgs`
在 Linux 上必失败。改成 `<-time.After(30 * time.Minute)` 后通过。

这个坑在 Windows 上不会暴露（Windows 的 Go 运行时行为不同），所以之前的 Windows
测试从未发现。**这是 CI 的第一个真实收益。**

---

## 4. 各 PR 的测试结果

### 4.1 PR #1 `handoff/20261112`（原始 head，未含夹具修复）

| 指标 | 结果 |
|---|---|
| `go vet ./...` | **通过**（无输出） |
| 测试总数 | 350 |
| 通过 | 349 |
| 失败 | **1** |

失败详情：

```
--- FAIL: TestAllowedFileWhitelistUnchanged (0.00s)
    fileserver_test.go:46: path traversal must be denied
```

原因：`web/fileserver_test.go:45` 写死了反斜杠路径 `..\ip.csv`。
`allowedFile()` 的第一行是 `if name != filepath.Base(name) { return false }`，
在 Linux 上 `filepath.Base("..\\ip.csv")` 返回 `..\ip.csv` 本身（反斜杠不是分隔符），
于是 `allowedFile` 按 `.csv` 后缀放行 → 断言失败。

这是**测试断言的平台依赖**，被测代码 `allowedFile` 本身在 Linux 上行为正确
（反斜杠文件名确实不是路径穿越）。

### 4.2 PR #2 `audit/round2-20261112`（原始 head）

| 指标 | 结果 |
|---|---|
| `go vet ./...` | **通过** |
| 测试总数 | 414 |
| 通过 | 413 |
| 失败 | **1**（同上 `TestAllowedFileWhitelistUnchanged`） |

### 4.3 PR #3 `ci/github-actions-20261112`（本分支，含夹具修复）

| 指标 | Windows 本机 | Linux CI |
|---|---|---|
| `go vet ./...` | 通过 | **通过** |
| `go test ./... -count=1` | **413 通过 / 0 失败** | **通过** |
| `go test -race ./...` | 未跑（Windows 无 gcc） | **通过** |
| linux build | — | **通过**（三个产物） |

---

## 5. 剩余风险

### R1｜PR1/PR2 自身仍带那个平台依赖断言（高）

夹具修复只在 PR #3 里。PR1 和 PR2 的 `web/fileserver_test.go` 仍然写死 `..\ip.csv`，
在 Linux CI 上永远红一项。

**影响**：PR1/PR2 单独看 CI 都是红的，评审者会以为代码有问题。

**建议**：PR #3 与 PR1 同时合入，或把 `fileserver_test.go` 那 11 行改动单独摘进 PR1。
后者更干净——它本来就是 PR1 自己引入的测试。

### R2｜pr-baseline 的 `expected` 需要人工清零（中）

CI 里两个 `expected` 都硬编码为 `TestAllowedFileWhitelistUnchanged`。
夹具修复合入 PR1/PR2 后，原始 head 上这项就不再失败，`actual` 变空，
`diff` 不匹配 → **job 反而会红**。

这是设计上故意的（防止基线悄悄漂移），但**合入 PR1/PR2 时必须同步把 `expected` 清空**，
否则 CI 会在"修好了"的那一刻报错。已在 workflow 注释里写明，但没有自动机制。

**建议**：合入 PR1/PR2 的同一次提交里改 `ci.yml`，把两个 `expected` 改成空串。

### R3｜PR2 叠在 PR1 上，合并顺序不能错（高）

PR #2 的 base 是 `handoff/20261112`（PR1 的 head），不是 `main`。
必须先合 PR1、再让 PR2 rebase 到 `main`、重跑 CI。

PR #3 同理，base 是 PR2 head。合并顺序必须是：**PR1 → PR2 → PR3**。

如果反过来，GitHub 会因为 base 分支被删而自动关闭后续 PR。

### R4｜无 docker 环境，容器构建未在 CI 覆盖（中）

CI 只验证 `go build` 产出二进制，**不验证 `Dockerfile` 能构建、镜像能起来**。
线上是 `FROM scratch` + `docker compose build`，这条路径完全没被 CI 覆盖。

**建议**：后续加一个 `docker build` job（不需要 push，只需要 build 成功）。
但 `Dockerfile` 依赖 `dist/linux-amd64/sing-box`（第三方二进制，不在仓库里），
需要先解决这个依赖，暂不阻塞本次。

### R5｜`cfnat.go` / `cfdata.go` 的测试覆盖薄（中）

这两个文件带 `//go:build ignore`，`go test ./...` 不会带上它们。
CI 只做了 `go build` 验证能编译，**没跑它们的测试**。

PR1 新增了 `cfnat_test.go`、`cfdata_test.go`、`download_limit_test.go`、
`pipe_timeout_test.go`，但这些在 `./...` 之外。需要单独的 job：
`go test cfnat.go cfnat_test.go` 这种按文件列表的方式。

**当前状态**：CI 里没有这一步。属于已知缺口，不影响本次验收。

### R6｜线上重启问题一行未修（高，非 CI 范围）

CI 转绿不代表线上问题解决。生产日志实测：

- 11-12 北京时间 03:00–12:00，每小时 2/2/4/3/3/5/5/4/6/2 次重启，**30 次/9 小时**。
- 9 小时事件统计：`proxy active pool applied` **36** 次，
  `proxy active pool applied (unchanged, no restart)` **0** 次。
- 相邻两次 applied 的成员集合做差：**35 次比较，0 次相同**，成员数在 32–36 跳。

v4 的"同参不重启"优化瞄准的场景**在现实中不存在**。真正原因是
`autoApplyProxyPool` 每轮全量重挑、无在位粘性，加上域名集合每轮重算。

这就是改动 D 要解决的（`.agent/CHANGE-D-DESIGN.md`），**本次只有设计，没有实现**。

### R7｜`.agent` 证据冲突（低，已在 docs 分支修正）

PR1 的 `.agent/HANDOFF.md` 第 21 行仍写着「P0｜NAS 出口对境外 IP 存在 TCP payload 黑洞」，
证据是「建连成功后立刻发数据 → 6.1 s 无任何回应」。

这条结论**已被推翻**，两个方法论错误：

1. **用 IP 当 SNI 打 TLS 会得到 `TypeError`，不代表网络不通。** 换真实 SNI 后
   `104.18.42.163` / `172.64.147.104` / `104.18.33.74` / `104.18.41.208` 全部
   `OK tcp 106–116ms tls 329–362ms`。
2. **只发 `\x16\x03\x01` 三字节不发 ClientHello body，所有目标都超时**——
   包括 fastly `151.101.1.140` 和国内 `223.5.5.5`。这是"半包不响应"的通用行为，
   不是 CF 特有。

决定性反证：经 `104.18.42.163`（SNI `www.shopify.com`）真实 GET → **`HTTP/1.1 200 OK`，673ms**。

修正已推到 `docs/audit-correction-20261112`（`cf3c5f0`），**但这个分支没合进 PR1**，
所以 PR1 里的 HANDOFF 仍是错的。

**建议**：合 PR1 时把 `docs/audit-correction-20261112` 一并合入，或手工摘那 4 个文件的改动。

---

## 6. 改动 D 设计摘要

完整文档：`.agent/CHANGE-D-DESIGN.md`（434 行）。

三个独立病根：

| 编号 | 振荡源 | 代码位置 |
|---|---|---|
| R1 | 全量重挑无在位粘性 | `web/funnel.go` `autoApplyProxyPool` |
| R2 | 域名成员每轮重算 | `web/main.go` `poolDomainMembers` |
| R3 | 三条后台路径各写各的 `-fixed`，无合并 | `web/pool.go` `applyPoolLocked` |

两个改动方向：

- **D1 池成员粘性**：新增 `promoteIncumbents(passed, incumbents, poolSize)`，
  保留集（在位且本轮通过，按在位顺序）++ 补位集，截 `poolSize`。
  插入点在 `carryPinnedIntoRebuild` 之后、`sameIPSet` 提前返回之前。
  配套换代闸：`PROXY_INCUMBENT_STICKY_MINUTES=60`、
  `PROXY_INCUMBENT_MAX_REPLACE_PER_ROUND=2`，防止慢 IP 固化。
- **D2 应用防抖窗口**：按变更幅度分 A/B/C 三档，只有 C 档（有成员被剔除或跌到
  `MinPool` 以下）立即重启，B 档进 300s 合并窗口。

紧急故障切换单独设计，分 HEALTHY / DEGRADED / CRITICAL / DEAD 四级，
CRITICAL 与 DEAD 完全绕过 D1 和 D2。缺口是"部分死亡"——
现在 10 个 IP 死 6 个，`activePoolAllDead` 不触发，冷却闸会拦 30 分钟。

**实施顺序**：D1 先上 → 观察 24 小时 → D2 + 紧急切换同期上。
每项独立可回滚。

---

## 7. 等审批的事项

1. **PR #3 是否合入**（建议：与 PR1 同时合，先解决 R1）。
2. **PR1 → PR2 → PR3 的合并顺序**是否按建议执行。
3. **R2 的 `expected` 清零**是否授权我在合 PR1 时一并改。
4. **R7 的 docs 分支**是否一并合入 PR1（修正 HANDOFF 的错误结论）。
5. **改动 D** 是否按"D1 先上、观察 24 小时、再上 D2"的顺序实施。

线上仍是 v4（`b1e5133+thronefix4`），本次全程未动线上任何东西。
