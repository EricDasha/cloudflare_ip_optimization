# 接手须知（给 muse / 任意 agent）

**先读 `.agent/PROJECT-CONTEXT.md`** —— 那是这个项目的完整说明书，架构、机制、改动史、回滚点、踩过的坑全在里面。记忆或对话记录丢失时，它和根目录的 `CLAUDE.md` 是唯一的恢复依据。

这里只是导航。

## 这个项目是什么

NAS Docker 里跑 `cloudflare-tools`，自动优选 Cloudflare IP 与反代域名，经 cfnat 转发给前端代理软件。**服务对象是 [cmliu/edgetunnel](https://github.com/cmliu/edgetunnel) 与 [xtgm/GrainTCPV1](https://github.com/xtgm/GrainTCPV1)。**

## 当前状态（2026-11-12 22:55 CST）

- 线上版本 `b1e5133+d1incumbent`（D1 在位粘性），健康。
- 池：10 IP + 24 域名。经 1234 真实请求 6/6 全 200。
- **主线问题仍是重启抖动**：D1 已上线观察中，D2（防抖窗口 + 紧急故障切换）只有设计。

## 立刻要做的

1. 读 `.agent/PROJECT-CONTEXT.md` 第 9 节「当前已知问题」。
2. 读 `.agent/CHANGE-D-DESIGN.md` 的 D2 部分 —— 那是下一件该做的事。
3. 观察 D1 效果：`grep -c '正在监听 0.0.0.0:1234' cfnat.log`，30 分钟差值 > 3 说明还没治好。

## 三个不能碰的红线

- 不提高探测并发、不扩大探测压力
- 不动 Active 池、不删候选、不改订阅预算
- 不重构调度核心，尽量缩小 diff

## 最容易犯的错

**不要降级官方 CF IP。** 直觉上「官方段探测失败率高就该排除」，但实测证明失败的是 NAS 整条出口：shopify、github、google 的自有 IP 同样超时，fastly 和国内 DNS 不受影响。官方段在轮次间会自行恢复。降级只会缩小可选面，治不了病。

## 判定成功的标准

不是「探测日志绿色勾变多」，是「经 1234 端口的真实请求成功率上升」。

```bash
# 在 NAS 上
curl -s -o /dev/null -w '%{http_code} %{time_total}\n' --noproxy '*' \
  --connect-to www.cloudflare.com:443:127.0.0.1:1234 \
  https://www.cloudflare.com/cdn-cgi/trace
```

## 文件导航

| 文件 | 内容 |
|---|---|
| `PROJECT-CONTEXT.md` | **完整说明书，先读这个** |
| `HANDOFF.md` | 给 muse 的任务单（A/B/C 三项改动，部分结论已被复测推翻，看修正说明） |
| `CI-REPORT.md` | GitHub Actions CI 交付报告、22 项 Windows 失败的根因与修法 |
| `CHANGE-D-DESIGN.md` | 改动 D 完整设计：D1（已实施）、D2（待实施）、紧急故障切换 |
| `evidence/` | 探测原始输出、状态快照、日志片段 |
| `patches/` | v4 工作区 diff（已并入 `ddff7e6`） |
