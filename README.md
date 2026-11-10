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

打开 `http://localhost:8080` 即进入「优选台」单页控制面。候选缓存会按计划刷新；是否自动重考并替换 active pool 由 `PROXY_AUTO_APPLY` 控制，用户有四个动作：

1. **看**：在位 IP 一屏尽览——每个 IP 的实时承载连接数（呼吸数字）、殿试成绩（Mbps/延迟）、来源徽标；优选域名以「域名直连」成员并列显示（DNS 活解析、跟随上游刷新）；自动化心跳行显示供给层（手动/订阅/官方段/社区）是否活着。「设备」抽屉列出局域网连接明细——cfnat 是四层哑管道，TLS 加密后读不到 UA/Host，设备识别以局域网 IP 为准。
2. **当前测试**：点「当前测试」先**暂停转发**，把此刻正在承载流量的 IP 与域名逐个拉到真实数据面上验一遍（IP 走 WS 101 握手；域名先 TCP:443，再跑 VLESS 短时下载），不合格的当场从池里剔除，然后恢复转发。**全部不合格时不动池**——全灭说明验证通路本身坏了，把池清空是最糟的选择。一轮最长约四分钟（域名要跑下载），接口立刻返回 202，页面轮询进度。
3. **立即优选**：点「立即优选」先刷新候选缓存（社区源/订阅/手动供给汇合），再跑三层漏斗（TCP/TLS 初筛 → WS 101 短名单 → VLESS 终审），通过的候选按质量替换 active pool。这是 2026-11-05 被误删、本轮恢复的能力；它与「当前测试」是两个问题——一个验「占着位置的还通不通」，一个找「外面有没有更好的」。手动触发豁免 30 分钟换池冷却（与手动供 IP、扶持登基同一口径），但「只转固定的」池锁仍然生效——锁着时直接报错说明原因，不去跑一轮注定被丢弃的漏斗。同样异步 202 + 轮询，报告写明池前后数量、是否真的换池，以及没落地时的原因。
   入池仍是另一条路：在「考生供给」粘贴 IP/域名/订阅内容一键入池——按 `.env` 的节点参数自动考试，手动供给登基最高优先且豁免换池冷却。手动操作与后台考试重叠时自动排队等待（最多 60 秒），不再吃「已有扫描」闭门羹。
4. **踢**：感觉卡顿时，点在位成员（IP 或域名皆可）行的「踢」按钮将其逐出——在途连接瞬断重连、剩余成员立即接管，被踢 IP 同步从候选除名，后台随即自动补考新 IP 把池补回目标规模（`PROXY_AUTO_POOL_SIZE`，建议 5~15）；最后一个成员不允许踢。

「当前测试」与「立即优选」互斥：一个在跑时点另一个会得到 409，两个按钮同时置灰。它们都要占探测预算、都要动 cfnat，同时跑只会互相把对方拖成超时。

日志抽屉默认收起，需要排障时展开。「订阅」区常驻，可增删维护者、按状态筛候选、逐条删除、分别触发「立即去收」与「立即考试」。除此之外没有参数表单——候选刷新、订阅采集/考试在后台按各自节奏运行；自动换池与后台慢速调度只有在配置启用后才会执行。

### 反代 IP 维护

候选供给会自动汇合（订阅本子里考过的 + cfdata 自扫 + 手动输入），优选域名以 `-fixed` 直连成员身份常驻转发池；所有进入 active pool 的 IP 都要通过配置的验证流程。自动换池是否启用由 `PROXY_AUTO_APPLY` 决定。需要人工干预时有三条路：

1. **手动供 IP**：在优选台「手动供 IP」粘贴 IPv4 列表、节点链接、Clash 配置或 base64 订阅，点「优选入池」——按 `.env` 的节点参数（`PROXY_AUTO_HOST/PATH/PORT`）做 **WS 101 体检**，握手延迟低于进池上限（`PROXY_POOL_MAX_ENTRY_LATENCY_MS`，默认 800ms）才入池，入池数量按 `PROXY_AUTO_POOL_SIZE` 封顶并按片区轮转取样；通过者立即应用（最高优先、豁免冷却），后台调度开启时再继续复核。
2. **手动踢人**：感觉卡顿时点在位 IP 行的「踢」，被踢 IP 立即退出皇位并从候选除名。
3. **订阅页维护**：从主页顶栏「订阅」按钮进入独立页 `/subscription`，增删维护者、停用拉不到的、删掉本子里的死地址，或单独触发「立即去收」/「立即考试」看这两段各自的实际情况。

自动池启用 VLESS probe 时，后台会用本地 sing-box 模板先请求 `generate_204`，再读取固定大小的下载响应。手动供给与自动换池现在走同一道入池闸（WS 101 + 进池延迟上限），不再出现「手动只验 TCP/TLS、进池后连 WS 都敲不开」的情况。只有启用自动换池和后台调度时，手动入池的线路才会在后续调度轮次中继续复核。

内置候选源：

- `cf.090227.xyz`：第三方优选目录；抓取 `/ct?ips=6`、`/cu`、`/cmcc?ips=8` API 与固定优选域名 DNS。候选源清单由服务端维护，未启用的旧社区 DNS 源不会参与刷新。

订阅转换器（`PROXY_SUBSCRIPTION_CONVERTER`，默认 `https://vlesdy.trojanjd.dpdns.org/sub`）配合第三方维护者域名（`PROXY_SUBSCRIPTION_MAINTAINERS`，默认 `owo.o00o.ooo`、`cm.soso.edu.kg`、`zrf.zrf.me`、`sub.keaeye.icu`、`sub.mot.cloudns.biz`、`sub.mia.xx.kg`、`sub.lzjbaby.com`、`sub.xdu.qzz.io`）是日常 IP 来源：服务端按 `?token=…&sub=<维护者>` 拉取节点链接，提取 IP 后慢筛入池。token 经 `PROXY_SUBSCRIPTION_TOKEN` 从 `.env` 注入，不进仓库、不回显。手动扫描的“订阅内容”粘贴同样支持，把转换后的 VLESS/Trojan/SS/VMess 或 Clash 内容贴入即可；服务端只从明确的节点 URI、JSON 或 Clash `server` 字段提取节点域名，不保存或记录原文。

服务端只允许上述固定候选源与订阅维护者清单，不接受任意 URL；第三方 HTTPS 源禁止跨域跳转，正文限制为 512 KiB，不执行脚本。手动导入的节点域名最多 64 个 hostname、共享 3 秒解析期限，并只接受公网 IPv4。所有结果仍拒绝私网、回环、链路本地与组播地址。扫描接口限制请求体为 1 MiB、并发为 `1-500`、扫描数量为 `1-10000`，同一时刻只运行一个扫描任务。

Web 服务启动时会立即刷新一次候选缓存，之后每 6 小时重新解析全部内置源。成功结果以原子替换方式写入 `/data/proxy-candidates.json`；刷新失败时保留上一次成功候选。优选台显示下次候选刷新时间。**「立即优选」会先刷新候选再开考**；只想体检在位成员、不动池，用「当前测试」；只想刷新候选、不跑漏斗，走 `/api/cfnat/proxy-candidates` 的 POST 或等定时刷新。

