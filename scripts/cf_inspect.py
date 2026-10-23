#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""cloudflare-tools 确定性巡检。

为什么不用 LLM 巡检：
  2026-10-22 ~ 10-23 实测，openclaw 的 LLM 巡检连续 7 次失败（上游 provider
  拒绝 schema / 缺 API key / 请求超时），期间 Telegram 投递也失败过一次
  （HttpError: Network request for 'sendMessage' failed!）。结果是：告警通道
  名义存在、实际又瞎又哑。10/22 早上那次它甚至判断对了（"VLESS 终审仅 0 个
  通过"）却没发出去。

  本脚本不依赖任何 LLM、不依赖 openclaw、不依赖 Telegram bot 的 SDK：
  读 /api/status，按固定规则判定，命中才发 Telegram（直连 API，绕开
  openclaw 的投递链路）。provider 挂了、openclaw 挂了，都不影响它。

判定依据（全部取自 /api/status 的真实字段）：
  1. /api/health 不通 / ok != true            -> CRITICAL 服务本身不通
  2. proxyAuto.ips 为空                        -> CRITICAL 没有在位 IP
  3. activeProbe.members[*].consecutiveFailures
     >= failThreshold                           -> WARN 该成员连续探活失败
  4. forward.dialMs > 阈值                     -> WARN 拨号显著变慢
  5. poolSegments 段数 < 2                     -> WARN 抗单点能力弱
  6. sources 全部处于退避期                    -> WARN 无可用供给源
  7. domainFallback 非空                       -> INFO 正在靠兜底撑（不算故障）
  8. proxyAuto.error 非空                       -> 只作 INFO 附注，不单独告警

  第 8 条是刻意的：proxyAuto.error 是**粘滞字段**，换池成功后不一定被清
  （线上实测：池子在 09:28 重建成功、转发正常，而 error 仍留着 08:52 的
  "自动换池被换池闸拦截"）。把它当异常判据会永远误报，故降级为附注。

用法：
  python3 cf_inspect.py            # 巡检 + 有异常才推送
  python3 cf_inspect.py --dry-run  # 只打印判定，不推送
  python3 cf_inspect.py --always   # 无论是否异常都推送一条状态摘要

