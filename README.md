# Cloudflare Tools Console

本项目把 `cfdata.go` 与 `cfnat.go` 打进同一个本地 Docker 镜像，并提供一个 Web 控制台：

架构与故障边界见 [`DESIGN.md`](DESIGN.md)。

- Web UI：`http://localhost:8080`
- CFnat 转发入口：`localhost:1234`
- 数据与扫描产物：`./data`

## 启动（无 Docker Hub 拉取）

先在本机编译 Linux 静态产物：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build-dist.ps1
```

再构建并启动容器：

```bash
docker compose up -d --build
```

默认 `Dockerfile` 使用 `FROM scratch`，不会拉取 `golang` / `alpine` 基础镜像，适合 Docker Hub 或镜像站不可用的环境。

如果你的 Docker Hub 可用，也可以改用传统多阶段构建：

```bash
docker compose build --build-arg BUILDKIT_INLINE_CACHE=1
docker build -f Dockerfile.multistage -t local/cloudflare-tools:multistage .
```

## 部署验收（硬门禁）

不允许"应该是最新镜像"这种模糊状态——每轮部署必须闭环核验 `gitCommit`：

```bash
# 0) 部署前取定 HEAD（此 commit 即本轮验收基准）
git rev-parse --short HEAD

# 1) 本机编译（ldflags 注入 gitCommit/buildTime）
powershell -ExecutionPolicy Bypass -File .\scripts\build-dist.ps1   # Windows
# 或 ./scripts/build-dist.sh

# 2) 重建并启动
docker compose build --no-cache
docker compose up -d

# 3) 硬验收：gitCommit 必须 == 第 0 步的 commit
curl -s http://<host>:8080/api/health
# {"ok":true,...,"version":{"gitCommit":"<必须一致>","buildTime":"..."}}
```

不一致即部署失败：检查 `build-dist` 是否在构建前运行（Dockerfile 只 COPY `dist/`，不自行编译）、`dist/linux-amd64/cloudflare-web` 的时间戳是否新于 commit。

部署后的正确观察顺序（**首轮不换池不是失败**）：

```text
GET /api/status → qualityScheduler：
  lastProbeAt 在走        ← 调度活着
  Active 建 latency 基线   ← WS 轨裁判就位（≤1 个探测周期）
  候选逐建 latency/success 基线
  records 里 consecutiveSuperior > 0 开始出现  ← 本轮修复的核心病灶是否痊愈
    ↓ 连续 3 轮
  lastDecision = PROMOTION ← 完整闭环（对比修复前 SUPERIOR_GT0 = 0 / 9106）
```

比"换池次数有没有增加"更早要看的是 `consecutiveSuperior > 0` 是否终于出现——那正是晋升死锁是否破除的直接证据。

打开 `http://localhost:8080` 即进入「优选台」单页控制面。候选缓存会按计划刷新；是否自动重考并替换 active pool 由 `PROXY_AUTO_APPLY` 控制，用户只有三个动作：

1. **看**：在位 IP 一屏尽览——每个 IP 的实时承载连接数（呼吸数字）、殿试成绩（Mbps/延迟）、来源徽标；优选域名以「域名直连」成员并列显示（DNS 活解析、跟随上游刷新）；自动化心跳行显示供给层（手动/订阅/官方段/社区）是否活着。「设备」抽屉列出局域网连接明细——cfnat 是四层哑管道，TLS 加密后读不到 UA/Host，设备识别以局域网 IP 为准。
2. **优选**：点「立即优选」触发一轮完整终审（候选汇合 → WS 乡试 → 可选 VLESS 殿试 → 按配置换池）；或在「手动供 IP」粘贴 IP/域名/订阅内容一键入池——按 `.env` 的节点参数自动考试，手动供给登基最高优先且豁免换池冷却。手动操作与后台考试重叠时自动排队等待（最多 60 秒），不再吃「已有扫描」闭门羹。
3. **踢**：感觉卡顿时，点在位成员（IP 或域名皆可）行的「踢」按钮将其逐出——在途连接瞬断重连、剩余成员立即接管，被踢 IP 同步从候选除名，后台随即自动补考新 IP 把池补回目标规模（`PROXY_AUTO_POOL_SIZE`，建议 5~15）；最后一个成员不允许踢。

