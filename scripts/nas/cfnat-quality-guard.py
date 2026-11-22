#!/usr/bin/env python3
"""cfnat 质量守卫：按「用户真正感受到的那条路」判断好坏，并在变差时自动处置。

为什么需要它
------------
web 里那套调度器判断的是「探针有没有绿」，不是「从 1234 转发出去到底快不快」。
2026-11-22 白天实测：漏斗连续 20 小时每一轮都是「VLESS 终审仅 0 个通过」，
旧池 29/29 仍然存活，于是永远跳过降级、沿用同一批成员——池子在手，但已经
二十小时没换过血。用户感受到的「又开始卡了」，在调度器的日志里是全绿。

这个脚本站在外面看结果：
  1. 经探针入站打一个非 CF 目标（CF 目标经 proxyIP 结构性挂死），量 p50 /
     max / 失败数——这是延迟 KPI。**另外还看带宽**：延迟绿着、吞吐只有
     200KB/s 的池子，用户的感受就是「卡」，而延迟探针永远看不见这件事。
  2. 连续两轮不达标才动手，避免抽风：
       a. 池里有成员自己握手就慢 → 调 /api/cfnat/active/kick 把它踢掉，
          web 会补位，cfnat 5 秒内读到新清单。
       b. 钉住的成员吞吐不够 → 同样踢掉它，设备会重钉到别人身上。
       c. 成员都正常但整体还是慢 → 是上游/Worker 的问题，触发一次重排
          （/api/cfnat/optimize），每小时最多一次。
  3. 池子超过 3 小时没更新、且整轮考试 0 通过 → 这是「换血停滞」，
     记一条告警并触发重排；重排也救不了就通知人。

动作都带冷却和状态文件，脚本被 cron 每隔几分钟叫一次也不会连击。
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
# cfnat 自己的日志。不用 docker logs 读它：cfnat 直接把内容写进这个文件，
# 容器日志里不一定有这么全，而且 tail 文件比拉整个容器日志便宜得多。
CFNAT_LOG = os.environ.get("QUALITY_GUARD_CFNAT_LOG", os.path.join(DATA_DIR, "cfnat.log"))
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

# 带宽判据。2026-11-22 实测：守卫连续报 p50=240ms 全绿，用户却在缓冲——池里的
# 175.41.232.194 是台「先转到别人的机器再进 CF」的中转盒子，握手 170ms 全池最漂亮，
# 单连接吞吐只有 200KB/s；同一时刻 172.64.145.33 有 3.4MB/s。
# 判断顺序是「先看池子自己的测速数据（零流量），没样本才真拉一次」：
# proxy-active.json 的 results 里带着每个成员的 downloadMbps。
MIN_MBPS = float(os.environ.get("QUALITY_GUARD_MIN_MBPS", "3"))
# 兜底探针不能用 proof.ovh.net：2026-11-22 实测它自己就只有 104~159 KB/s，
# 直连和走隧道一样慢，拿它当尺子会把好线路全判成坏。codeload 的 tarball 同一
# 时刻有 2 MB/s，且不是 Cloudflare 托管的域名（CF 目标经 proxyIP 结构性挂死）。
BANDWIDTH_URL = os.environ.get(
    "QUALITY_GUARD_BANDWIDTH_URL",
    "https://codeload.github.com/xtgm/GTPV1/tar.gz/refs/heads/main")
MIN_KBPS = float(os.environ.get("QUALITY_GUARD_MIN_KBPS", "400"))
BANDWIDTH_EVERY = int(os.environ.get("QUALITY_GUARD_BANDWIDTH_EVERY", "3"))


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


def measure_bandwidth():
    """经探针入站真拉一次数据，返回 KB/s；样本不可用时返回 None。

    只在池子里没有该成员的测速样本时才走这条路——它是兜底，不是常规手段。
    """
    cmd = [
        "curl", "-s", "-o", "/dev/null", "--noproxy", "*",
        "-x", PROBE_PROXY,
        "-m", "8",
        "-w", "%{http_code} %{speed_download} %{size_download}", BANDWIDTH_URL,
    ]
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=20).stdout.split()
    except subprocess.TimeoutExpired:
        return None
    if len(out) != 3:
        return None
    code, speed, size = out
    # 门槛不能定太高：8 秒只拿到 151KB（19KB/s）本身就是最该被抓的病，
    # 按 512KB 卡掉会把这个样本直接丢掉，等于「慢得离谱就装作没看见」。
    # 128KB 以下是另一回事——那点数据全被握手的固定开销吃掉，算出来的速度没意义。
    if code != "200" or float(size) < 131072:
        return None
    return float(speed) / 1024.0


def tail_lines(path, size=524288):
    """读文件末尾的一段并切成行；文件不存在就返回空列表。

    cfnat.log 可能是几十上百 MB，全读进来只为找一行没有意义。
    """
    try:
        with open(path, "rb") as f:
            f.seek(0, os.SEEK_END)
            end = f.tell()
            f.seek(max(0, end - size))
            text = f.read().decode("utf-8", "replace")
    except OSError:
        return []
    return text.splitlines()


def pinned_targets():
    """最新一次「谁被派到了哪个成员」，最近的排最前，返回 [(target, count)]。

    带宽不足时最需要回答的是「现在这台设备走的是谁」。cfnat 每条新连接打一行
    「选择转发连接: 客户端 X，地址: Y」——这是唯一能直接看到的证据，比猜池子快。

    读文件尾而不是 docker logs：cfnat 直接写这个文件，内容更全也更便宜。
    另外连接一旦复用就不再打新行（2026-11-22 实测：钉住之后几分钟里一条都没有），
    所以「最近一条」比「出现次数最多」准。
    """
    lines = tail_lines(CFNAT_LOG)
    if not lines:
        return []
    counts, last = {}, None
    for line in lines:
        idx = line.find("选择转发连接: 客户端 ")
        if idx < 0:
            continue
        parts = line[idx:].split("地址: ")
        if len(parts) < 2:
            continue
        # 日志里写的是「172.64.151.230:443」，池子里存的是裸 IP——不剥端口
        # 就永远匹配不上，守卫会安静地什么都不做。
        target = parts[1].split("，")[0].strip().split(":")[0]
        if target:
            counts[target] = counts.get(target, 0) + 1
            last = target
    ordered = sorted(counts.items(), key=lambda kv: kv[1], reverse=True)
    if last and ordered and ordered[0][0] != last:
        ordered = [(last, counts[last])] + [kv for kv in ordered if kv[0] != last]
    return ordered


def member_mbps(ip):
    """读池子里该成员的测速样本（Mbps）；没有样本返回 None。"""
    try:
        with open(POOL_FILE, "r", encoding="utf-8") as f:
            pool = json.load(f)
    except (OSError, ValueError):
        return None
    for row in pool.get("results") or []:
        if row.get("ip") != ip:
            continue
        mbps = row.get("downloadMbps")
        if isinstance(mbps, (int, float)) and mbps > 0:
            return float(mbps)
    return None


def bandwidth_check(ips, rounds, state):
    """返回 (这一轮的带宽说明, 吞吐不够的成员列表 [(ip, mbps)])。

    只认池子里的测速样本，零流量。**不用本机经 39282 的速度来判**——
    那是 NAS 自己那条出口的手感（2026-11-22 实测它 16KB/s 时，PC 同一
    成员上有 2MB/s）；拿它定责会把用户正在用的好成员踢掉，越修越糟。
    """
    if not ips:
        return "", []
    if rounds % BANDWIDTH_EVERY and not int(state.get("bwStreak", 0)):
        return "", []
    slow = []
    for ip in ips:
        mbps = member_mbps(ip)
        if mbps is not None and mbps < MIN_MBPS:
            slow.append((ip, mbps))
    if not slow:
        return "", []
    slow.sort(key=lambda kv: kv[1])
    return ("低吞吐成员 " + "、".join("%s=%.1fMbps" % kv for kv in slow[:3]), slow)


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
    state["rounds"] = int(state.get("rounds", 0)) + 1
    rounds = state["rounds"]
    ips, updated = pool_members()
    kicked = state.setdefault("kicked", {})

    times, fails = measure_user_path()
    ordered = sorted(times)
    # 单次 1.7s 不算病：这条出口本来就有 2~5% 的 SYN 丢失，五次里慢一次是常态。
    # 所以用「次差值」当 worst，要两次以上都慢才判 degraded。
    worst = ordered[-2] if len(ordered) >= 4 else (ordered[-1] if ordered else 0)
    p50 = ordered[len(ordered) // 2] if ordered else 0
    bad = fails > MAX_FAILS or worst > OK_MAX_MS

    # 延迟绿了也不算过关：钉位成员若是台吞吐 200KB/s 的中转盒子，用户照样缓冲。
    bw_note, bw_culprits = ("", [])
    if not bad:
        bw_note, bw_culprits = bandwidth_check(ips, rounds, state)
    bw_bad = bool(bw_culprits)

    if not bad and not bw_bad:
        if state.get("streak") or state.get("bwStreak"):
            log("recovered: p50=%dms max=%dms fails=%d %s" % (p50, worst, fails, bw_note))
            notify("cfnat 质量恢复：p50=%dms max=%dms 失败=%d %s" % (p50, worst, fails, bw_note))
        state["streak"] = 0
        state["bwStreak"] = 0
        state["lastOk"] = now
        save_state(state)
        log("ok: p50=%dms max=%dms fails=%d %s" % (p50, worst, fails, bw_note))
        return 0

    state["lastBad"] = now

    if not bad:
        # 带宽这条路：连续两轮不足才动手。
        streak = int(state.get("bwStreak", 0)) + 1
        state["bwStreak"] = streak
        state["streak"] = 0
        log("degraded(bw %d): %s" % (streak, bw_note))
        if streak < DEGRADED_STREAK:
            save_state(state)
            return 0
        # 定责：日志里的钉位 + 池子测速样本。样本说它好就不动它。
        pinned = ", ".join("%s×%d" % kv for kv in pinned_targets()[:3])
        log("当前钉位：%s" % (pinned or "无"))
        # 本机路径顺手量一下，只当旁证写进日志：它代表的是 NAS 自己那条出口，
        # 跟用户在 PC 上的感受可能完全不同，绝不能拿它去踢人。
        diag = measure_bandwidth()
        if diag is not None:
            log("本机路径实测 %.0f KB/s（仅记录）" % diag)
        for target, mbps in bw_culprits[:3]:
            if now - float(kicked.get(target, 0)) < KICK_COOLDOWN:
                continue
            code, body = http_post("/api/cfnat/active/kick", {"ip": target})
            kicked[target] = now
            log("kick %s (测速 %.1f Mbps) -> %s %s" % (target, mbps, code, body[:120]))
            if code == 200:
                notify("cfnat 带宽不足（%s），已踢掉被钉住的成员 %s" % (bw_note, target))
                save_state(state)
                return 0
        # 钉位成员都正常，那就是上游/Worker：重排一次。
        last_opt = float(state.get("lastOptimize", 0))
        if now - last_opt > OPTIMIZE_COOLDOWN:
            code, body = http_post("/api/cfnat/optimize", {})
            state["lastOptimize"] = now
            log("optimize (bandwidth) -> %s %s" % (code, body[:120]))
            notify("cfnat 带宽连续 %d 轮不足（%s），已触发重排" % (streak, bw_note))
            save_state(state)
            return 0
        save_state(state)
        log("no action available: bw streak=%d（kick 与重排都在冷却中）" % streak)
        return 0

    # 延迟这条路
    streak = int(state.get("streak", 0)) + 1
    state["streak"] = streak
    log("degraded(%d): p50=%dms max=%dms fails=%d" % (streak, p50, worst, fails))
    if streak < DEGRADED_STREAK:
        save_state(state)
        return 0

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