退出码：0 = 无异常；1 = 有 WARN/CRITICAL；2 = 脚本自身故障。
"""

import argparse
import calendar
import json
import os
import socket
import sys
import time
import urllib.error
import urllib.request

# ---- 配置（可用环境变量覆盖）-------------------------------------------

API_BASE = os.environ.get("CF_API_BASE", "http://127.0.0.1:8080").rstrip("/")
TELEGRAM_TOKEN = os.environ.get("CF_TG_TOKEN", "")
TELEGRAM_CHAT = os.environ.get("CF_TG_CHAT", "")

# Token 兜底来源：openclaw 的配置里已有同一个 bot token。直接从那里读，
# 免得为了巡检再往别处复制一份凭据（多一份 = 多一处泄露面）。
# 该文件通常只有 openclaw 属主可读；读不到就静默跳过（无 token 则不推送）。
OPENCLAW_CONFIG = os.environ.get(
    "CF_OPENCLAW_CONFIG",
    "/vol2/@apphome/trim.openclaw/data/home/.openclaw/openclaw.json")


def _load_token_from_openclaw():
    global TELEGRAM_TOKEN
    if TELEGRAM_TOKEN:
        return
    try:
        with open(OPENCLAW_CONFIG, encoding="utf-8") as fh:
            cfg = json.load(fh)
        token = (cfg.get("channels", {}).get("telegram", {}) or {}).get("botToken", "")
        if token:
            TELEGRAM_TOKEN = token
    except Exception:
        pass


def _load_chat_from_openclaw():
    global TELEGRAM_CHAT
    if TELEGRAM_CHAT:
        return
    try:
        with open(OPENCLAW_CONFIG, encoding="utf-8") as fh:
            cfg = json.load(fh)
        allowed = ((cfg.get("channels", {}).get("telegram", {}) or {})
                   .get("allowFrom") or [])
        if allowed:
            TELEGRAM_CHAT = str(allowed[0])
    except Exception:
        pass

# NAS 上 api.telegram.org 被 DNS 污染到 Meta 的 IP，必须走本地 sing-box 代理。
TELEGRAM_PROXY = os.environ.get("CF_TG_PROXY", "http://127.0.0.1:39281")

# 判定阈值
DIAL_MS_WARN = int(os.environ.get("CF_DIAL_MS_WARN", "3000"))
HTTP_TIMEOUT = float(os.environ.get("CF_HTTP_TIMEOUT", "20"))
MAX_FORWARD_AGE_SEC = int(os.environ.get("CF_MAX_FORWARD_AGE_SEC", "1800"))

# CF 官方 IPv4 段（与 web/cfranges.go 的 cloudflareIPv4CIDRs 保持一致）。
CF_CIDRS = [
    "173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
    "141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
    "197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
    "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
]


def _cidr_nets():
    import ipaddress
    return [ipaddress.ip_network(c) for c in CF_CIDRS]


CF_NETS = _cidr_nets()


def in_cf_segment(ip):
    try:
        import ipaddress
        addr = ipaddress.ip_address(ip)
    except ValueError:
        return False
    return any(addr in net for net in CF_NETS)


# ---- HTTP 助手 ----------------------------------------------------------


def http_json(url, timeout=HTTP_TIMEOUT):
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


# ---- 判定 ---------------------------------------------------------------


class Verdict(object):
    """一条判定结果。level: OK / INFO / WARN / CRITICAL"""

    def __init__(self, level, title, detail=""):
        self.level = level
        self.title = title
        self.detail = detail

    def as_text(self):
        if self.detail:
            return "[%s] %s — %s" % (self.level, self.title, self.detail)
        return "[%s] %s" % (self.level, self.title)

    def __repr__(self):
        return "<Verdict %s>" % self.as_text()


def check_health(verdicts):
    try:
        h = http_json(API_BASE + "/api/health")
    except Exception as e:
        verdicts.append(Verdict("CRITICAL", "健康端点不可达",
                                "%s %s" % (type(e).__name__, e)))
        return None
    if not h.get("ok"):
        verdicts.append(Verdict("CRITICAL", "健康端点返回 ok=false",
                                json.dumps(h, ensure_ascii=False)[:200]))
    else:
        v = h.get("version") or {}
        verdicts.append(Verdict("OK", "服务在线",
                                "gitCommit=%s buildTime=%s" %
                                (v.get("gitCommit", "?"), v.get("buildTime", "?"))))
    return h


def check_pool(verdicts, status):
    pa = status.get("proxyAuto") or {}
    ips = pa.get("ips") or []
    domains = pa.get("domains") or []

    if not ips:
        verdicts.append(Verdict("CRITICAL", "生效池没有 IP",
                                "proxyAuto.ips 为空，域名 %d 个" % len(domains)))
    else:
        non_cf = [ip for ip in ips if not in_cf_segment(ip)]
        if non_cf:
            verdicts.append(Verdict("WARN", "池内存在非 CF 官方段 IP",
                                    ", ".join(non_cf)))
        else:
            verdicts.append(Verdict("OK", "生效池正常",
                                    "%d IP + %d 域名，全在 CF 官方段" %
                                    (len(ips), len(domains))))

    # 段分布：少于 2 段意味着抗单点能力弱（2026-10-22 事故形态）。
    segs = status.get("poolSegments") or {}
    if len(segs) < 2:
        verdicts.append(Verdict("WARN", "池内 CF 段少于 2 个",
                                "段分布=%s，某个段一挂全灭" % json.dumps(segs)))
    else:
        verdicts.append(Verdict("OK", "段分布健康", json.dumps(segs)))

    # domainFallback：降级状态是 INFO，不是故障。
    fb = status.get("domainFallback")
    if fb:
        verdicts.append(Verdict("INFO", "正在靠域名兜底撑",
                                json.dumps(fb, ensure_ascii=False)[:200]))

    # proxyAuto.error：粘滞字段，只作附注。
    err = pa.get("error")
    if err:
        verdicts.append(Verdict("INFO", "proxyAuto.error 附注", err[:200]))

    return ips


def check_active_probe(verdicts, status):
    ap = status.get("activeProbe") or {}
    if not ap.get("enabled"):
        verdicts.append(Verdict("INFO", "在位探活未启用", "activeProbe.enabled=false"))
        return
    members = ap.get("members") or {}
    threshold = ap.get("failThreshold") or 3
    bad = [(k, v) for k, v in members.items()
           if (v.get("consecutiveFailures") or 0) >= threshold]
    if bad:
        detail = ", ".join("%s(%d 次失败)" % (k, v.get("consecutiveFailures"))
                           for k, v in sorted(bad)[:6])
        verdicts.append(Verdict("WARN", "%d 个在位成员连续探活失败" % len(bad), detail))
    else:
        lat = [(v.get("lastLatencyMs") or 0) for v in members.values()]
        rng = ("%d~%dms" % (min(lat), max(lat))) if lat else "无数据"
        verdicts.append(Verdict("OK", "在位探活正常",
                                "%d 成员全绿，延迟 %s" % (len(members), rng)))


def check_forward(verdicts, status):
    fw = status.get("forward") or {}
    total = fw.get("total") or 0
    dial = fw.get("dialMs") or 0
    at = fw.get("at") or ""

    if not total:
        verdicts.append(Verdict("WARN", "没有转发记录", "forward.total=0"))
        return

    age = None
    if at:
        try:
            # forward.at 是 UTC（尾部带 Z），必须按 UTC 解析——用本地时间
            # 会凭空差一个时区，把「刚刚的转发」误判成「几小时前的陈旧记录」。
            ts = calendar.timegm(time.strptime(
                at.replace("Z", "").split(".")[0], "%Y-%m-%dT%H:%M:%S"))
            age = time.time() - ts
        except Exception:
            age = None
    if age is not None and age > MAX_FORWARD_AGE_SEC:
        verdicts.append(Verdict("WARN", "转发记录陈旧",
                                "最后转发在 %d 秒前（阈值 %d）" % (age, MAX_FORWARD_AGE_SEC)))

    if dial > DIAL_MS_WARN:
        verdicts.append(Verdict("WARN", "拨号延迟过高",
                                "dialMs=%d（阈值 %d）" % (dial, DIAL_MS_WARN)))
    else:
        verdicts.append(Verdict("OK", "转发正常",
                                "total=%d dialMs=%d" % (total, dial)))


def check_sources(verdicts, status):
    srcs = status.get("sources") or {}
    if not srcs:
        verdicts.append(Verdict("WARN", "没有源健康数据", "sources 为空"))
        return
    backed_off = []
    for k, v in srcs.items():
        bu = v.get("backoffUntil") or ""
        if bu and not str(bu).startswith("0001"):
            backed_off.append(k)
    if srcs and len(backed_off) == len(srcs):
        verdicts.append(Verdict("WARN", "所有供给源都在退避期",
                                "无可用供给，自动建池会失败"))
    else:
        verdicts.append(Verdict("OK", "供给源健康",
                                "%d 源，退避 %d" % (len(srcs), len(backed_off))))


def check_cf_segment(verdicts, ips):
    """CF 官方段可达性抽查。

    2026-10-21 运营商把 CF 段掐了 7 小时，症状是 i/o timeout（静默丢包），
    不是 connection refused。TCP 探测能区分这两者：被丢包的地址会超时。
    用池内成员做抽查，不新增外部依赖。
    """
    if not ips:
        return
    sample = ips[:3]
    unreachable = []
    for ip in sample:
        s = socket.socket()
        s.settimeout(5)
        try:
            s.connect((ip, 443))
        except Exception:
            unreachable.append(ip)
        finally:
            s.close()
    if len(unreachable) == len(sample):
        verdicts.append(Verdict("CRITICAL", "CF 官方段全部不可达",
                                "抽查 %s 全部超时——疑似运营商再次掐断" %
                                ", ".join(sample)))
    elif unreachable:
        verdicts.append(Verdict("WARN", "部分 CF 官方段不可达",
                                "抽查超时：%s" % ", ".join(unreachable)))
    else:
        verdicts.append(Verdict("OK", "CF 官方段可达",
                                "抽查 %s 全部连通" % ", ".join(sample)))


def check_cfnat(verdicts, status):
    cn = status.get("cfnat") or {}
    if not cn.get("running"):
        verdicts.append(Verdict("CRITICAL", "cfnat 未运行", json.dumps(cn)[:200]))
    else:
        verdicts.append(Verdict("OK", "cfnat 运行中",
                                "pid=%s startedAt=%s" % (cn.get("pid"), cn.get("startedAt"))))


def inspect():
    verdicts = []
    health = check_health(verdicts)
    if health is None:
        return verdicts, None

    try:
        status = http_json(API_BASE + "/api/status")
    except Exception as e:
        verdicts.append(Verdict("CRITICAL", "/api/status 读取失败",
                                "%s %s" % (type(e).__name__, e)))
        return verdicts, None

    ips = check_pool(verdicts, status)
    check_active_probe(verdicts, status)
    check_forward(verdicts, status)
    check_sources(verdicts, status)
    check_cfnat(verdicts, status)
    check_cf_segment(verdicts, ips or [])
    return verdicts, status


# ---- 推送 ---------------------------------------------------------------


def send_telegram(text):
    _load_token_from_openclaw()
    _load_chat_from_openclaw()
    if not TELEGRAM_TOKEN or not TELEGRAM_CHAT:
        return False, "未配置 TELEGRAM_TOKEN / TELEGRAM_CHAT"
    url = "https://api.telegram.org/bot%s/sendMessage" % TELEGRAM_TOKEN
    payload = json.dumps({"chat_id": TELEGRAM_CHAT, "text": text,
                          "disable_web_page_preview": True}).encode("utf-8")

    # 先走本地 sing-box 代理（NAS 上 api.telegram.org 被 DNS 污染到 Meta 的
    # IP 段，直连必然超时），失败再试直连——将来 DNS 若被修好，兜底这条路
    # 会自己生效，不必回来改脚本。
    attempts = []
    if TELEGRAM_PROXY:
        attempts.append(("代理 " + TELEGRAM_PROXY, TELEGRAM_PROXY))
    attempts.append(("直连", ""))

    last = ""
    for label, proxy in attempts:
        try:
            import urllib.request as u
            handlers = []
            if proxy:
                handlers.append(u.ProxyHandler({"http": proxy, "https": proxy}))
            else:
                handlers.append(u.ProxyHandler({}))
            opener = u.build_opener(*handlers)
            req = u.Request(url, data=payload,
                            headers={"Content-Type": "application/json"})
            with opener.open(req, timeout=25) as resp:
                body = json.loads(resp.read().decode("utf-8"))
            if body.get("ok"):
                return True, "已推送（%s）" % label
            last = "%s 拒绝：%s" % (label, str(body.get("description"))[:120])
        except Exception as e:
            last = "%s 失败：%s %s" % (label, type(e).__name__, e)
    return False, last


def format_report(verdicts):
    bad = [v for v in verdicts if v.level in ("WARN", "CRITICAL")]
    lines = ["cloudflare-tools 巡检 · %s" %
             time.strftime("%Y-%m-%d %H:%M:%S")]
    if not bad:
        lines.append("结论：正常")
        for v in verdicts:
            if v.level in ("OK", "INFO"):
                lines.append("  · " + v.as_text())
        return "\n".join(lines)

    crit = [v for v in bad if v.level == "CRITICAL"]
    warn = [v for v in bad if v.level == "WARN"]
    head = "结论：异常"
    if crit:
        head += "（CRITICAL %d / WARN %d）" % (len(crit), len(warn))
    elif warn:
        head += "（WARN %d）" % len(warn)
    lines.append(head)
    for v in crit + warn:
        lines.append("  ! " + v.as_text())
    for v in verdicts:
        if v.level == "INFO":
            lines.append("  · " + v.as_text())
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description="cloudflare-tools 确定性巡检")
    ap.add_argument("--dry-run", action="store_true", help="只判定，不推送")
    ap.add_argument("--always", action="store_true", help="无论是否异常都推送")
    args = ap.parse_args()

    try:
        verdicts, _ = inspect()
    except Exception as e:
        print("巡检脚本自身故障：%s %s" % (type(e).__name__, e), file=sys.stderr)
        return 2

    report = format_report(verdicts)
    print(report)

    bad = [v for v in verdicts if v.level in ("WARN", "CRITICAL")]
    if not (bad or args.always):
        return 0

    if args.dry_run:
        print("\n[dry-run] 跳过推送")
        return 1 if bad else 0

    ok, why = send_telegram(report)
    print("\n推送：%s（%s）" % ("成功" if ok else "失败", why))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