启用 `PROXY_AUTO_APPLY` 后，后台先用实际 `Host + WebSocket path` 对全部候选并发执行 TLS 与 WebSocket `101 Switching Protocols` 初筛。启用 `PROXY_VLESS_PROBE` 后，再启动短生命周期 sing-box，以候选 `IP:PROXY_AUTO_PORT` 覆盖模板服务器地址，通过真实 VLESS 链路请求 `generate_204`，随后读取固定大小响应并记录 Mbps。候选按 `user > cfdata > subscription > bestcf` 分层进入候选池（每层有配额，见下），层内按下载 Mbps 降序、数据面延迟升序排列。只有最终通过数量达到 `PROXY_AUTO_MIN_POOL` 且通过换池防抖三道闸（冷却/健康/同池跳过）才替换 `/data/proxy-active.json` 并重启 CFnat。模板、sing-box 或数据面失败均保留旧池。

直接运行 Web 二进制时 `PROXY_AUTO_APPLY` 默认是 `false`；仓库的 `docker-compose.yml` 为完整部署显式设为 `true`，并同时开启 VLESS probe。复制 `.env.example` 后若需要手动运行或覆盖 Compose 行为，请明确设置该变量。

后台慢速优选调度器默认开启，但只有 `PROXY_AUTO_APPLY=true` 且 GUI 开关启用时才会实际探测并替换 active pool。启动 90 秒后执行首轮，此后每 5 分钟从候选缓存轮转抽取 12 个 IP，以并发 4、单 IP 1500 ms 上限进行初筛；启用 VLESS probe 时仍须通过真实数据面终审。每轮失败保留旧池，且不会与手动扫描或六小时全量维护并发。GUI 开关会写入 `/data/proxy-optimizer.json`，容器重启后保持用户选择。

CFdata 供给层：扫描器在启动时后台自扫一次，此后每 6 小时一轮（`CFDATA_AUTO_SCAN`，默认开启），结果写入 `result.csv`，由 `cfdataCandidateIPs()` 读入候选汇合，只要求公网 IPv4（2026-11-05 起不再过 CF 官方段闸，见下文「官方段降级为标签」）；设为 `false` 即退回纯手动（只在 `/api/cfdata/run` 时跑）。旧的 `PROXY_CFDATA_BACKGROUND_*` / `PROXY_CFDATA_SIFT_COUNT` 变量已废弃且不再读取。

日常 IP 供给走订阅**采集 / 考试分离**：

- **收（harvest）**：按维护者清单逐个请求订阅转换器 `PROXY_SUBSCRIPTION_CONVERTER + ?token=…&sub=<维护者>`（token 从 `PROXY_SUBSCRIPTION_TOKEN` 注入，不进仓库），提取明文 IP 与节点域名解析出的 IP，**只记账、不探测**，全部写进本子 `subscription-inventory.json`。默认每 `PROXY_SUBSCRIPTION_REFRESH_MINUTES=360` 分钟一轮。连续失败的维护者进入退避（30 分钟起，翻倍至 6 小时封顶），全部都在退避时整体放行——退避不能变成永久拉黑。
- **考（probe）**：从本子里挑候选做真实考试，默认每 `PROXY_SUBSCRIPTION_PROBE_MINUTES=10` 分钟一批、每批 `PROXY_SUBSCRIPTION_PROBE_BATCH=400` 个。两段式：先 WS 101 粗筛（并发 4、每批 240 秒预算），再对握手通过者跑 sing-box 真实数据面（并发 2、单条 12 秒、每轮最多 120 条）。模板不可用时自动降级为「仅握手级结论」并在页面异常栏写明原因。

本子里每条候选带状态：`pending`（收了还没考）、`pass`（考过能用）、`fail`（连续 3 次考不过）。**只有 `pass` 会进候选汇合**。单次失败不判死，与在位验活同一套阈值。70 小时内既没被任何维护者再供出、又连续失败到 6 次的条目才会被淘汰；超容（6000）时先削 `fail`，`pass` 最后动。候选来源优先级为 `user > cfdata > subscription > bestcf`；`PROXY_USER_CANDIDATES` 与手动采用永远最高优先。

**订阅页（`/subscription`）**：主页顶栏「订阅」按钮进入，与主控台分页，两页同源共用一份 `app.js`。维护者清单可在页面上增删、停用/启用，不必改 compose 再重启；每次拉取的时间、耗时、HTTP 状态、提取数量、失败原因都逐条落盘并按维护者保留最近 20 次；本子可按状态筛选、逐条删除。「立即去收」与「立即考试」是两个独立按钮——排障时经常只想知道「到底收不进来」还是「收进来了但考不过」，合成一个按钮就永远分不清。

分页的理由是数据量：本子上限 6000 条，`/api/subscriptions/inventory` 一次就是全量 JSON。主页每 3 秒轮询一次状态，没必要顺手把整本候选拖着走；拆页后主页完全不拉本子，订阅页 15 秒才拉一次。

**官方段降级为标签（2026-11-05）**：CF 官方 15 段不再作为准入门。线上实测推翻了原假设——在位池 45 个成员里 30 个不在官方段内，它们正在正常转发，延迟与官方段中位数相差不到 30ms（813ms 对 788ms），健康比例也一致。反代 / ProxyIP 地址可以正常承载流量，「像不像 Cloudflare 自己的地址」与「能不能把流量送到 Cloudflare」并不等价，后者该由端到端考试判定。官方段改为用于段多样性分组与页面展示：段内返回官方 CIDR，段外公网地址按 `/16` 归组，**不能返回空串**——否则这些成员会从片区统计与单点保护里整体隐形，而所有指标照常显示正常。

优选域名直连：启用的优选域名升格为 cfnat `-fixed` 转发成员——与登基 IP 并列轮换，拨号时按域名当前 DNS 活解析，天然跟随第三方维护者刷新；不解析成 IP 快照、不做域名级探测、不进候选池。用户显式配置的 `CFNAT_FALLBACK` 仍是主池全部拨号失败时的最后兜底。cfdata 是**第二大供给层**——它以本机网络为基准实测延迟与丢包，比社区表更贴近这条线路的真实状况。

踢人自动补位：在位 IP 被踢后，后台立即从候选缓存考试补考新 IP，保序并入现有成员，把池补回 `PROXY_AUTO_POOL_SIZE` 目标（建议 5~15，默认 5）。手动入池路径排队等待正在跑的考试（最多 60 秒）而非直接拒绝——「已有扫描」闭门羹不复存在。**「当前测试」与「踢人」是两个例外**：前者全程持 `cfnatCtlMu`（验证期间不许任何人换池），抢不到锁就回「已有优选或验证在运行」，不排队——转发停着的每一秒都是实打实的断网；后者用 `proxyScanMu.TryLock` 直接回 409，因为漏斗用的是开考前的候选快照，放行踢人只会让被踢的 IP 在几分钟后落池时复活。「立即优选」的漏斗阶段持 `proxyScanMu`，只在最后落池时短暂拿 `cfnatCtlMu`；入口与「当前测试」互斥，不会出现「一边暂停转发验在位、一边换池」的竞争。