日志抽屉默认收起，需要排障时展开。除此之外没有页面、没有配置面板、没有参数表单——候选刷新、订阅慢扫在后台运行；自动换池与后台慢速调度只有在配置启用后才会执行。

### 反代 IP 维护

候选供给会自动汇合（订阅慢扫 + 官方段抽样 + 手动输入），优选域名以 `-fixed` 直连成员身份常驻转发池；所有进入 active pool 的 IP 都要通过配置的验证流程。自动换池是否启用由 `PROXY_AUTO_APPLY` 决定。需要人工干预时只有两条路：

1. **手动供 IP**：在优选台「手动供 IP」粘贴 IPv4 列表、节点链接、Clash 配置或 base64 订阅，点「优选入池」——按 `.env` 的节点参数（`PROXY_AUTO_HOST/PATH/PORT`）做 TCP/TLS 粗筛，通过者立即应用（最高优先、豁免冷却），后台调度开启时再继续复核。
2. **手动踢人**：感觉卡顿时点在位 IP 行的「踢」，被踢 IP 立即退出皇位并从候选除名。

自动池启用 VLESS probe 时，后台会用本地 sing-box 模板先请求 `generate_204`，再读取固定大小的下载响应；手动供给的粗筛仍只验 TCP/TLS 可达。只有启用自动换池和后台调度时，手动入池的线路才会在后续调度轮次中继续复核。

内置候选源：

- `cf.090227.xyz`：第三方优选目录；抓取 `/ct?ips=6`、`/cu`、`/cmcc?ips=8` API 与固定优选域名 DNS。候选源清单由服务端维护，未启用的旧社区 DNS 源不会参与刷新。

订阅转换器（`PROXY_SUBSCRIPTION_CONVERTER`，默认 `https://vlesdy.trojanjd.dpdns.org/sub`）配合第三方维护者域名（`PROXY_SUBSCRIPTION_MAINTAINERS`，默认 `owo.o00o.ooo`、`cm.soso.edu.kg`、`zrf.zrf.me`、`sub.keaeye.icu`、`sub.mot.cloudns.biz`、`sub.mia.xx.kg`、`sub.lzjbaby.com`、`sub.xdu.qzz.io`）是日常 IP 来源：服务端按 `?token=…&sub=<维护者>` 拉取节点链接，提取 IP 后慢筛入池。token 经 `PROXY_SUBSCRIPTION_TOKEN` 从 `.env` 注入，不进仓库、不回显。手动扫描的“订阅内容”粘贴同样支持，把转换后的 VLESS/Trojan/SS/VMess 或 Clash 内容贴入即可；服务端只从明确的节点 URI、JSON 或 Clash `server` 字段提取节点域名，不保存或记录原文。

服务端只允许上述固定候选源与订阅维护者清单，不接受任意 URL；第三方 HTTPS 源禁止跨域跳转，正文限制为 512 KiB，不执行脚本。手动导入的节点域名最多 64 个 hostname、共享 3 秒解析期限，并只接受公网 IPv4。所有结果仍拒绝私网、回环、链路本地与组播地址。扫描接口限制请求体为 1 MiB、并发为 `1-500`、扫描数量为 `1-10000`，同一时刻只运行一个扫描任务。

Web 服务启动时会立即刷新一次候选缓存，之后每 6 小时重新解析全部内置源。成功结果以原子替换方式写入 `/data/proxy-candidates.json`；刷新失败时保留上一次成功候选。优选台显示下次候选刷新时间，点「立即优选」可随时触发全量重考。

启用 `PROXY_AUTO_APPLY` 后，后台先用实际 `Host + WebSocket path` 对全部候选并发执行 TLS 与 WebSocket `101 Switching Protocols` 初筛。启用 `PROXY_VLESS_PROBE` 后，再启动短生命周期 sing-box，以候选 `IP:PROXY_AUTO_PORT` 覆盖模板服务器地址，通过真实 VLESS 链路请求 `generate_204`，随后读取固定大小响应并记录 Mbps。候选按 `user > subscription > official > proxy` 分层进入候选池（每层有配额，见下），层内按下载 Mbps 降序、数据面延迟升序排列。只有最终通过数量达到 `PROXY_AUTO_MIN_POOL` 且通过换池防抖三道闸（冷却/健康/同池跳过）才替换 `/data/proxy-active.json` 并重启 CFnat。模板、sing-box 或数据面失败均保留旧池。

