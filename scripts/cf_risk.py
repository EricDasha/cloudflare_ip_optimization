#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""cloudflare-tools 风控模型：7 天滚动基线 + 断崖检测。

为什么要有它：
  2026-11-17 排查「时好时坏」时，所有判断都靠人肉翻日志——数 cfnat.log 的
  失败次数、数 /api/status 的 stage 分布、再逐个 IP 测下载。这些动作每天重
  复，结论却只活在当时的对话里，第二天没人记得基线是多少。于是「今天是不
  正常」这个问题永远没有参照物，只能等用户先感觉到卡。

  本模块每轮巡检留一条样本（cron 每 2 小时一次），只保留最近 7 个自然日，
  算三组基线：终审通过率、官方段存活、供给源成功率。判据是「和过去几天的
  自己比」，不是拍一个绝对阈值——反代池的绝对水平随线路波动，绝对值必然
  天天误报。

数学模型（刻意选简单、可解释的）：
  - 日基线 = 当天所有样本的聚合；比例用「求和后再除」而不是「各样本比例
    求平均」，否则一个只有 2 个样本的小时和 60 个样本的小时权重相同。
  - 参照 = 前若干天（最多 6 天）日基线的中位数。中位数对单日异常不敏感，
    不会因为昨天塌了一次就把基线拉低、导致今天塌了也看不出来。
  - 离散度 = MAD（中位绝对偏差），稳健 z = 0.6745 * (今天 - 中位数) / MAD。
    比标准差稳健：一个离群日不会把分母撑大到失效。
  - 触发条件（任一）：
      * 相对塌方：今天 < 参照中位数 × 0.5（参照本身要有意义，> 5%）
      * 稳健 z < -3.5（仅在 MAD > 0 时可用；MAD = 0 说明历史完全一致，
        只有相对塌方这条能判）
  - 官方段全灭单独成条：连续 2 个样本 official_tested >= 5 且 official_pass
    = 0，且 7 天内出现过 official_pass > 0。要求连续两次是为了排除单次抖动
    （VLESS 终审本身就有偶发超时）。

数据量要求（宁可不说，不可乱说）：
  - 当天终审样本 < 15 个：比例不可信，只记录不判定。
  - 历史有效天数 < 3 天：没有基线，只记录不判定（冷启动期约 1 天）。
  - 任何字段缺失都按「没有这条证据」处理，不猜。

存储：默认 /vol1/1000/Docker/CFNat/cf-risk-daily.json，与巡检补发队列同目录
（不写 data/，那是生产账本与日志目录）。写入用 tmp + os.replace，原子替换。

单独运行：
  python3 cf_risk.py                 # 读 /api/status，记录并打印判定
  python3 cf_risk.py --dry-run       # 只判定，不写历史
  python3 cf_risk.py --status-file /tmp/status.json
