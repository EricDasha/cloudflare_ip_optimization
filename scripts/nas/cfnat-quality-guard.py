#!/usr/bin/env python3
"""cfnat 质量守卫：按「用户真正感受到的那条路」判断好坏，并在变差时自动处置。

为什么需要它
------------
web 里那套调度器判断的是「探针有没有绿」，不是「从 1234 转发出去到底快不快」。
2026-11-22 白天实测：漏斗连续 20 小时每一轮都是「VLESS 终审仅 0 个通过」，
旧池 29/29 仍然存活，于是永远跳过降级、沿用同一批成员——池子在手，但已经
二十小时没换过血。用户感受到的「又开始卡了」，在调度器的日志里是全绿。

这个脚本站在外面看结果：
  1. 经 127.0.0.1:1234 打一个非 CF 目标（CF 目标经 proxyIP 结构性挂死），
     量 p50 / max / 失败数——这才是 KPI。
  2. 连续两轮不达标才动手，避免抽风：
       a. 池里有成员自己握手就慢 → 调 /api/cfnat/active/kick 把它踢掉，
          web 会补位，cfnat 5 秒内读到新清单。
       b. 成员都正常但整体还是慢 → 是上游/Worker 的问题，触发一次重排
          （/api/cfnat/optimize），每小时最多一次。
  3. 池子超过 3 小时没更新、且整轮考试 0 通过 → 这是「换血停滞」，
     记一条告警并触发重排；重排也救不了就通知人。

动作都带冷却和状态文件，脚本被 cron 每分钟叫一次也不会连击。
"""

import json
import os
import ssl
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

# 走 sing-box 的探针入站：它才是「客户端 → 1234 → Worker → 目标」这条完整
# 用户路径。直接 --connect-to 打 1234 只到 CF 边缘，SNI 换成一个 CF 不托管的
# 名字（比如 gstatic）会立刻 TLS 失败，测出来的是假的 000。
PROBE_PROXY = os.environ.get("QUALITY_GUARD_PROXY", "http://127.0.0.1:39282")
WEB = os.environ.get("QUALITY_GUARD_WEB", "http://127.0.0.1:8080").rstrip("/")
DATA_DIR = os.environ.get("QUALITY_GUARD_DATA_DIR", "/vol1/1000/Docker/CFNat/data")
POOL_FILE = os.path.join(DATA_DIR, "proxy-active.json")
STATE_FILE = os.path.join(DATA_DIR, "quality-guard-state.json")
LOG_FILE = os.path.join(DATA_DIR, "quality-guard.log")

# 非 CF 目标：这条链路对 CF 自家站点会零字节挂死（见 web/domain_latency.go）。
PROBE_URL = os.environ.get("QUALITY_GUARD_PROBE_URL", "https://www.gstatic.com/generate_204")
SAMPLES = int(os.environ.get("QUALITY_GUARD_SAMPLES", "5"))
# 正常值实测 0.23~0.26s；1.2s 已经意味着首包 SYN 丢了或者上游在排队。
OK_MAX_MS = int(os.environ.get("QUALITY_GUARD_OK_MAX_MS", "1200"))
MAX_FAILS = int(os.environ.get("QUALITY_GUARD_MAX_FAILS", "1"))
# 单个成员握手到这个数以上就算它慢，够得着「踢掉」的门槛。
MEMBER_SLOW_MS = int(os.environ.get("QUALITY_GUARD_MEMBER_SLOW_MS", "800"))
STALE_HOURS = float(os.environ.get("QUALITY_GUARD_STALE_HOURS", "3"))
KICK_COOLDOWN = int(os.environ.get("QUALITY_GUARD_KICK_COOLDOWN", "1800"))
OPTIMIZE_COOLDOWN = int(os.environ.get("QUALITY_GUARD_OPTIMIZE_COOLDOWN", "3600"))
DEGRADED_STREAK = int(os.environ.get("QUALITY_GUARD_DEGRADED_STREAK", "2"))