直接运行 Web 二进制时 `PROXY_AUTO_APPLY` 默认是 `false`；仓库的 `docker-compose.yml` 为完整部署显式设为 `true`，并同时开启 VLESS probe。复制 `.env.example` 后若需要手动运行或覆盖 Compose 行为，请明确设置该变量。

后台慢速优选调度器默认开启，但只有 `PROXY_AUTO_APPLY=true` 且 GUI 开关启用时才会实际探测并替换 active pool。启动 90 秒后执行首轮，此后每 5 分钟从候选缓存轮转抽取 12 个 IP，以并发 4、单 IP 1500 ms 上限进行初筛；启用 VLESS probe 时仍须通过真实数据面终审。每轮失败保留旧池，且不会与手动扫描或六小时全量维护并发。GUI 开关会写入 `/data/proxy-optimizer.json`，容器重启后保持用户选择。

CFdata 彻底手动化：无后台自启动、无定时扫描；只在用户显式调用 `/api/cfdata/run` 时执行，结果写入 `ip.csv` 供候选汇合读取。旧的 `PROXY_CFDATA_BACKGROUND_*` / `PROXY_CFDATA_SIFT_COUNT` 变量已废弃且不再读取。

日常 IP 供给走订阅慢扫：服务端按 `PROXY_SUBSCRIPTION_MAINTAINERS`（默认 8 个第三方维护者域名）逐个请求订阅转换器 `PROXY_SUBSCRIPTION_CONVERTER + ?token=…&sub=<维护者>`（token 从 `PROXY_SUBSCRIPTION_TOKEN` 注入，不进仓库），提取明文 IP 与节点域名解析出的 IP，做并发 4、2000 ms 上限的 WS 慢筛后丢入订阅池（`subscription-pool.json`，上限 1000），默认每 `PROXY_SUBSCRIPTION_REFRESH_MINUTES=360` 分钟一轮。候选来源优先级为 `user > subscription > official > proxy`；`PROXY_USER_CANDIDATES` 与手动采用永远最高优先。

优选域名直连：启用的优选域名升格为 cfnat `-fixed` 转发成员——与登基 IP 并列轮换，拨号时按域名当前 DNS 活解析，天然跟随第三方维护者刷新；不解析成 IP 快照、不做域名级探测、不进候选池。用户显式配置的 `CFNAT_FALLBACK` 仍是主池全部拨号失败时的最后兜底。cfdata（baipiao 上游）不再供候选——订阅 + 优选域名 + 官方段 + 手动已是充分渠道；CFdata 扫描器保留为手动工具，结果不再汇入候选。

踢人自动补位：在位 IP 被踢后，后台立即从候选缓存考试补考新 IP，保序并入现有成员，把池补回 `PROXY_AUTO_POOL_SIZE` 目标（建议 5~15，默认 5）。手动路径（立即优选 / 手动入池 / 踢人）一律排队等待正在跑的考试（最多 60 秒）而非直接拒绝——「已有扫描」闭门羹不复存在。

换池防抖三道闸（防“一个劲换 IP”）：调度器晋升需连续 3 轮证明优越、上次换池 30 分钟后；连续失败 3 次才判失败；任何自动换池（scheduler / 全量终审）还受全局冷却 `PROXY_POOL_SWITCH_COOLDOWN_MINUTES=30` 约束，且现生效池全部健康（`PROXY_ACTIVE_HEALTH_WINDOW_MINUTES=60` 内无失败）时拒绝顶池；同 IP 集合直接跳过不重启 cfnat。用户手动采用不受冷却限制。