换池防抖三道闸（防“一个劲换 IP”）：调度器晋升需连续 3 轮证明优越、上次换池 30 分钟后；连续失败 3 次才判失败；任何自动换池（scheduler / 全量终审）还受全局冷却 `PROXY_POOL_SWITCH_COOLDOWN_MINUTES=30` 约束，且现生效池全部健康（`PROXY_ACTIVE_HEALTH_WINDOW_MINUTES=60` 内无失败）时拒绝顶池；同 IP 集合直接跳过不重启 cfnat。用户手动采用不受冷却限制。

**冷却只认自动换池**：`/api/status` 的 `poolGuard` 现在区分 `lastSwitchAt`（任何池变更）与 `lastAutoSwitchAt`（自动换池真正执行的时间）。冷却只认后者——在位验活剔人、扶持登基、手动踢人属于维修动作，不会把 30 分钟重新拨满。旧实现共用一个时间戳，生产实测 6 小时里维修动作把冷却反复顶满，自动换池被挡回 21 次、0 次成功，系统退化成「只能靠人工粘 IP」。用户显式动作（手动供给、扶持）仍会重置自动窗口，避免刚点完就被后台顶掉。

**残员豁免**：池缩到 `PROXY_AUTO_MIN_POOL`（3）以下时，冷却是“日志照打但不再拦截”——补员优先于防抖。若你连踢数次把池踢残，底层自动路径仍可换池补回，无需等冷却。注意健康闸不看池规模：**单员健康池仍会被健康闸拦住自动增员**，此时用「手动供 IP 入池」（真豁免一切闸）最快。

**救援快车道（整池判死）**：现生效池的**每个**成员都达到在位验活的连续失败阈值（`PROXY_ACTIVE_PROBE_FAIL_THRESHOLD`，默认 3）时，换池闸整体让路——冷却与健康闸都不再拦。理由是这两道闸防的是"瞎换"，而整池已死时不换就是断网。判定数据源是在位验活账本（`activeProbe.members[*].consecutiveFailures`），不是调度器成绩册：成绩册按"单条线路"建模，对多成员池答不上来。

时间预期要说清楚：按默认每 10 分钟一次验活、阈值 3 次，从第一次失败到触发救援最长约 **3 个探测周期（≈30 分钟）**，不是用户感知上的"立刻"。要更快只能让数据面拨号失败直接反馈进控制面，那是另一条独立的改造，本轮不做。

救援放行只代表"允许换"，不代表"有得换"。如果候选池里没有合格替补，决策层返回的仍是不换——此时 `/api/status` 的 `qualityScheduler.lastError` 会写明"整池 N 个成员均已判死，但候选池没有合格替补"，容器日志同时打 `scheduler rescue stalled`。这种情况需要人工补 IP 或等供给层恢复，系统不会假装自救成功。

**片区多样性（调度器也受约束）**：`spreadBySegment` 原先只在全量重挑与踢人补位时生效，调度器单独换一个成员完全绕过片区检查——池结构可以被一轮一轮换成同一段，而所有指标都显示正常。现在调度器换人前会比对换入前后的段数，**段数变少即拒绝**（`degradedSegmentDiversity`）。代价是极少数情况下晋升被推迟一轮；收益是池不会在无人察觉时退化成单段。

**片区降权（软性，不淘汰）**：每轮探测结果会按 CF 段汇总失败率。某段样本 ≥5 且失败率 ≥80% 时，该段的候选在终审名单里被稳定移到末尾——候选不够时照样会被采用，只是排在后面。判定是单轮滚动，片区恢复后下一轮自动解除，无需人工重置。失败原因同时被归类为 `timeout / refused / unreachable / handshake / protocol / other`（`proxyScanResult.errorClass`），便于事后判断"整片被掐"还是"个别地址没了"。

**成绩册单 ACTIVE 不变量**：成绩册里同一时刻只应有一条 `ACTIVE`。历史上会漏：无 Active 时的晋升（`promoted_without_active`）其 `FromIP` 为空，执行层降不到任何记录，旧的 `ACTIVE` 就永久留在账上（线上曾见两条并存，其中一条 `lastProbeAt` 为零值）。现在 `seedActive` 与 `commitSwitch` 都会把除当前 Active 之外的所有 `ACTIVE` 降为 `STANDBY`，原因记为 `orphan_active_demoted`；成绩册载入时也会对一次账（`ActiveIP` 为空但只有一条 `ACTIVE` 时认它作代表，多条则全部降级）。这一条不只是整洁问题——墓碑清理永不淘汰 `ACTIVE`，幽灵记录会一直占着名额。

**报错必显**：凡是考试通过却被换池闸拦下的自动终审，一律把原因写进在位池 `error`（优选台 toast 即此字段的原文）——包括冷却拦截与快照过期。以前这两条路只写容器日志，前端却弹"优选完成"，属于吞单谎报；现已修：拦了就说拦了。`error` 现在同时带 `errorAt` 时间戳，`/api/status` 另有结构化的 `poolBlock`（`blocked / reason / source / at / nextAllowedAt`），换池成功后自动清空。

**快照有效性判据**：终审期间候选被刷新这件事，本身不该作废一轮考试。这条踩过两次坑，值得记清楚：

1. 初版判据只比快照时间戳，一次无害的刷新就能吞掉整轮 90 秒考试——线上 2026-11-04 12:18 与 12:21 两轮各有 20 个候选通过 VLESS 终审，全被丢掉。
2. 改成「通关 IP 是否仍在候选名单里」后，12:38 又丢一轮（passed=10）。根因这时才看清：**候选池不是稳定名册，是按层配额灌入、上限 1000 的轮转采样队列**，每轮重新排序；GitHub raw 六个源超时一次，整池就从 743 掉到 650，同一个 IP 这轮在榜、下轮掉出榜是常态。拿供给队列的抖动去否定一个端到端验过的结论，属于本末倒置。

现在只守一条与候选名单无关、可就地复验的不变量：**进池的必须是公网 IPv4 且属于 Cloudflare 边缘段**（`filterPoolAdmissibleIPs`，与刷新时那道段闸同一把尺子）。个别地址复验不过就逐个剔除，剩下的照常采用，不因一两个失效地址作废整轮。

**优越判定双轨**（`superiorTo`，累计 3 轮与晋升复核走同一函数）：

- **吞吐轨**：双方都有有效吞吐成绩（`avg > 0`）→ 候选须 ≥ Active×1.25 **且** ≥ Active+80Mbps。
- **WS 轨**：任一方缺吞吐成绩（VLESS 关闭 / 测速稀疏 / Active 未测速）→ 改比 WS 延迟：改善 ≥25% **且** ≥15ms，且成功率不低于 Active；任一方没有延迟基线则本轮不判优。
- **`Mbps = 0` 的含义是"该样本没有有效吞吐成绩"，不是"实测 0 Mbps"**。平均值只对 `Mbps > 0` 的样本计算。切勿用 `0 >= 0 + 80` 形态的加法门槛比较两个零值——那会把晋升通路静默锁死（生产曾因此 9106 条记录 0 晋升）。
- **测速超时不再归零**（v2）：时间预算耗尽但已读满 64 KiB 的候选按实读字节得**诚实低速分**（EWMA 平滑，突发尖峰被衰减）——`800KB/20s` 的慢 IP 现在拿 ~0.3 Mbps 而非 0 分，吞吐轨不再对慢线全盲；真跑不动的依旧 0 分走 WS 轨。

