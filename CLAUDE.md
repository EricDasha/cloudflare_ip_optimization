# cloudflare-tools — 项目须知

> 这份文件在每次会话自动加载。**先读它，再读 `.agent/PROJECT-CONTEXT.md`。**
> 如果记忆、上下文、对话记录全部丢失，这两份文件足以让任何 agent 接手。

## 这个项目为什么存在

为两个上游项目服务：

| 项目 | 角色 |
|---|---|
| [cmliu/edgetunnel](https://github.com/cmliu/edgetunnel) | 部署在 Cloudflare Workers 上的 VLESS over WebSocket 服务端 |
| [xtgm/GrainTCPV1](https://github.com/xtgm/GrainTCPV1) | 前端代理软件，消费上游节点 |

**数据流**：

```
订阅器（vlesdy.trojanjd.dpdns.org）
        ↓  拉取候选 IP / 反代域名
cloudflare-web（本仓库，跑在 NAS Docker 里）
        ↓  三层漏斗探测，选出「生效池」
cfnat（同容器，监听 0.0.0.0:1234）
        ↓  按 -fixed 列表 TCP 转发
前端代理软件（GrainTCPV1 / Throne）
        ↓  TLS + WS + VLESS
Cloudflare 边缘 → Worker → 目标站点
```

一句话：**本仓库唯一的产出是「一份当下可用的 IP/域名清单」，并把它塞进 cfnat 的转发参数里。**

## 三个程序

| 二进制 | 源码 | 职责 |
|---|---|---|
| `cloudflare-web` | `web/`（Go package `local/cloudflare-tools/web`） | 管理界面 + 调度器：探测候选、维护生效池、写 cfnat 启动参数 |
| `cfnat` | `cfnat.go`（`//go:build ignore`，单独 `go build cfnat.go`） | TCP 转发器，监听 1234，按 `-fixed` 依次尝试目标 |
| `cfdata` | `cfdata.go`（同上） | 辅助数据面 |

三者都打进同一个 `FROM scratch` 镜像。**容器里没有 shell、没有 curl、没有 cat**——排查只能在 NAS 宿主上做。

## 三层漏斗（`web/funnel.go` `autoApplyProxyPool`）

1. **初筛**：全量候选（约 990 个）做 TCP/TLS RTT 测试，并发 100，按延迟取前 60 进 shortlist。
2. **精筛**：shortlist 做 WS 101 握手，可选再做 VLESS 数据面终审。
3. **在位**：cfnat 用 `-sticky=true` 按设备粘性分发。

通过的候选经「延迟排序 → 机房偏好 → 片区健康 → 段分散」重排，截 `PoolSize` 后成为生效池，写入 `proxy-active.json` 与 cfnat 的 `-fixed`。

## 硬约束（主席定的，不要违反）

- 不提高探测并发、不扩大探测压力（`PROXY_AUTO_PRESCREEN_CONCURRENCY` 保持 100）
- 不调整订阅预算（`PROXY_SUBSCRIPTION_BUDGET` 保持 900s，VLESS 并发保持 2）
- 不扩大 shortlist（保持 60）
- 不动 Active 池、不删候选
- 不改 `cfnat` 的 `-ipnum`、`-num`、`-sticky` 语义
- 不重构调度核心，尽量缩小 diff
- 改完 `go test ./... -count=1` 必须通过

## 五个最容易犯的错

1. **不要降级官方 CF IP。** 直觉上「官方段探测失败率高就该排除」，但实测失败的是整条 NAS 出口——shopify、github、google 的自有 IP 同时超时。降级只缩小可选面，治不了病。
2. **不要用 IP 当 SNI 打 TLS。** `server_hostname="104.18.42.163"` 会得到 `SSLError`，这跟网络通不通毫无关系。要测就用真实域名做 SNI。
3. **不要只发 `\x16\x03\x01` 三字节就等回应。** 半包不发 ClientHello body，所有目标（含国内 223.5.5.5）都不回应。这是通用行为，不是「payload 被黑洞」。
4. **在本机测 1234 必须加 `--noproxy '*'`。** 否则 curl 会走本机 Throne 的 2080 端口，结果被本地代理污染。
5. **不要拿探测日志的绿色勾当 KPI。** KPI 是「经 1234 端口的真实请求成功率」。

## 常用运维命令

```bash
# NAS 连接（Windows Git Bash）
PLINK="/c/Program Files/PuTTY/plink"
"$PLINK" -ssh -batch -pw "$NAS_PW" \
  -hostkey "ssh-ed25519 255 SHA256:EbpCOvbtp5Mg7wijFboZr7HFpHq4GaRhGfgnxYMT/D4" \
  administer@192.168.1.50 "<命令>"

# 健康检查
curl -s http://192.168.1.50:8080/api/health

# 看 cfnat 实际启动参数（容器无 sh，只能用 docker top）
docker top cloudflare-tools -o pid,args

# 看调度日志（web 层）
docker logs --since 30m cloudflare-tools 2>&1 | grep -E 'proxy active pool applied|prescreen|reaped|reconciled'

# 重启计数（cfnat 每次启动都打这行）
grep -c '正在监听 0.0.0.0:1234' /vol1/1000/Docker/CFNat/data/cfnat.log

# 数据面验证（在 NAS 上）
curl -s -o /dev/null -w '%{http_code} %{time_total}\n' --noproxy '*' \
  --connect-to www.cloudflare.com:443:127.0.0.1:1234 https://www.cloudflare.com/cdn-cgi/trace
```

**构建与部署**：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.gitCommit=<tag> -X main.buildTime=<RFC3339Z>" \
  -o dist/linux-amd64/cloudflare-web ./web
# pscp 上传到 /vol1/1000/Docker/CFNat/dist/linux-amd64/
# 然后 docker compose build && docker compose up -d
```

部署前**必须**：备份 `proxy-active.json` + `docker-compose.yml` + `.env` 到 `_backup/pre-<改动名>-<时间戳>/`，并 `docker tag local/cloudflare-tools:latest local/cloudflare-tools:rollback-<时间戳>`。

## 版本判据

`/api/health` 的 `gitCommit` 只显示 `main.gitCommit` 里给的那个短串，工作区未提交时它不会变。**真正的判据是 `cfnat.log` 的启动行**：

```
成功提取 10 个有效IP + 24 个域名，耗时 0秒
```

「N IP + M 域名」这个格式是 v4 之后的版本才有。

## 已知问题（2026-11-12 状态）

| 问题 | 状态 |
|---|---|
| 日志无限增长（曾到 639 MB） | 已修（`rotatingLogWriter`，100 MB × 3 份） |
| `-ipnum` 截断把域名挤掉 | 已修（`capFixedTargets` 只截 IP） |
| 验活把慢域名成批判死 | 已修（`throneResultIsTimeout`，域名超时只记 slowStrike） |
| **cfnat 每 10–30 分钟重启一次** | **D1 已上线，观察中**（在位粘性） |
| 换池抖动（D2 防抖窗口 + 紧急故障切换） | 只有设计，未实施，见 `.agent/CHANGE-D-DESIGN.md` |
| 非 CF 域名走反代 IP 必然 TLS 失败 | 结构性，不修（证书不含该 SNI） |

## 目录

```
web/                    主程序（唯一的 Go package）
cfnat.go cfdata.go      独立二进制（//go:build ignore）
scripts/build-dist.ps1  Windows 构建脚本
scripts/nas/            NAS 侧运维脚本（看门狗等）
.agent/                 交接材料：PROJECT-CONTEXT.md 是本项目的完整说明书
```

`.agent/PROJECT-CONTEXT.md` 有架构细节、改动史、每次部署的回滚点、以及全部踩过的坑。