"""

import argparse
import json
import os
import statistics
import sys
import time

from cf_inspect import API_BASE, in_cf_segment, http_json

# ---- 配置 ---------------------------------------------------------------

RISK_FILE = os.environ.get("CF_RISK_FILE", "/vol1/1000/Docker/CFNat/cf-risk-daily.json")
RETENTION_DAYS = 7
MAX_SAMPLES_PER_DAY = 200          # 每 2 小时一条 = 每天 12 条，留足余量

MIN_FINALISTS = 15                 # 当天终审样本少于此数，比例不可信
MIN_BASELINE_DAYS = 3              # 冷启动：历史少于 3 天才开始判定
MIN_BASELINE_RATIO = 0.05          # 基线低于 5% 时，相对塌方判据没有意义
DROP_FACTOR = float(os.environ.get("CF_RISK_DROP_FACTOR", "0.5"))
MAD_Z_LIMIT = float(os.environ.get("CF_RISK_MAD_Z", "3.5"))

OFFICIAL_MIN_TESTED = 5            # 单条样本里官方段至少测了这么多个才算证据
OFFICIAL_DEAD_STREAK = 2           # 连续这么多个样本官方段全灭才算坐实

SOURCE_RATIO_WARN = 0.4            # 今天源成功率低于此值
SOURCE_RATIO_BASE = 0.8            # 且历史基线高于此值 —— 才算「塌方」
POOL_SHRINK_FACTOR = 0.5           # 池内 IP 数低于基线的一半


# ---- 采样 ---------------------------------------------------------------


def extract(status, now=None):
    """从 /api/status 抽一条样本。

    stage 语义（web/funnel.go）：
      WS_FAIL    TCP/TLS 通但 WS 101 没拿到 —— 根本没走到数据面
      WS_PASS    WS 通了，但 VLESS 数据面失败
      VLESS_FAIL 数据面请求失败（超时或报错）
      VLESS_PASS 数据面真通了 —— 这才是「验活」
    「验活比例」的分子只认 VLESS_PASS，分母是进终审的全部候选。把 WS_FAIL
    算进分子会虚高：WS 握手成功跟能不能过流量是两回事（2026-11-17 实测
    一堆 WS_PASS 的 IP 下载只有 27KB/s 或直接 000）。
    """
    now = now or time.time()
    pa = status.get("proxyAuto") or {}
    results = pa.get("results") or []

    stages = {}
    for r in results:
        stage = r.get("stage") or ""
        stages[stage] = stages.get(stage, 0) + 1

    official = [r for r in results if in_cf_segment(str(r.get("ip") or ""))]
    official_pass = sum(1 for r in official
                        if (r.get("stage") or "") == "VLESS_PASS")

    srcs = status.get("sources") or {}
    src_fetches = 0
    src_successes = 0
    src_backoff = 0
    for v in srcs.values():
        src_fetches += int(v.get("fetches") or 0)
        src_successes += int(v.get("successes") or 0)
        bu = v.get("backoffUntil") or ""
        if bu and not str(bu).startswith("0001"):
            src_backoff += 1

    return {
        "t": int(now),
        "finalists": len(results),
        "pass": stages.get("VLESS_PASS", 0),
        "ws_pass": stages.get("WS_PASS", 0),
        "ws_fail": stages.get("WS_FAIL", 0),
        "vless_fail": stages.get("VLESS_FAIL", 0),
        "timeouts": sum(1 for r in results
                        if (r.get("errorClass") or "") == "timeout"),
        "official_tested": len(official),
        "official_pass": official_pass,
        "pool_ips": len(pa.get("ips") or []),
        "pool_domains": len(pa.get("domains") or []),
        "segments": len(status.get("poolSegments") or {}),
        "src_total": len(srcs),
        "src_fetches": src_fetches,
        "src_successes": src_successes,
        "src_backoff": src_backoff,
    }


def day_key(ts):
    return time.strftime("%Y-%m-%d", time.localtime(ts))


def aggregate(samples):
    """把一个自然日内的样本聚合成一天的体检记录。"""
    if not samples:
        return None
    finalists = sum(s.get("finalists", 0) for s in samples)
    passed = sum(s.get("pass", 0) for s in samples)
    off_tested = sum(s.get("official_tested", 0) for s in samples)
    off_pass = sum(s.get("official_pass", 0) for s in samples)
    ratios = [s["src_successes"] / float(s["src_fetches"])
              for s in samples if s.get("src_fetches")]
    pools = [s.get("pool_ips", 0) for s in samples]
    return {
        "samples": len(samples),
        "finalists": finalists,
        "pass": passed,
        "ratio": (passed / float(finalists)) if finalists else None,
        "official_tested": off_tested,
        "official_pass": off_pass,
        "official_ratio": (off_pass / float(off_tested)) if off_tested else None,
        "src_ratio": statistics.median(ratios) if ratios else None,
        "pool_ips": statistics.median(pools) if pools else None,
    }


# ---- 存储 ---------------------------------------------------------------


def load_store(path=None):
    path = path or RISK_FILE
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except Exception:
        return {"version": 1, "days": {}}
    if not isinstance(data, dict) or not isinstance(data.get("days"), dict):
        return {"version": 1, "days": {}}
    return data


def save_store(store, path=None):
    path = path or RISK_FILE
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(store, fh, ensure_ascii=False, separators=(",", ":"))
    os.replace(tmp, path)


def prune(store, today):
    """只保留最近 RETENTION_DAYS 个自然日（含今天）。"""
    keep = sorted(store["days"].keys())[-RETENTION_DAYS:]
    if today not in keep:
        keep = sorted(set(keep) | {today})[-RETENTION_DAYS:]
    store["days"] = {k: store["days"][k] for k in keep}


def record(status, path=None, now=None):
    """追加一条样本；返回 (store, sample)。"""
    now = now or time.time()
    sample = extract(status, now)
    store = load_store(path)
    today = day_key(now)
    bucket = store["days"].setdefault(today, [])
    bucket.append(sample)
    del bucket[:-MAX_SAMPLES_PER_DAY]
    prune(store, today)
    save_store(store, path)
    return store, sample


# ---- 判定 ---------------------------------------------------------------


def _recent_samples(store, limit):
    """按时间取最后 limit 条样本（跨天）。"""
    out = []
    for key in sorted(store["days"].keys()):
        out.extend(store["days"][key])
    out.sort(key=lambda s: s.get("t", 0))
    return out[-limit:]


def _robust_z(value, base_values):
    """MAD 稳健 z。base_values 为空或 MAD=0 时返回 None（不可用）。"""
    if not base_values:
        return None
    med = statistics.median(base_values)
    devs = [abs(v - med) for v in base_values]
    mad = statistics.median(devs)
    if mad <= 0:
        return None
    return 0.6745 * (value - med) / mad


def evaluate(store, now=None):
    """返回 [(level, title, detail), ...]。level 只用 OK/INFO/WARN/CRITICAL。"""
    now = now or time.time()
    today = day_key(now)
    out = []

    today_agg = aggregate(store["days"].get(today) or [])
    prior_keys = [k for k in sorted(store["days"].keys()) if k < today]
    prior = [a for a in (aggregate(store["days"][k]) for k in prior_keys[-6:]) if a]
    usable = [a for a in prior if a["finalists"] >= MIN_FINALISTS]

    if today_agg is None:
        out.append(("INFO", "风控模型无当日样本", "本轮状态读取失败或池为空"))
        return out

    # 1) 官方段全灭：连续 N 条样本官方段全测全灭。
    recent = _recent_samples(store, OFFICIAL_DEAD_STREAK)
    ever_alive = any((a.get("official_pass") or 0) > 0
                     for a in (aggregate(store["days"][k])
                               for k in store["days"]))
    if (len(recent) >= OFFICIAL_DEAD_STREAK and ever_alive and
            all(s.get("official_tested", 0) >= OFFICIAL_MIN_TESTED and
                s.get("official_pass", 0) == 0 for s in recent)):
        out.append(("CRITICAL", "官方 IP 段疑似全灭",
                    "连续 %d 次终审里官方段 0 通过（每次至少测 %d 个）——"
                    "疑似运营商掐断 CF anycast" %
                    (len(recent), OFFICIAL_MIN_TESTED)))

    # 2) 官方段进不了短名单：以前能进，今天一个都没有。
    if ever_alive and today_agg["official_tested"] == 0:
        prior_off = [a["official_tested"] for a in prior if a["official_tested"]]
        if prior_off and statistics.median(prior_off) >= OFFICIAL_MIN_TESTED:
            out.append(("WARN", "官方段未进终审名单",
                        "今天官方段 0 个进短名单，历史基线中位数 %d 个——"
                        "官方段可能整段不可达" % statistics.median(prior_off)))

    # 3) 验收通过率断崖。
    if today_agg["finalists"] < MIN_FINALISTS:
        out.append(("INFO", "今日终审样本不足",
                    "只测了 %d 个（阈值 %d），不参与比率判定" %
                    (today_agg["finalists"], MIN_FINALISTS)))
    elif len(usable) < MIN_BASELINE_DAYS:
        out.append(("INFO", "风控基线积累中",
                    "有效历史 %d/%d 天，暂不判定（当前通过率 %.0f%%）" %
                    (len(usable), MIN_BASELINE_DAYS, today_agg["ratio"] * 100)))
    else:
        base = [a["ratio"] for a in usable if a["ratio"] is not None]
        base_med = statistics.median(base) if base else 0.0
        r = today_agg["ratio"] or 0.0
        drop_hit = base_med > MIN_BASELINE_RATIO and r < base_med * DROP_FACTOR
        z = _robust_z(r, base)
        z_hit = z is not None and z < -MAD_Z_LIMIT
        if drop_hit or z_hit:
            out.append(("WARN", "终审通过率断崖",
                        "今天 %.0f%%（%d/%d），前 %d 天中位数 %.0f%%%s" %
                        (r * 100, today_agg["pass"], today_agg["finalists"],
                         len(base), base_med * 100,
                         ("，稳健 z=%.1f" % z) if z is not None else "")))
        else:
            out.append(("OK", "终审通过率在基线内",
                        "今天 %.0f%%（%d/%d），前 %d 天中位数 %.0f%%" %
                        (r * 100, today_agg["pass"], today_agg["finalists"],
                         len(base), base_med * 100)))

    # 4) 供给源塌方（今天的源成功率远低于历史）。
    src_base = [a["src_ratio"] for a in prior if a["src_ratio"] is not None]
    if (today_agg["src_ratio"] is not None and src_base and
            today_agg["src_ratio"] < SOURCE_RATIO_WARN and
            statistics.median(src_base) >= SOURCE_RATIO_BASE):
        out.append(("WARN", "供给源大面积失败",
                    "今天源成功率 %.0f%%，历史中位数 %.0f%%" %
                    (today_agg["src_ratio"] * 100,
                     statistics.median(src_base) * 100)))

    # 5) 池子缩水（IP 数掉到历史一半以下）。
    pool_base = [a["pool_ips"] for a in prior if a["pool_ips"]]
    if (today_agg["pool_ips"] is not None and pool_base and
            statistics.median(pool_base) >= 4 and
            today_agg["pool_ips"] <= statistics.median(pool_base) * POOL_SHRINK_FACTOR):
        out.append(("WARN", "生效池明显缩水",
                    "今天 %d 个 IP（中位），历史 %d 个——候选质量或供给出了问题" %
                    (today_agg["pool_ips"], statistics.median(pool_base))))

    # 6) 固定附一条摘要，便于在推送里看到 7 天趋势。
    trend = []
    for k in sorted(store["days"].keys())[-4:]:
        a = aggregate(store["days"][k])
        if not a or not a["finalists"]:
            continue
        trend.append("%s %.0f%%(%d/%d)" % (k[5:], (a["ratio"] or 0) * 100,
                                           a["pass"], a["finalists"]))
    if trend:
        out.append(("INFO", "近 4 天验活趋势", " · ".join(trend)))

    return out


def check(status, path=None, now=None, record_sample=True):
    """巡检入口：记录样本并返回 [(level, title, detail), ...]。"""
    now = now or time.time()
    if record_sample:
        store, _ = record(status, path, now)
    else:
        store = load_store(path)
        today = day_key(now)
        bucket = store["days"].setdefault(today, [])
        bucket.append(extract(status, now))
        prune(store, today)
    return evaluate(store, now)


# ---- 单独运行 -----------------------------------------------------------


def main():
    ap = argparse.ArgumentParser(description="cloudflare-tools 风控模型")
    ap.add_argument("--dry-run", action="store_true", help="不写历史")
    ap.add_argument("--status-file", help="从文件读 /api/status（离线调试）")
    args = ap.parse_args()

    if args.status_file:
        with open(args.status_file, encoding="utf-8") as fh:
            status = json.load(fh)
    else:
        status = http_json(API_BASE + "/api/status")

    for level, title, detail in check(status, record_sample=not args.dry_run):
        print("[%s] %s — %s" % (level, title, detail))
    return 0


if __name__ == "__main__":
    sys.exit(main())