**机房感知（colo）**：WS 握手响应的 `CF-RAY` 头被解析出三字机房码（如 HKG），随成绩单存档并在优选台徽标显示。`PROXY_POOL_COLO_PREFER=HKG,SJC` 启用**软偏好**：终审/补位选才时偏好机房排前、区内保序——充裕时整池收敛到偏好机房，匮乏时其余机房照常补位；只重排不淘汰，永不因偏好缩短池子。调度器晋升（superiorTo）保持纯质量判定不参与偏好。

Active 的延迟基线来自每轮调度的轻量 WS 健康探测，故 WS 轨在 Active 上位后一个探测周期内即可开判。入池体检为三次握手取最优，失败次数一并记档（`3 测 2 失` 在体检注记可见）——丢手即质量，股神/CFST 多发采样思想的正统继承。

**候选源配额（防单源垄断）**：刷新按优先序灌入但每层封顶——`user ≤100`、`cfdata ≤250`、`subscription ≤300`、`bestcf ≤350`（吸收余量），总池 ≤1000。前三层配额之和 650，bestcf 吃剩余 350；灌入按优先序进行且每层封顶，故任何一层都吃不光其他层。官方 Cloudflare CIDR 采样层已下线（`PROXY_OFFICIAL_CANDIDATES` 不再读取）。

**渠道健康账**：`/api/status` 的 `sources` 节逐源记录抓取次数、成功次数、累计 IP 数与最近一次成败——bestcf 的每个 URL、订阅的每个维护者、user/域名表各一席。哪个源死了，一眼可辨；渠道审计以此为准。

**渠道汇总（优选台环形图）**：`/api/status` 的 `sourceChannels` 把逐源账按 `user/cfdata/subscription/bestcf` 四层归并，给出每层当前候选数、在位数、验活分档（活/慢/败）、抓取成功率与活性分（0.7×在位存活率 + 0.3×抓取成功率，只用于展示排序）。环形图只画 **IP 在位份额**，优选域名单列一行说明——域名全部来自 BestCF 域名表，混进环里会让 BestCF 恒占大半，反而看不出 IP 供给的真实结构。图例按活性分排序，直接标出份额百分比——逐源表回答「哪个 URL 挂了」，渠道汇总回答「哪一层最近在真正供血」。渠道归类规则只放在后端 `sourceChannelOf()`，前端不再各猜一套。

成员来源以**入池时**记下的 `memberChannels` 为准，不拿当前候选快照反推：候选池每 6 小时整体换一份，而健康池会长期保留旧成员；若按「现在还在不在候选名单」判断，旧成员会被整批误记为「手动」。映射在 `applyProxyPool()` 这一个提交点结算，所有换池入口共用。

**测速通路自检**：`/api/status` 的 `speedPath` 记录每轮域名体检中有多少域名真的测出了速度。连续 3 轮「确实走到测速阶段、但零成绩」会打一条 `domain speed path suspect` 日志并在页面上标出——2026-10-20 那次 `speed.cloudflare.com` 经隧道恒零的事故，此前只会表现为「测速功能悄悄失效」，现在能被看见。模板降级成 `tcp-only` 的轮次不算端点故障；中间夹一轮 TCP 全灭会重新计数，避免「连续 N 轮」失实。页面分四种状态显示：无域名可测、域名全部 TCP 不通、连续 N 轮零成绩、正常。

同一道保险也补进了稳定性账：整轮零成绩时，失败样本不再进入 `domain-quality`。此前只挡住「连败计数」，成功率仍会被拉低，端点坏三轮足以把好域名判成 `unstable` 并踢出转发——告警还没响，域名池先被自己砍掉。

这里刻意不自动切换测速端点：2026-11-08 在 NAS 上实测，候选镜像（Hetzner/Bouygues）直连皆不可达，换成它们只会把「零成绩」换个理由继续。

**扫描节流诊断**：初筛（TCP/TLS RTT）并发默认 100（`PROXY_AUTO_PRESCREEN_CONCURRENCY`）。若连续几轮考试通过数**递减**、停一阵又自愈，多半是被运营商/CF 判为网络扫描触发临时限流（CFST 社区实证经验）——把并发回调（如 50），等几轮恢复后再逐步升回。

### VLESS 探针模板

复制 `.env.example` 为 `.env`，填写与探针模板一致的 `PROXY_AUTO_HOST` 和 `PROXY_AUTO_PATH`。`.env` 已被 Git 忽略；Compose 会在缺少这两项时拒绝启动，避免误用仓库中的示例值筛选候选。

将一个 sing-box VLESS outbound 保存为 `./data/vless-probe-outbound.json`。也可以放入完整 sing-box 配置，服务端会提取第一个 `type: "vless"` 的 outbound。文件应包含真实 `uuid`、TLS `server_name`、WebSocket `path` 与 `headers.Host`；`server` 和 `server_port` 会在每次探测时替换为候选 IP 与 `PROXY_AUTO_PORT`。

`./data` 已被 Git 忽略，模板不会进入仓库。服务端不会通过 API 返回模板内容——文件接口（`/api/files`、`/api/file/`）对模板基名（含自定义 `PROXY_VLESS_TEMPLATE`）与 sing-box 探针临时配置残留一律拒收；每次生成的临时 sing-box 配置权限为 `0600`，进程退出后立即删除，错误文本会脱敏 UUID、Host、SNI 和 path。

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
| `CFNAT_NUM` | `-num` | `3`（钳制区间 1–8） |
| `CFNAT_PORT` | `-port` | `443` |
| `CFNAT_RANDOM` | `-random` | `true` |
| `CFNAT_TASK` | `-task` | `100` |
| `CFNAT_TLS` | `-tls` | `true` |
| `CFNAT_CODE` | `-code` | `200` |

启用固定转发 IP 池后，CFnat 只在新 TCP 连接建立时按顺序轮换池中的 IP；一个前端连接从拨号到关闭始终绑定同一个上游，不会在数据面并发竞速，也不会主动连接自身监听口做协议健康检查。

`CFNAT_NUM` 是**拨号失败后的顺序回退深度**，不是并发数。它的语义是：第一个目标 2 秒内拨不通，就顺着回退顺序试下一个，直到连上或用完深度。任何时刻都只有一个上游连接在建立，所以「一次前端连接只产生一个上游会话」这条约束没有被破坏。默认 `3`，钳制在 1–8。

### 设备粘性（`CFNAT_STICKY`）

这个开关决定「同一个局域网里有几台设备在上网」这件事，cfnat 该怎么分。默认 `true`，语义是**设备之间错开、设备自己稳定**：