**残员豁免**：池缩到 `PROXY_AUTO_MIN_POOL`（3）以下时，冷却是“日志照打但不再拦截”——补员优先于防抖。若你连踢数次把池踢残，底层自动路径仍可换池补回，无需等冷却。注意健康闸不看池规模：**单员健康池仍会被健康闸拦住自动增员**，此时用「手动供 IP 入池」（真豁免一切闸）最快。

**报错必显**：凡是考试通过却被换池闸拦下的自动终审，一律把原因写进在位池 `error`（优选台 toast 即此字段的原文）——包括冷却拦截与快照过期。以前这两条路只写容器日志，前端却弹“优选完成”，属于吞单谎报；现已修：拦了就说拦了。

**优越判定双轨**（`superiorTo`，累计 3 轮与晋升复核走同一函数）：

- **吞吐轨**：双方都有有效吞吐成绩（`avg > 0`）→ 候选须 ≥ Active×1.25 **且** ≥ Active+80Mbps。
- **WS 轨**：任一方缺吞吐成绩（VLESS 关闭 / 测速稀疏 / Active 未测速）→ 改比 WS 延迟：改善 ≥25% **且** ≥15ms，且成功率不低于 Active；任一方没有延迟基线则本轮不判优。
- **`Mbps = 0` 的含义是"该样本没有有效吞吐成绩"，不是"实测 0 Mbps"**。平均值只对 `Mbps > 0` 的样本计算。切勿用 `0 >= 0 + 80` 形态的加法门槛比较两个零值——那会把晋升通路静默锁死（生产曾因此 9106 条记录 0 晋升）。

Active 的延迟基线来自每轮调度的轻量 WS 健康探测，故 WS 轨在 Active 上位后一个探测周期内即可开判。

**候选源配额（防单源垄断）**：刷新按优先序灌入但每层封顶——`user ≤100`、`subscription ≤600`（须为保底让位）、`official ≥150`、`proxy ≥150`（吸收余量），总池 ≤1000。`subscriptionQuota = min(600, 1000 - 已用 - official保底 - proxy保底)`，订阅层在数学上吃不光保底层。

### VLESS 探针模板

复制 `.env.example` 为 `.env`，填写与探针模板一致的 `PROXY_AUTO_HOST` 和 `PROXY_AUTO_PATH`。`.env` 已被 Git 忽略；Compose 会在缺少这两项时拒绝启动，避免误用仓库中的示例值筛选候选。

将一个 sing-box VLESS outbound 保存为 `./data/vless-probe-outbound.json`。也可以放入完整 sing-box 配置，服务端会提取第一个 `type: "vless"` 的 outbound。文件应包含真实 `uuid`、TLS `server_name`、WebSocket `path` 与 `headers.Host`；`server` 和 `server_port` 会在每次探测时替换为候选 IP 与 `PROXY_AUTO_PORT`。

`./data` 已被 Git 忽略，模板不会进入仓库。服务端不会通过 API 返回模板内容；每次生成的临时 sing-box 配置权限为 `0600`，进程退出后立即删除，错误文本会脱敏 UUID、Host、SNI 和 path。

镜像固定从 Go module tag `v1.13.18` 构建 sing-box，依赖由 Go checksum database 校验。构建使用 `CGO_ENABLED=0` 且只启用节点所需的 `with_utls` tag，以保持 scratch 镜像兼容并减少体积。上游许可文本保存在 `third_party/sing-box/LICENSE`，并复制到镜像 `/usr/share/licenses/sing-box/LICENSE`；这不代表当前仓库其余源码采用同一许可证。

## Compose 参数

`docker-compose.yml` 内的 `CFNAT_*` 环境变量会作为默认启动参数：

