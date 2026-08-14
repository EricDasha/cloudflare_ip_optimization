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

打开 `http://localhost:8080` 后可以：

1. 在 `/` 查看进程、生效池和候选池，或点击“完整优选”依次运行 CFdata、候选汇合、WebSocket 初筛、VLESS 数据面终审与 CFnat 换池。
2. 在 `/` 的日志工作面切换进程、搜索、筛选成功/错误、调整行数、暂停、跟随或复制日志。
3. 在 `/cfnat` 启动、停止、重启 `cfnat`；常用参数直接显示，完整 flags 收在“高级参数”。
4. 在 `/cfdata` 发起扫描、查看实时扫描表、数据中心汇总、`ip.csv` 扫描结果、丢包率与下载测速；并发、端口和强制更新收在“高级参数”。
5. 在 `/files` 下载 `/data` 中的 `.csv`、`.txt`、`.json`、`.log` 文件。

### 反代 IP 维护

在 `/cfnat` 展开“手动扫描”后选择社区候选源，也可以粘贴自己的 IPv4 列表。候选源通过 DNS 解析读取，不执行第三方脚本，也不会修改 DNS。

推荐的手动维护流程：

1. 填写实际节点使用的 `SNI / Host` 和目标端口。
2. 选择候选源，按网络情况设置扫描并发、延迟上限和扫描数量。
3. 点击“开始扫描”，检查 `PASS` 结果与来源错误。
4. 点击“采用通过 IP”，确认“固定转发 IP”已更新。
5. 自动池启用 VLESS probe 时，后台会用本地 sing-box 模板请求 `generate_204`；手动扫描仍只负责 TCP/TLS 粗筛。
6. 只有真实节点返回 `204` 才进入自动池；单纯 TCP/TLS 或 WebSocket `101` 不能证明业务可用。

内置候选源：

- `proxyip.zhaobo.org`：社区聚合池，上游项目 `kouzhaobo/proxyip-scanner`。
- `tw.william.us.ci`、`kr.william.us.ci`：台湾和韩国候选。
- `cdn.xn--b6gac.eu.org`：EU.org 社区候选池。

服务端只允许上述固定候选源，不接受任意 URL，并拒绝私网、回环、链路本地与组播地址。扫描接口限制请求体为 1 MiB、并发为 `1-500`、扫描数量为 `1-10000`，同一时刻只运行一个扫描任务。

Web 服务启动时会立即刷新一次候选缓存，之后每 6 小时重新解析全部内置源。成功结果以原子替换方式写入 `/data/proxy-candidates.json`；刷新失败时保留上一次成功候选。页面会显示上次成功时间、下次刷新时间，并提供“立即拉取候选”和“载入自动候选”。

启用 `PROXY_AUTO_APPLY` 后，后台先用实际 `Host + WebSocket path` 对全部候选并发执行 TLS 与 WebSocket `101 Switching Protocols` 初筛。启用 `PROXY_VLESS_PROBE` 后，再按 WS 延迟从快到慢启动短生命周期 sing-box，以候选 `IP:PROXY_AUTO_PORT` 覆盖模板的服务器地址，并通过真实 VLESS 链路请求 `generate_204`。探针凑满 `PROXY_AUTO_POOL_SIZE` 即停止；只有 VLESS 通过数量达到 `PROXY_AUTO_MIN_POOL` 才替换 `/data/proxy-active.json` 并重启 CFnat。模板、sing-box 或数据面失败均保留旧池。

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
| `CFNAT_IPNUM` | `-ipnum` | `20` |
| `CFNAT_IPS` | `-ips` | `4` |
| `CFNAT_NUM` | `-num` | `5` |
| `CFNAT_PORT` | `-port` | `443` |
| `CFNAT_RANDOM` | `-random` | `true` |
| `CFNAT_TASK` | `-task` | `100` |
| `CFNAT_TLS` | `-tls` | `true` |
| `CFNAT_CODE` | `-code` | `200` |

自动候选池相关变量：

| 变量 | 说明 | 默认 |
|---|---|---|
| `PROXY_AUTO_APPLY` | 是否自动 WS 终审并替换固定池 | `false` |
| `PROXY_AUTO_HOST` | 实际 VLESS/WS 节点 Host 与 TLS SNI | 空 |
| `PROXY_AUTO_PATH` | 实际 WebSocket path | `/` |
| `PROXY_AUTO_PORT` | 候选目标端口 | `443` |
| `PROXY_AUTO_CONCURRENCY` | WS 终审并发 | `20` |
| `PROXY_AUTO_MAX_LATENCY` | 单 IP 终审超时，毫秒 | `5000` |
| `PROXY_AUTO_POOL_SIZE` | 自动生效池最大数量 | `5` |
| `PROXY_AUTO_MIN_POOL` | 允许替换旧池的最少通过数量 | `3` |
| `PROXY_AUTO_CFDATA` | 每轮自动候选维护前运行一次 CFdata | `false` |
| `PROXY_AUTO_CFDATA_TIMEOUT` | CFdata 最长运行秒数 | `600` |
| `PROXY_CFDATA_CANDIDATES` | 从 `ip.csv` 读取的候选上限 | `300` |
| `PROXY_OFFICIAL_CANDIDATES` | 从 Cloudflare 官方 CIDR 均匀抽样的候选数 | `150` |
| `PROXY_VLESS_PROBE` | 是否在 WS 初筛后运行真实 VLESS 数据面终审 | `false` |
| `PROXY_VLESS_TEMPLATE` | 本地 VLESS outbound 或完整 sing-box 配置 | `/data/vless-probe-outbound.json` |
| `PROXY_VLESS_TEST_URL` | 经 VLESS 请求的轻量验活地址 | `https://www.gstatic.com/generate_204` |
| `PROXY_VLESS_EXPECT_STATUS` | 数据面成功状态码 | `204` |
| `PROXY_VLESS_TIMEOUT` | 每个 VLESS probe 超时，秒 | `15` |
| `PROXY_VLESS_MAX_CANDIDATES` | 每轮最多执行真实 VLESS probe 的候选数 | `20` |
| `SING_BOX_BIN` | sing-box 二进制路径 | `/usr/local/bin/sing-box` |

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

注意：本地 `ips-v4.txt` 当前是数千个 `/24` 的广义 CDN/合作网络候选，不等同于 Cloudflare 官方公布的 15 个 IPv4 CIDR。自动维护会分别使用三类输入：社区 ProxyIP DNS、CFdata 已扫描的 `ip.csv`、`https://www.cloudflare.com/ips-v4` 官方段抽样；三者最终都必须通过实际 Host + WebSocket path 的 `101` 初筛，并在启用 VLESS probe 时完成真实数据面终审。

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