- 一台设备的全部连接钉在同一个出口上。视频不断流、会话不漂移。
- 不同设备按到达顺序各占一个出口，出口数 ≥ 设备数时**绝不撞车**。这是「均衡」的判据——不是「碰运气没撞上」，而是「结构上不允许撞」。
- 换池时设备不跳线：钉位存的是出口地址，只要这个出口还在新池里就继续用。补位、剔除、重排都不会把正在看视频的设备晃下来；只有它的出口真的被剔除了才重分配。
- 钉住的出口拨不通、按回退顺序落到别的成员并拨通时，钉位会跟着改到那个真通的出口。不改的话，这台设备的下一条连接会再去撞同一堵墙，表现为「这台设备一直卡，别的设备正常」。

设成 `false` 就退回「按连接轮转铺开全部成员」。这条路的问题不在公平，在于**每建立一条连接就换一次出口**：同一台设备的不同请求散到不同上游，视频卡、登录态来回跳，用户看到的现象就是「乱七八糟」。

设备与出口的对应关系在页面上直接可见：壹区「在途转发目标」按设备逐行列出「`192.168.1.11` 转发目标 `x.x.x.x`」，陆区「设备名册」每台设备多一列同名信息，鼠标悬停可看近 N 次的出口分布。数据来自 cfnat 日志里「选择转发连接」那一行——四层转发没有别的办法把「哪台设备」和「哪个出口」连起来，/proc 里只有两段互不相干的 socket。

### 谁在转：在效成员标注

粘性分发下，池里十几个成员中通常只有「设备数」那么几个真的承载流量，其余全是待命。壹区每一位成员都带一个徽标，回答「此刻流量落在谁身上」：

- **在转**：cfnat 到该出口有活着的出站连接（`/proc` 实况，最强证据），整行加亮。
- **近用**：最近一段日志里实际拨通过、当前没有活连接（设备断线或刚切走的余温）。
- 没有徽标：仍在位，但不承载流量——待命、被用户关掉、或已被判死。

成绩栏读的是**在位验活**那份账（`PROXY_ACTIVE_PROBE_*`），而不是质量调度器：后者只测候选、从不碰在位成员，快照里的延迟可能是几轮前的旧账。连败的成员直接显示「连败 N，满 M 次下台」，不用等它真的被踢走才知道出了问题。

### 固定转发（指定 IP / 域名）

壹区多了一行「固定转发」输入框，可以填一个 IP 或域名，按回车或点「固定」。语义是**用户担保**，与 `PROXY_DOMAIN_FORWARD` 同级：

- 不在位 → 走「扶持登基」通道，不体检，直接并入生效池并置顶，转发立刻用上。
- 已在位但被关掉 → 顺手勾回来。只写置顶是不够的：`planFixedMembers` 先看关闭集合，被关的成员永远不会进 `-fixed`。
- 已在位且已置顶 → 幂等跳过，不白重启一次转发。

行内的「固定」按钮做的是同一件事的另一半：把已在池里的成员排到转发最前。再勾上「只转固定的」（原「只转扶持登基的」），池里其余成员就全部停转——这是「劣质成员分走流量」由用户亲手拍板的总阀门。

固定成员**不过域名质量闸**。此前扶持登基收下的域名若体检判坏，会被 `admittedPoolDomains` 静默过滤掉，而接口仍报「已登基」；同理 `reconcilePoolDomains` 每轮体检后会按「配置 + 自动候选」重算域名集合，用户固定的域名会被抹掉。两处都已改为保留用户担保。

调大它的理由来自 2026-10-21 的事故：上游链路中断约 3 小时，池里混入 0%–30% 成功率的成员，而 `-num 1` 让「一次拨号超时」直接等于「丢连接」，用户侧表现为整站打不开。深度 1 等于没有任何容错，3 才是能扛住单个死目标的最小值。显式设成 `1` 仍被尊重。

`CFNAT_PRIORITY_IPS` 仅为兼容旧配置保留，不再在数据面重复加权；这样 active pool 才是严格 IP 轮换。候选控制面按 `user > cfdata > subscription > bestcf` 保留来源顺序（每层配额封顶），但任何来源都必须先通过真实 Host/SNI/WebSocket/VLESS 验证才可进入 active pool。

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
| `PROXY_AUTO_PRESCREEN_CONCURRENCY` | 三层漏斗初筛（TCP/TLS RTT）并发；过高会触发扫描限流（见诊断指引） | `100` |
| `PROXY_AUTO_SHORTLIST` | 初筛短名单容量，按延迟升序取前 N 进精筛 | `60` |
| `PROXY_DOMAIN_FORWARD` | 并入 CFnat 固定转发池的优选域名，逗号分隔 | 空 |
| `PROXY_AUTO_DOMAINS` | 启用的优选域名并入 cfnat 固定转发（`-fixed`），不是 `-fallback` | `false`（Compose 为 `true`） |
| `PROXY_PREFERRED_RESOLVE` / `PROXY_PREFERRED_IPS_PER_DOMAIN` / `PROXY_PREFERRED_RESOLVED_CANDIDATES` | 已废弃不再读取（优选域名直接做 `-fixed` 成员活解析） | -- |
| `PROXY_CFDATA_CANDIDATES` | 已废弃不再读取（cfdata 走 `result.csv` 供给层） | -- |
| `PROXY_OFFICIAL_CANDIDATES` | 已废弃不再读取（官方 CIDR 采样层已下线，内置 snapshot 与采样器已随死码清除） | -- |
| `PROXY_USER_CANDIDATES` | 用户指定的公网 IPv4，逗号或空白分隔 | 空 |
| `PROXY_BACKGROUND_OPTIMIZER` | 是否启用低占用后台轮转优选 | `true` |
| `PROXY_SCHEDULER_APPLY` | 调度器是否允许自动换池（仍需 `PROXY_AUTO_APPLY=true`） | `true` |
| `PROXY_SCHEDULER_PROBE_INTERVAL_SECONDS` | 后台调度探测间隔，秒 | `300` |
| `PROXY_SCHEDULER_BATCH_SIZE` | 每轮调度候选数 | `12` |
| `PROXY_ACTIVE_PROBE_ENABLED` | 在位验活总闸：定时复检生效池每个 IP，坏者请下台（独立于后台优化器） | `true` |
| `PROXY_ACTIVE_PROBE_INTERVAL_SECONDS` | 在位验活间隔，秒（钳制 `[300,1800]`＝5~30 分，越界回落 `600`） | `600` |
| `PROXY_ACTIVE_PROBE_FAIL_THRESHOLD` | 连续失败几次才请下台（钳制 `[1,10]`，越界回落 `3`；单次抖动不换人） | `3` |
| `PROXY_ACTIVE_PROBE_SLOW_MS` | 握手通了但超过它算「慢」，连续两轮即换人（钳制 `[300,5000]`；详见「劣质成员怎么被换掉」） | `1500` |
| `PROXY_POOL_COLO_PREFER` | 机房软偏好（IATA 码，如 `HKG,SJC`）：充裕取偏好、匮乏放行，只重排不拒收 | 空（禁用） |
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

### 劣质成员怎么被换掉