def log(msg):
    line = "%s %s" % (time.strftime("%Y-%m-%d %H:%M:%S"), msg)
    print(line)
    try:
        with open(LOG_FILE, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except OSError:
        pass


def notify(text):
    """复用看门狗的 Telegram 通道；不可用就只写日志。"""
    path = os.environ.get(
        "QUALITY_GUARD_NOTIFY_MODULE",
        os.path.join(os.path.dirname(os.path.abspath(__file__)), "cfnat-watchdog.py"))
    try:
        # 看门狗文件名带连字符，import 语句用不了，只能按路径装载；
        # 它自己有 __main__ 保护，装载不会触发一轮检查。
        import importlib.util

        spec = importlib.util.spec_from_file_location("cfnat_watchdog_ext", path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        mod.notify({}, text)
    except Exception as exc:  # noqa: BLE001 - 通知失败不能影响处置
        log("notify skipped: %s" % exc)


def load_state():
    try:
        with open(STATE_FILE, "r", encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_state(state):
    tmp = STATE_FILE + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(state, f, ensure_ascii=False, indent=2)
    os.replace(tmp, STATE_FILE)


def http_post(path, payload):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(WEB + path, data=data, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, resp.read(400).decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read(400).decode("utf-8", "replace")
    except Exception as exc:  # noqa: BLE001
        return 0, str(exc)


def http_get(path):
    try:
        with urllib.request.urlopen(WEB + path, timeout=10) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except Exception:  # noqa: BLE001
        return None


def measure_user_path():
    """经 cfnat 打真实请求，返回 (耗时列表ms, 失败数)。"""
    times, fails = [], 0
    for _ in range(SAMPLES):
        cmd = [
            "curl", "-s", "-o", "/dev/null", "--noproxy", "*",
            "-x", PROBE_PROXY,
            "-m", "12", "-w", "%{http_code} %{time_total}", PROBE_URL,
        ]
        try:
            out = subprocess.run(cmd, capture_output=True, text=True, timeout=20).stdout.split()
        except subprocess.TimeoutExpired:
            fails += 1
            continue
        if len(out) != 2:
            fails += 1
            continue
        code, secs = out
        if code not in ("200", "204"):
            fails += 1
            continue
        times.append(int(float(secs) * 1000))
    return times, fails


def pool_members():
    try:
        with open(POOL_FILE, "r", encoding="utf-8") as f:
            pool = json.load(f)
    except (OSError, ValueError):
        return [], None
    ips = [ip for ip in pool.get("ips", []) if ip and ":" not in ip]
    try:
        updated = time.mktime(time.strptime(pool.get("updatedAt", "")[:19], "%Y-%m-%dT%H:%M:%S"))
    except (ValueError, TypeError):
        updated = None
    return ips, updated


def member_handshake_ms(ip, sni):
    """只量到这个成员 :443 的 TLS 握手：不经过 cfnat，专门给「谁在拖」定责。"""
    best = None
    for _ in range(2):
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        start = time.monotonic()
        try:
            with socket.create_connection((ip, 443), timeout=4) as raw:
                with ctx.wrap_socket(raw, server_hostname=sni) as tls:
                    tls.send(b"")
                    ms = int((time.monotonic() - start) * 1000)
        except Exception:  # noqa: BLE001
            return None
        best = ms if best is None else min(best, ms)
    return best


def stale_funnel():
    """近 3 小时里，考试是不是每轮都 0 通过。"""
    try:
        res = subprocess.run(
            ["docker", "logs", "--since", "3h", "cloudflare-tools"],
            capture_output=True, text=True, timeout=60)
        out = res.stdout + res.stderr
    except Exception:  # noqa: BLE001
        return False
    if not out:
        return False
    zero = out.count("终审仅 0 个通过")
    applied = out.count("proxy active pool applied")
    return zero >= 2 and applied == 0


def main():
    state = load_state()
    now = time.time()
    times, fails = measure_user_path()
    ordered = sorted(times)
    # 单次 1.7s 不算病：这条出口本来就有 2~5% 的 SYN 丢失，五次里慢一次是常态。
    # 所以用「次差值」当 worst，要两次以上都慢才判 degraded。
    worst = ordered[-2] if len(ordered) >= 4 else (ordered[-1] if ordered else 0)
    p50 = ordered[len(ordered) // 2] if ordered else 0
    bad = fails > MAX_FAILS or worst > OK_MAX_MS

    if not bad:
        if state.get("streak"):
            log("recovered: p50=%dms max=%dms fails=%d" % (p50, worst, fails))
            notify("cfnat 质量恢复：p50=%dms max=%dms 失败=%d" % (p50, worst, fails))
        state["streak"] = 0
        state["lastOk"] = now
        save_state(state)
        log("ok: p50=%dms max=%dms fails=%d" % (p50, worst, fails))
        return 0

    streak = int(state.get("streak", 0)) + 1
    state["streak"] = streak
    state["lastBad"] = now
    log("degraded(%d): p50=%dms max=%dms fails=%d" % (streak, p50, worst, fails))
    if streak < DEGRADED_STREAK:
        save_state(state)
        return 0

    ips, updated = pool_members()
    if updated and now - updated > STALE_HOURS * 3600:
        log("pool stale: %.1fh since last apply" % ((now - updated) / 3600))
    if stale_funnel():
        log("funnel stalled: 近 3 小时考试 0 通过、池子没有更新")

    # 1) 先找拖后腿的成员。只在「有成员自己握手就慢」时踢人。
    sni = os.environ.get("QUALITY_GUARD_SNI", "vlesdy.trojanjd.dpdns.org")
    slow = []
    for ip in ips[:6]:
        ms = member_handshake_ms(ip, sni)
        if ms is None or ms > MEMBER_SLOW_MS:
            slow.append((ms if ms is not None else 9999, ip))
    slow.sort(reverse=True)
    kicked = state.setdefault("kicked", {})
    for _, ip in slow[:2]:
        if now - float(kicked.get(ip, 0)) < KICK_COOLDOWN:
            continue
        code, body = http_post("/api/cfnat/active/kick", {"ip": ip})
        kicked[ip] = now
        log("kick %s -> %s %s" % (ip, code, body[:120]))
        if code == 200:
            save_state(state)
            return 0

    # 2) 成员都还行，那就是上游/Worker 的事：重排一次，冷却一小时。
    last_opt = float(state.get("lastOptimize", 0))
    if now - last_opt > OPTIMIZE_COOLDOWN:
        code, body = http_post("/api/cfnat/optimize", {})
        state["lastOptimize"] = now
        log("optimize -> %s %s" % (code, body[:120]))
        notify("cfnat 连续 %d 轮变慢（p50=%dms max=%dms 失败=%d），已触发重排" % (streak, p50, worst, fails))
        save_state(state)
        return 0

    save_state(state)
    log("no action available: streak=%d (kick 与重排都在冷却中)" % streak)
    return 0


if __name__ == "__main__":
    sys.exit(main())