| 变量 | 对应 cfnat 参数 | 默认 |
|---|---|---|
| `CFNAT_ADDR` | `-addr` | `0.0.0.0:1234` |
| `CFNAT_COLO` | `-colo` | 空 |
| `CFNAT_DELAY` | `-delay` | `2000` |
| `CFNAT_DOMAIN` | `-domain` | `cloudflaremirrors.com/debian` |
| `CFNAT_FIXED_IPS` | `-fixed` | 空 |
| `CFNAT_FALLBACK` | `-fallback`（兜底目标，仅拨号失败时尝试） | 空 |
| `CFNAT_PRIORITY_IPS` | `-priority` | 空；仅兼容旧配置，不参与数据面加权 |
| `CFNAT_IPNUM` | `-ipnum` | `20` |
| `CFNAT_IPS` | `-ips` | `4` |
| `CFNAT_NUM` | `-num` | `1` |
| `CFNAT_PORT` | `-port` | `443` |
| `CFNAT_RANDOM` | `-random` | `true` |
| `CFNAT_TASK` | `-task` | `100` |
| `CFNAT_TLS` | `-tls` | `true` |
| `CFNAT_CODE` | `-code` | `200` |

启用固定转发 IP 池后，CFnat 只在新 TCP 连接建立时按顺序轮换池中的 IP；一个前端连接从拨号到关闭始终绑定同一个上游，不会在数据面并发竞速，也不会主动连接自身监听口做协议健康检查。`CFNAT_NUM` 仅表示拨号失败后的顺序回退数量，建议保持 `1`，避免 EdgeTunnel/GrainTCP 的空连接与多上游竞速干扰真实 VLESS/WS 会话。

`CFNAT_PRIORITY_IPS` 仅为兼容旧配置保留，不再在数据面重复加权；这样 active pool 才是严格 IP 轮换。候选控制面按 `user > subscription > official > proxy` 保留来源顺序（每层配额封顶），但任何来源都必须先通过真实 Host/SNI/WebSocket/VLESS 验证才可进入 active pool。

自动候选池相关变量：

| 变量 | 说明 | 默认 |
|---|---|---|
| `PROXY_AUTO_APPLY` | 是否自动 WS 终审并替换固定池 | `false` |
| `PROXY_AUTO_HOST` | 实际 VLESS/WS 节点 Host 与 TLS SNI | 空 |
| `PROXY_AUTO_PATH` | 实际 WebSocket path | `/` |
| `PROXY_AUTO_PORT` | 候选目标端口 | `443` |
| `PROXY_AUTO_CONCURRENCY` | WS 终审并发 | `20` |
| `PROXY_AUTO_MAX_LATENCY` | 单 IP 终审超时，毫秒 | `5000` |
| `PROXY_AUTO_POOL_SIZE` | 生效池目标规模（5~15 保障，踢人后自动补位回满） | `5` |
| `PROXY_AUTO_MIN_POOL` | 允许替换旧池的最少通过数量 | `3` |
| `PROXY_DOMAIN_FORWARD` | 并入 CFnat 固定转发池的优选域名，逗号分隔 | 空 |
| `PROXY_AUTO_DOMAINS` | 优选域名并入 cfnat 兜底转发（-fallback） | `false`（Compose 为 `true`） |
| `PROXY_PREFERRED_RESOLVE` / `PROXY_PREFERRED_IPS_PER_DOMAIN` / `PROXY_PREFERRED_RESOLVED_CANDIDATES` | 已废弃不再读取（优选域名直接做 `-fixed` 成员活解析） | -- |
| `PROXY_CFDATA_CANDIDATES` | 已废弃不再读取（cfdata 不再供候选） | -- |
| `PROXY_OFFICIAL_CANDIDATES` | 从 Cloudflare 官方 CIDR 均匀抽样的候选数 | `150` |
| `PROXY_USER_CANDIDATES` | 用户指定的公网 IPv4，逗号或空白分隔 | 空 |
| `PROXY_BACKGROUND_OPTIMIZER` | 是否启用低占用后台轮转优选 | `true` |
| `PROXY_SCHEDULER_APPLY` | 调度器是否允许自动换池（仍需 `PROXY_AUTO_APPLY=true`） | `true` |
| `PROXY_SCHEDULER_PROBE_INTERVAL_SECONDS` | 后台调度探测间隔，秒 | `300` |
| `PROXY_SCHEDULER_BATCH_SIZE` | 每轮调度候选数 | `12` |
| `PROXY_VLESS_PROBE` | 是否在 WS 初筛后运行真实 VLESS 数据面终审 | `false` |
| `PROXY_VLESS_TEMPLATE` | 本地 VLESS outbound 或完整 sing-box 配置 | `/data/vless-probe-outbound.json` |
| `PROXY_VLESS_TEST_URL` | 经 VLESS 请求的轻量验活地址 | `https://www.gstatic.com/generate_204` |
| `PROXY_VLESS_EXPECT_STATUS` | 数据面成功状态码 | `204` |
| `PROXY_VLESS_TIMEOUT` | 每个 VLESS probe 超时，秒 | `15` |
| `PROXY_VLESS_MAX_CANDIDATES` | 每轮最多执行真实 VLESS probe 的候选数 | `20` |
| `PROXY_VLESS_SPEED_BYTES` | 每个通过节点的下载测速字节数（仅前 6 个候选，软门槛） | `2097152` |
| `PROXY_VLESS_SPEED_TIMEOUT` | 每个下载测速超时，秒 | `20` |
| `PROXY_VLESS_MIN_DOWNLOAD_MBPS` | 下载测速软门槛，仅影响排序与展示 | `3` |
| `SING_BOX_BIN` | sing-box 二进制路径 | `/usr/local/bin/sing-box` |