池子里会混进两类坏成员：**死了的**（握手不通）和**慢的**（握手通、但要一秒多）。以前只有前者有处置通道，而且要等满 3 个 10 分钟周期（≈30 分钟）；后者根本没有通道——只要握手通着，1600ms、1800ms 的成员可以一直占着位子，粘性分发还会把某台设备整条钉在它上面。

现在的流程（`web/throne_probe.go`）：

1. **每轮摸一遍在位成员**（IP + 域名一起摸），记下连败数与连续慢轮数。
2. **出现异常但还没到动手阈值** → 排一次快速复检，2 分钟后只对着这批人再摸一遍，常态 10 分钟节拍不受影响。死/慢的处置时间从 ≈30 分钟压到 ≈12–14 分钟。**域名只按连败排复检**：判慢没有处置通道，给慢域名排复检等于每 2 分钟白摸一次，还会把主节拍长期拽到高频档。
3. **到阈值就换人**：从候选池里挑 WS 通、延迟低于进池上限（`PROXY_POOL_MAX_ENTRY_LATENCY_MS`，默认 800ms）的替补，优先选换完不减少片区多样性的。一轮里所有该换的一次换完，只重启一次 cfnat。**判慢只换 IP**：域名没有「就地换人」这条路，换域名等于换入口身份；而域名的 DNS 本来就会自己换边缘，一轮慢多半是瞬时的。域名只保留「判死下线」这一条硬判据，否则一次整体变慢就能把域名池削掉一层。
4. **换不掉就暂留**：候选池里没有合格替补时，宁可留着一个病号也不让池子缩水（5→4→3 会更容易触发「整组已死」）。页面成绩栏标「暂留」并写明理由。**注意区分「没替补」和「没轮到」**：如果替补考试那一刻扫描锁正被全量重挑或手动扫描占着，我们根本没看候选池，这时不记暂留，只给这几个 IP 排快速复检，2 分钟后再考——把「资源被占」写成「没有替补」是给用户一个错误结论。
5. **域名判死就下线**，并进 30 分钟观察期：期内 `reconcilePoolDomains` 不会把它自动加回来，到期自动放行再考一次。`PROXY_DOMAIN_FORWARD` 显式配置不受此限。
6. **被用户关闭的成员不归这条链管**：它们仍在池里、仍占一个位置，但不进 `-fixed`、不参与救援/人手不足/健康闸的判定，也不在这条链里被探测。关闭本身就是「用户觉得它卡」的信号，交给下一条独立的体检通道处理（见下一节），避免两条链同时对同一个成员动手。

三条底线：

- **绝不踢空**：全部判死时留下失败数最少的那一个。`-fixed` 变空串 cfnat 起不来，用户看到的是彻底断网，比留一个待观察的成员严重得多。这不影响救援快车道——`activePoolAllDead` 读的仍是原始账本。
- **无替补不删**：见第 4 条。
- **固定成员只对「死」负责**：用户亲手固定的成员判慢不动（那是运营者的担保），判死才摘；摘的时候同时解除固定，并在页面上写「已失效下线」，避免配置与运行状态脱节。

快速复检有退避：判慢但换不掉的成员一旦标成「暂留」，复检标记立刻清掉，节拍退回 10 分钟常态——否则系统会长期锁在 2 分钟一次的高频探测上，烧负载且毫无进展。

相关变量：

| 变量 | 说明 | 默认 |
|---|---|---|
| `PROXY_ACTIVE_PROBE_SLOW_MS` | 握手耗时超过它就算「慢」（钳制 `[300,5000]`，越界回落 `1500`） | `1500` |
| `PROXY_POOL_MAX_ENTRY_LATENCY_MS` | 替补进池的延迟上限；也是页面标注「慢」的参照 | `800` |

### 被关闭的成员怎么处理

用户在页面上把某个成员关掉，本身就是一条质量信号：**他觉得这条线卡了**。而关闭之后它已经不再转发，探测它不会影响任何真实流量——这是一个零成本的取证窗口。所以关闭不等于「打入冷宫不再管」，而是换一条独立的体检通道（`web/disabled_bench.go`）。

**只体检被关闭的 IP。** 被关闭的域名不在这里处理：域名有 `domain_latency.go` 那条 30 分钟的质量闸，重复探测只会多烧一份流量。

体检流程：

1. **排队**：每 15 分钟取一批，最多 3 个，优先挑最久没测过的。用户刚关闭成员时会立刻唤醒一次，不等下一个周期。
2. **验活**：WS 101 握手。失败会重试一次；**连续 2 轮**都不通才判死移除——单次抖动不足以处决一个成员。
3. **测速**：握手通过的跑一次 VLESS 短下载，拿到实际 Mbps。若整轮所有成员测速都失败，判定为「测速通路本身坏了」，不记账、不据此下结论，避免把基础设施故障算到成员头上。
4. **下结论**：
   - 死了 → 移除。
   - 测速低于 `PROXY_DISABLED_BENCH_MIN_MBPS` → 判「体质差」，去找替补。
   - 测速正常 → 保留，说明用户感觉到的卡顿另有原因（可能是上游、可能是设备侧）。
   - 没跑成测速但握手延迟超过 `PROXY_ACTIVE_PROBE_SLOW_MS` → 也判「体质差」。
5. **替补必须明显更好才换**：候选要 WS 通过、进池延迟低于 800ms，并且满足其一——测速达到门槛且比被换者快 25% 以上；或者双方都没测速时，延迟改善达到 25% 且至少快 15ms。只快一点点不换，否则只是把抖动换成另一种抖动。
6. **池被锁定时不换人**：`Lock` 是用户亲手按下的「只转固定的」，自动流程不替他做决定；此时只做移除，不做替换。

执行细节：**纯移除不重启 cfnat**（写盘、更新内存即可，少一次断流）；只有发生替换才走完整换池流程并重启。替补进池时渠道标记为 `disabled-bench`，便于事后追溯是谁把它放进来的。

相关变量：

| 变量 | 说明 | 默认 |
|---|---|---|
| `PROXY_DISABLED_BENCH_ENABLED` | 关闭成员的体检通道总闸 | `true` |
| `PROXY_DISABLED_BENCH_INTERVAL_SECONDS` | 体检周期，秒（钳制 `[300,3600]`） | `900` |
| `PROXY_DISABLED_BENCH_MIN_MBPS` | 低于此值判「体质差」，触发找替补（钳制 `[0,10000]`） | `3` |

### 优选域名：固定成员与质量闸

第三方「优选域名」是优选 IP 的实时转发别名：每次新 TCP 连接时按域名当前 DNS 解析，天然跟随上游优选结果刷新。

- **固定成员**：`PROXY_DOMAIN_FORWARD` 显式配置、bestcf 推荐、以及启用的优选域名（需 `PROXY_AUTO_DOMAINS=true`）都会进入 cfnat `-fixed`，与登基 IP 并列轮换，拨号时活解析（`applyPoolLocked`）。它们不解析成 IP 快照、不进候选池。
- **兜底只认 `CFNAT_FALLBACK`**：`-fallback` 参数只由 `CFNAT_FALLBACK` 控制。优选域名和 `PROXY_DOMAIN_FORWARD` 都不写 `-fallback`——旧文档把这两件事混在一起，代码里从来没有这条路径。
- **域名要过质量闸**：域名不是「只转发、不探测」。`domain_latency.go` 每 30 分钟对池内与候选域名做 TCP:443 + VLESS 短下载，记录成功率、平均 Mbps、P50/P95 与抖动比。判 `dead`/`unstable` 的域名不进 `-fixed`；`PROXY_DOMAIN_FORWARD` 显式配置与用户固定的域名视为担保，不过闸。判死下线后进 30 分钟观察期，期内不会被自动加回。

域名只使用内置清单与 `PROXY_DOMAIN_FORWARD` 明确配置的域名，不接受任意输入；CFnat `-fixed` 参数同时接受 IP 与域名。
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

注意：本地 `ips-v4.txt` 当前是数千个 `/24` 的广义 CDN/合作网络候选，不等同于 Cloudflare 官方公布的 15 个 IPv4 CIDR。自动维护按配额合并用户输入、cfdata 自扫结果、订阅本子和启用的社区源（优选域名不解析供 IP）。**官方段不是准入门**：判据只有一条——能不能通过实际 Host + WebSocket path 的 `101` 初筛，以及启用 VLESS probe 时的真实数据面终审。内置 snapshot 只解决官方列表获取失败，不绕过业务验证。

## 无人值守运维

### 健康判据从哪看

唯一的健康出口是 `GET /api/status`（配合 `GET /api/health`）。判读表：

| 字段 | 健康值 | 异常含义 |
|---|---|---|
| `health.ok` | `true` | `false` 或端点不通 = 服务挂了 |
| `proxyAuto.ips` | 非空 | 空 = 没有在位 IP，转发无从谈起 |
| `poolSegments` | ≥ 2 个段 | 只有 1 段 = 抗单点能力弱，该段一挂全灭 |
| `activeProbe.members[*].consecutiveFailures` | 全 0 | ≥ `failThreshold`（默认 3）= 该成员已连续探活失败 |
| `activeProbe.members[*].lastLatencyMs` | 通常 < 2000 | 持续 > 3000 = 成员劣化但未达失败阈值 |
| `forward.dialMs` | < 1000 | > 3000 = 拨号显著变慢 |
| `forward.total` | 持续增长 | 长时间不变 = 没有流量在走 |
| `domainFallback` | `null` | 非空 = 正在靠被质量闸拦下的域名兜底撑（降级中，不算故障） |
| `sources[*].consecutiveFailures` | 全 0 | 连续失败 ≥ 3 = 该源已进退避期 |
| `sources[*].backoffUntil` | 零值 | 非零 = 该源退避中 |
| `cfnat.running` | `true` | `false` = 转发进程已停 |

**`proxyAuto.error` 不是异常判据。** 它是**粘滞字段**——换池成功后不一定被清。线上实测（2026-10-23）：池子在 09:28 重建成功、转发完全正常，而 `error` 仍留着 08:52 的「自动换池被换池闸拦截」。那只是换池冷却期的提示，把「error 非空」当异常判据会永远误报。

现在 `error` 带 `errorAt` 时间戳，且换池成功后 `poolBlock` 会被清空；判断"当前是否真的被拦"应当读 `poolBlock.blocked`，不要读 `error` 是否为空。

### 降级级（L1 / L2 / L3）

| 级 | 触发条件 | 行为 |
|---|---|---|
| L1 扩池 | 终审通过数 < `PROXY_AUTO_MIN_POOL` | 扩大 shortlist 重新考（**尚未实现**） |
| L2 放宽延迟 | 同上 | 放宽延迟上限（**尚未实现**） |
| L3 降级采用 | VLESS 终审 0 个通过 | 采用 WS 101 已过的候选（`degradedFromWSResults`，已实现） |
| 域名兜底 | 可用域名 < `minFallbackForwardDomains`（3） | 按体检成功率降序放行至多 3 个并标 degraded |

### 确定性巡检（不依赖 LLM）

`scripts/cf_inspect.py` 是替代 openclaw LLM 巡检的方案。2026-10-22~23 实测：LLM 巡检连续 7 次失败（上游 provider 拒绝 schema / 缺 API key / 请求超时），且 Telegram 投递也失败过一次（`sendMessage` 网络错误）——告警通道名义存在、实际又瞎又哑。

脚本读 `/api/status`，按上表固定规则判定，命中才推 Telegram：

```bash
# 判定并打印（不推送）
python3 scripts/cf_inspect.py --dry-run

# 判定 + 有异常才推送
python3 scripts/cf_inspect.py

# 无条件推送一条状态摘要
python3 scripts/cf_inspect.py --always
```

退出码：`0` 无异常 / `1` 有 WARN 或 CRITICAL / `2` 脚本自身故障。

**推送失败不丢消息**：发不出去（代理没起来、DNS 又漂了、API 超时）就把这份报告写进
`/vol1/1000/Docker/CFNat/cf-inspect-pending.json`（上限 20 条），**下一轮先补发一条再发新的**，
补发内容带 `[补发·N 分钟前]` 前缀。

两个容易写错的地方，这里都踩过：

- **补发必须排在「本轮无告警就返回」之前。** 先判断有没有事、没事就退出，补发写在后面，
  等于「网络一恢复、队列就永远不再被碰」——故障恢复得越快，那条告警丢得越彻底。
- **每条投递路径各重试一次。** 2026-11-03 那三次丢失都是「一次超时就放弃」，而代理端口
  刚重启时有十几秒不可用，多半再打一次就过去了。

**部署要点（NAS）**：

- 脚本位置 `/vol1/1000/Docker/CFNat/scripts/cf_inspect.py`
- 调度用**用户级 crontab**（`administer`），每 2 小时整点：`0 */2 * * *`
- 日志 `/vol1/1000/Docker/CFNat/data/cf-inspect.log`

**Telegram 投递必须走代理**：NAS 上 `api.telegram.org` 被 DNS 污染到 Meta 的 IP 段（`31.13.76.65` 等），直连必然超时。`127.0.0.1:39281` 是本机 sing-box 容器的代理口，实测可达：

```bash
CF_TG_PROXY=http://127.0.0.1:39281
```

脚本默认从 openclaw 配置里读 bot token 与 chat id（`CF_OPENCLAW_CONFIG`，默认指向
`/vol2/@apphome/trim.openclaw/data/home/.openclaw/openclaw.json`），也可用
`CF_TG_TOKEN` / `CF_TG_CHAT` 环境变量覆盖。**不必为巡检再复制一份凭据。**

### sing-box 隧道看门狗

`scripts/nas/cfnat-watchdog.py` 是另一条独立防线：它不关心优选池算得好不好，只保证**本机
sing-box 的出口隧道一直活着**。cron 每分钟跑一次，逻辑是：