### 优选域名：解析供给与直连兜底

第三方「优选域名」是优选 IP 的实时转发别名：每次新 TCP 连接时按域名当前 DNS 解析，天然跟随上游优选结果刷新。本项目中优选域名承担两个职责：

- **直连成员**：启用的优选域名升格为 cfnat `-fixed` 转发成员——与登基 IP 并列轮换，拨号时活解析（`applyPoolLocked`）。**不解析成 IP 快照、不做域名级探测、不进候选池**（C 区解析供给已拆除，`PROXY_PREFERRED_RESOLVE` 等变量已废弃不再读取）。
- **直连兜底**：设置 `PROXY_DOMAIN_FORWARD`（逗号分隔的域名，如 `youxuan.cf.090227.xyz,www.visa.cn`）；这些域名写入 CFnat `-fallback`，主池全部拨号失败时才按顺序尝试。数据面拨号时按域名当前 DNS 解析。

域名只使用内置清单与 `PROXY_DOMAIN_FORWARD` 明确配置的域名，不接受任意输入；域名目标不经域名级探测，CFnat `-fixed` 参数同时接受 IP 与域名。

## IP 列表获取方式

`cfdata.go` 与 `cfnat.go` 都是同一套取数逻辑：

1. 优先读取工作目录本地缓存：
   - `ips-v4.txt`
   - `ips-v6.txt`
   - `locations.json`
2. 如果缓存不存在，才联网下载并保存：
   - IPv4：`https://www.baipiao.eu.org/cloudflare/ips-v4`
   - IPv6：`https://www.baipiao.eu.org/cloudflare/ips-v6`
   - 数据中心位置：`https://www.baipiao.eu.org/cloudflare/locations`

所以联网正常时无需把作者包里的 IP 列表强行塞进镜像；需要离线运行时再预置到 `./data` 即可。

注意：本地 `ips-v4.txt` 当前是数千个 `/24` 的广义 CDN/合作网络候选，不等同于 Cloudflare 官方公布的 15 个 IPv4 CIDR。自动维护按配额合并用户输入、订阅池、官方 CIDR 抽样和启用的社区源（优选域名不解析供 IP、cfdata 仅手动推送）；所有候选最终都必须通过实际 Host + WebSocket path 的 `101` 初筛，并在启用 VLESS probe 时完成真实数据面终审。内置 snapshot 只解决官方列表获取失败，不绕过业务验证。

## 日志上限

Compose 已限制 Docker `json-file` 日志：

```yaml
logging:
  driver: json-file
  options:
    max-size: "10m"
    max-file: "3"
```

容器内进程日志仍会写入 `./data/cfnat.log` 与 `./data/cfdata.log`，Web 内存日志只保留最近片段用于页面展示。

## 注意

- `x-tunnel.go` 当前只作为参考，不参与镜像构建。
- `cfdata` 已补 `-auto` / `-no-wait` / `-dc` 等参数；Web 控制台不再依赖交互式 stdin。
- 如果手动填写数据中心，必须是 `ip.csv` 里已经存在的机房代号；留空表示提取全部。