> **当前状态：已部署并在运行。** 2026-11-05 21:22 核对：`/vol1/1000/Docker/sing-box/cfnat-watchdog.py`
> 存在，md5 `e210ced1af83dd278e071f4a6cf0f7bb`，与仓库版本一致；`watchdog.log` 与
> `watchdog-state.json` 都在持续更新。注意它**不在** `administer` 的 crontab 里
> （那里只有两小时一次的巡检），调度条目在 root 的 crontab 中，普通账户读不到。
> `cfnat-watchdog-drill.py` 尚未传上去，演练要手动 scp。

| 情形 | 动作 |
|---|---|
| 隧道通 + pool 模式 | 什么都不做 |
| 隧道通 + fallback 模式 | 证书探测 cfnat 池；连续 2 次通过才切回并告警 |
| 隧道死 | 连续 2 次确认后，按持久化的 `fb_idx` 轮换兜底域名；每次运行只试 1 个（重启+探测约 40s，试多了会撞下一次 cron） |

三个容易踩错的实现细节，都是拿故障换来的：

- **探测 URL 必须是会命中代理规则的站点**（`google.com/generate_204`）。用 `gstatic` 会走
  `direct` 规则，请求根本不经隧道，永远测不出死链路。
- **健康 = 204 且出口 IP ≠ 本机 IP**。只看状态码会被「代理失效后回落到直连」骗过去。
- **兜底入口用证书验证**：每个可用入口都会出示 subject CN 为节点域名的证书，这是它证明
  「我真的到了节点」的唯一方式。用进程内 socket 做（带硬超时），不用
  `openssl s_client | openssl x509` 管道——那条管道曾在死 socket 上把看门狗卡死数分钟。

告警走 openclaw 同一个 bot（token 运行时从 `openclaw.json` 读，不落日志），且**先恢复代理再发送**，
否则隧道断了告警也发不出去。发送失败的消息进 `pending` 队列（上限 20 条），隧道恢复后逐条补发。

`scripts/nas/cfnat-watchdog-drill.py` 是它的**实弹演练**：把 vless 目标改成关闭端口、
同时去掉 WS path 里的 `proxyip=` 参数，造一个「无法自动回落」的故障，然后观察看门狗是否
发现、切换、告警、再自动切回。原配置在 `finally` 里无条件还原。

> 两个脚本里的 sudo 口令只从环境变量 `NAS_SUDO_PW` 读取，未设置时退回 `sudo -n`
> （要求已配 NOPASSWD）。**口令不进仓库。**

### 已知运维陷阱

- **CF 官方段可能被运营商掐断，无预警。** 2026-10-21 上午 8 点至下午 3 点，CF 官方段全部不可达，症状是 `dial tcp ...: i/o timeout`（静默丢包，**不是** `connection refused`），累计 79889 次拨号超时；TCP 连通性检查无法区分「拒绝」与「丢包」，必须看超时。恢复后系统靠自动换池自愈（10-22 13:04、10-23 09:28 各重建一次）。巡检脚本内置了 CF 段可达性抽查来发现这类故障。
- **同 IP 不同 SNI 结果不同。** `curl https://IP/`（SNI=IP）通不代表 VLESS 用域名 SNI 也通；判断 CF 入口可用性必须用 WS 101 握手或完整 VLESS 链路。
- **`CFNAT_FIXED_IPS` 会被池文件覆盖。** `web/pool.go` 在每次换池时用 `proxy-active.json` 的成员覆盖它，改 compose 对转发**无作用**，只在池文件尚不存在时作冷启动种子。
- **`CFNAT_NUM` 是回退深度不是并发数。** 任何时刻只有一个上游连接在建立。
- **cfnat 日志时区是 UTC**，不是容器 `TZ=Asia/Shanghai`。

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

## 调试接口（仅测试构建）

`/api/debug/*` 在生产构建里**不存在**。它由 Go 构建标签控制：

```bash
go build -tags debugapi -o /tmp/cfnat-web-debug ./web
```

不加标签时 `registerDebugRoutes` 是空实现，调试代码不参与编译。发行构建（`scripts/build-dist.sh`、Dockerfile、`docker compose build`）都不带该标签——所以这不是「开关关掉了」，而是「根本没有这段代码」。`go test ./web/...` 与 `go test -tags debugapi ./web/...` 分别验证这两个世界：前者断言 `/api/debug/state` 返回 404，后者验证调试操作真的生效。

即使误发了带标签的构建，第二道保险仍然在：接口默认只认回环来源（`127.0.0.1` / `::1`），其它来源一律 403。在 NAS 上用 SSH 转发即可：

```bash
ssh -N -L 8080:127.0.0.1:8080 administer@192.168.1.50
```

隔离测试网络里确实需要远程访问时，才设 `DEBUG_API_ALLOW_REMOTE=1`。

| 路径 | 方法 | 作用 |
|---|---|---|
| `/api/debug/state` | GET | 一次取回池、候选、逐源健康、渠道汇总、质量账、探测账、两个动作的进度、`poolBlock`、`poolGuard`、cfnat 状态 |
| `/api/debug/env` | GET | 白名单内的运行参数（候选/漏斗/池大小/探针开关等）；密钥类变量不在白名单 |
| `/api/debug/pool` | POST | 直接覆盖并应用生效池：`{"ips":[],"domains":[],"priority":[],"results":[],"lock":false,"apply":true}`；`apply:false` 只写内存与磁盘、不重启 cfnat |
| `/api/debug/pool/reset-cooldown` | POST | 清零冷却锚点、拦截记录与池上的 `error/errorAt` |
| `/api/debug/candidates` | POST | 替换候选快照：`{"ips":[],"sourceByIp":{},"save":true}`；缺省来源记为 `user` |
| `/api/debug/kick` | POST | 踢成员：`{"member":"1.2.3.4","force":true}`；`force` 允许踢掉最后一个成员——空池只写内存与磁盘、不重启 cfnat（返回 `applied:false`） |
| `/api/debug/lock` | POST | 设置池锁：`{"locked":true}` |
| `/api/debug/probe` | POST | 对单个公网 IPv4 做一次 WS 探测：`{"ip":"1.2.3.4","timeoutMs":6000}`，返回延迟、colo、错误分类 |
| `/api/debug/revalidate` | POST | 触发「当前测试」；`?wait=1` 同步返回报告 |
| `/api/debug/optimize` | POST | 触发「立即优选」；`?wait=1` 同步返回报告 |
| `/api/debug/ledger` | POST | 清账：`{"what":"sources"}`，可选 `throne` / `domain` / `all`，同时清关联的隔离/健康记录 |

逐条 `curl` 示例与字段说明见 [docs/DEBUG-API.md](docs/DEBUG-API.md)。

调试接口能绕过换池闸、冷却、健康检查与片区配额——这正是它存在的意义，也是它绝不能随生产镜像发布的原因。改了 `debugapi_on.go` 之后，至少跑一次 `go test -tags debugapi ./web/...`。

## 注意

- `docs/legacy/x-tunnel.go` 只是历史参考，不参与镜像构建。
- `cfdata` 已补 `-auto` / `-no-wait` / `-dc` 等参数；Web 控制台不再依赖交互式 stdin。
- 如果手动填写数据中心，必须是 `ip.csv` 里已经存在的机房代号；留空表示提取全部。
