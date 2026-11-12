#!/usr/bin/env python3
"""
cfnat-watchdog: keep the sing-box tunnel alive and Telegram-alerted.

- Pool mode: vless outbound -> cfnat (192.168.1.50:1234, rides the CFNat pool)
- Fallback mode: vless outbound -> one of the verified CF direct-entry domains

Every run (cron, 1/min):
  1. probe tunnel through sing-box (gstatic 204)
  2. healthy + pool mode          -> done
     healthy + fallback mode      -> probe pool directly (cert check);
                                     2 consecutive OK -> switch back + TG alert
  3. dead   -> debounce (2 consecutive dead runs), then rotate fallback
     entries directly; trying a fallback IS the node liveness test
     (it only succeeds if the node is up) -> TG alert on switch
     (30 min cooldown). There is no separate "node down" alert:
     the old "probe the node first" gate was removed — when the tunnel
     is down this host's direct path to Cloudflare is blocked as well,
     so the gate could never pass, and scanning the candidates cost
     minutes and overran the 1-minute cron window.

TG alerts go through the SAME bot openclaw uses (token read at runtime from
openclaw.json, never logged). The proxy is restored BEFORE sending, so the
alert can actually get out.

Logs: watchdog.log (self-capped). State: watchdog-state.json.
"""
import fcntl
import json
import os
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

SB_DIR = "/vol1/1000/Docker/sing-box"
CFG = f"{SB_DIR}/config.json"
CFG_BAK_DIRECT = f"{SB_DIR}/config.json.bak-ox-direct"
STATE_F = f"{SB_DIR}/watchdog-state.json"
LOG_F = f"{SB_DIR}/watchdog.log"
LOCK_F = f"{SB_DIR}/watchdog.lock"

POOL_SERVER, POOL_PORT = "192.168.1.50", 1234
PROXY = "http://127.0.0.1:39281"
# MUST be a site that does NOT hit the geosite-cn/geoip-cn direct rules -
# gstatic.com resolves to a CN-reachable edge and the probe silently goes
# out via `direct`, so it can never detect a dead tunnel (learned the hard way).
PROBE_URL = "https://www.google.com/generate_204"
TRACE_URL = "https://www.cloudflare.com/cdn-cgi/trace"

# all verified 2026-10-24: every combo hands back the node cert with node SNI
FALLBACKS = [
    ("op.chinwa.eu.cc", 443),
    ("mfa.gov.ua", 443),
    ("www.shopify.com", 443),
    ("www.deepl.com", 443),
    ("store.ubi.com", 443),
    ("spark.cf.090227.xyz", 443),
    ("op.chinwa.eu.cc", 2053),
    ("mfa.gov.ua", 2053),
    ("www.shopify.com", 2053),
    ("www.deepl.com", 2053),
    ("store.ubi.com", 2053),
    ("spark.cf.090227.xyz", 2053),
]
# One attempt per run: a restart + probe costs ~40s and cron fires every
# minute, so more than one attempt would make runs overlap and starve the
# next tick. Rotation advances across runs instead (fb_idx persists).
ATTEMPTS_PER_RUN = 1
RECOVERY_CONSEC = 2
ALERT_COOLDOWN = 1800  # seconds between identical alerts
RESTART_SETTLE = 10    # seconds after docker restart before probing
# CF direct-entry handshakes routinely take 1.5-7s from CN; an 8s cap made
# ~70% of probes read as "dead". 20s + 2 consecutive failures to debounce.
PROBE_TIMEOUT = 20
DEAD_CONSEC = 2

OC_CFG = "/vol2/@apphome/trim.openclaw/data/home/.openclaw/openclaw.json"
DEFAULT_CHAT_ID = "8975840695"
# Every working entry (pool or fallback domain) presents this cert in its
# subject CN - it is how an entry proves it actually reached the node.
NODE_CERT_CN = b"trojanjd.dpdns.org"


def log(msg):
    line = time.strftime("%Y-%m-%d %H:%M:%S") + " " + msg
    try:
        with open(LOG_F, "a") as f:
            f.write(line + "\n")
        if os.path.getsize(LOG_F) > 300_000:
            data = open(LOG_F, "rb").read()
            open(LOG_F, "wb").write(data[-150_000:])
    except Exception:
        pass


def run(cmd, timeout=30):
    return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)


def load_state():
    try:
        return json.load(open(STATE_F))
    except Exception:
        return {"mode": "pool", "consec_ok": 0, "fb_idx": 0, "last_alert": 0,
                "last_kind": "", "dead_consec": 0}


def save_state(st):
    # 原子写：先写临时文件再 rename。直接 open(STATE_F, "w") 写一半崩了，
    # 状态文件损坏，下次 load_state 回落默认值，mode/fb_idx 全丢。
    # 与 cf_inspect.py 的 _queue_save 同一做法。
    tmp = STATE_F + ".tmp"
    with open(tmp, "w") as f:
        json.dump(st, f, indent=2)
    os.replace(tmp, STATE_F)


def read_cfg():
    return json.load(open(CFG))


def vless_of(d):
    for o in d.get("outbounds", []):
        if o.get("type") == "vless":
            return o
    raise RuntimeError("no vless outbound in config")


def sni_of(d):
    v = vless_of(d)
    return (v.get("tls") or {}).get("server_name", "")


def current_target():
    v = vless_of(read_cfg())
    return v.get("server"), v.get("server_port")


def probe_tunnel(timeout=PROBE_TIMEOUT):
    try:
        r = run(["curl", "-s", "-o", "/dev/null", "-m", str(timeout), "-x", PROXY,
                 "-w", "%{http_code}", PROBE_URL], timeout=timeout + 6)
    except subprocess.TimeoutExpired:
        # 超时即探针失败：旧代码让 TimeoutExpired 一路抛到 cron 主流程，
        # 整轮崩掉，本轮 dead_consec 白计。
        return False
    return r.stdout.strip() == "204"


def egress_ip(timeout=PROBE_TIMEOUT):
    """Exit IP as seen through the tunnel; empty on failure."""
    try:
        r = run(["curl", "-s", "-m", str(timeout), "-x", PROXY, TRACE_URL],
                timeout=timeout + 6)
    except subprocess.TimeoutExpired:
        return ""
    for line in r.stdout.splitlines():
        if line.startswith("ip="):
            return line[3:].strip()
    return ""


_DIRECT_IP = None


def direct_ip(timeout=10):
    """This host's own public IP (no proxy), used to prove the tunnel is real.
    Cached per process - it appears in every health check."""
    global _DIRECT_IP
    if _DIRECT_IP is not None:
        return _DIRECT_IP
    try:
        r = run(["curl", "-s", "-m", str(timeout), TRACE_URL], timeout=timeout + 6)
    except subprocess.TimeoutExpired:
        return ""
    for line in r.stdout.splitlines():
        if line.startswith("ip="):
            _DIRECT_IP = line[3:].strip()
            return _DIRECT_IP
    return ""


def tunnel_health():
    """(healthy, detail). Healthy means: probe OK *and* the exit IP seen
    through the proxy differs from this host's own IP - otherwise the
    request leaked out via the `direct` outbound and proved nothing."""
    if not probe_tunnel():
        return False, "probe failed"
    eip = egress_ip()
    if not eip:
        return False, "no egress ip"
    dip = direct_ip()
    if dip and eip == dip:
        return False, f"leaked direct (ip={eip})"
    return True, f"via {eip}"


def cert_ok(host, port, sni, timeout=8):
    """TLS cert probe: a usable endpoint hands back the node's real cert.

    Done in-process rather than via `openssl s_client | openssl x509`: the
    shell pipeline left a reader hanging on a dead socket and the watchdog
    wedged on it for minutes, so the pool was never seen as recovered.
    A socket with a hard timeout cannot do that.

    The subject CN is read from the parsed cert (exact match on
    commonName, or an exact DNS SAN) - no temp files, no pipelining.

    The handshake itself verifies the chain and the hostname against
    the system CA store (CERT_REQUIRED + check_hostname with the
    config's SNI): a self-signed cert with a matching CN no longer
    passes. The exact CN/SAN match below stays as a second layer -
    it proves the endpoint is this node, not just any host with a
    valid cert for the SNI.
    """
    ctx = ssl.create_default_context()
    ctx.check_hostname = True
    ctx.verify_mode = ssl.CERT_REQUIRED
    try:
        with socket.create_connection((host, int(port)), timeout=timeout) as sock:
            with ctx.wrap_socket(sock, server_hostname=sni) as tls:
                der = tls.getpeercert(binary_form=True)
    except (OSError, ssl.SSLError, ValueError):
        return False
    # 精确比对 subject CN（或 SAN DNS）：旧写法 `CN in der` 是 DER
    # 子串匹配，证书任一字段含该字节串即通过。链与主机名已由握手校验，
    # 这里再精确比对一次，证明对端是本节点而非任意持有该 SNI 有效
    # 证书的主机。DER 直接解析：getpeercert() 字典只在校验通过后可信。
    if not der:
        return False
    want = NODE_CERT_CN.decode("utf-8", "ignore")
    cns, sans = _cert_names(der)
    return want in cns or want in sans


def _der_read(data, pos):
    """读一个 DER TLV 头，返回 (tag, value_start, value_len)。"""
    tag = data[pos]
    pos += 1
    if tag & 0x1F == 0x1F:  # 多字节 tag（本处用不到，防御性处理）
        while True:
            b = data[pos]
            pos += 1
            if not b & 0x80:
                break
    length = data[pos]
    pos += 1
    if length & 0x80:
        n = length & 0x7F
        if n == 0 or n > 4:
            raise ValueError("bad DER length")
        length = int.from_bytes(data[pos:pos + n], "big")
        pos += n
    if pos + length > len(data):
        raise ValueError("DER overrun")
    return tag, pos, length


def _der_children(data, start, end):
    pos = start
    while pos < end:
        tag, vs, ln = _der_read(data, pos)
        yield tag, vs, vs + ln
        pos = vs + ln


def _cert_names(der):
    """从 DER 证书里精确取出 (subject CN 列表, SAN DNS 列表)。解析失败返回 ([], [])。"""
    try:
        tag, vs, ln = _der_read(der, 0)
        if tag != 0x30:  # Certificate SEQUENCE
            return [], []
        tbs_tag, tbs_vs, tbs_ln = _der_read(der, vs)
        if tbs_tag != 0x30:
            return [], []
        kids = list(_der_children(der, tbs_vs, tbs_vs + tbs_ln))
        idx = 0
        if kids and kids[0][0] == 0xA0:  # [0] version
            idx = 1
        # serial, signature, issuer, validity, subject
        if len(kids) < idx + 5:
            return [], []
        _, subj_vs, subj_end = kids[idx + 4]
        cns = []
        for stag, svs, send in _der_children(der, subj_vs, subj_end):
            if stag != 0x31:  # SET
                continue
            for atag, avs, aend in _der_children(der, svs, send):
                if atag != 0x30:  # AttributeTypeAndValue SEQUENCE
                    continue
                parts = list(_der_children(der, avs, aend))
                if len(parts) != 2:
                    continue
                otag, ovs, oend = parts[0]
                vtag, vvs, vend = parts[1]
                if otag == 0x06 and der[ovs:oend] == b"\x55\x04\x03":  # OID 2.5.4.3 commonName
                    cns.append(_der_decode_string(vtag, der[vvs:vend]))
        sans = []
        for tag, evs, eend in kids:
            if tag != 0xA3:  # [3] extensions
                continue
            # [3] 的内容本身是一个 SEQUENCE，扩展是它的子项。
            stag, svs, sln = _der_read(der, evs)
            if stag != 0x30:
                continue
            for xtag, xvs, xend in _der_children(der, svs, svs + sln):
                if xtag != 0x30:
                    continue
                xparts = list(_der_children(der, xvs, xend))
                if not xparts or xparts[0][0] != 0x06:
                    continue
                otag, ovs, oend = xparts[0]
                if der[ovs:oend] != b"\x55\x1d\x11":  # OID 2.5.29.17 subjectAltName
                    continue
                val_tag, val_vs, val_end = xparts[-1]  # OCTET STRING
                if val_tag != 0x04:
                    continue
                inner = der[val_vs:val_end]
                itag, ivs, iln = _der_read(inner, 0)
                if itag != 0x30:
                    continue
                for gtag, gvs, gend in _der_children(inner, ivs, ivs + iln):
                    if gtag == 0x82:  # [2] dNSName IA5String
                        sans.append(inner[gvs:gend].decode("ascii", "ignore"))
        return cns, sans
    except (ValueError, IndexError):
        return [], []


def _der_decode_string(tag, raw):
    if tag == 0x1E:  # BMPString
        return raw.decode("utf-16-be", "ignore")
    if tag in (0x0C, 0x16):  # UTF8String, IA5String
        return raw.decode("utf-8", "ignore")
    return raw.decode("ascii", "ignore")


def set_target(server, port):
    d = read_cfg()
    v = vless_of(d)
    v["server"] = server
    v["server_port"] = port
    # 原子写：先写临时文件再 rename，与 save_state 同款。直接覆盖写一半
    # 崩了（断电/OOM），config.json 被截断，sing-box 下次启动解析失败，
    # 隧道彻底停摆，需人工从备份恢复。
    tmp = CFG + ".tmp"
    with open(tmp, "w") as f:
        json.dump(d, f, indent=2)
    os.replace(tmp, CFG)
    r = run(["docker", "restart", "sing-box"], timeout=120)
    if r.returncode != 0:
        log(f"docker restart failed: {r.stderr[:120]}")
        return False
    time.sleep(RESTART_SETTLE)
    return True


def tg_credentials():
    try:
        d = json.load(open(OC_CFG))
        tgc = d.get("channels", {}).get("telegram", {})
        token = tgc.get("botToken", "")
        allow = tgc.get("allowFrom") or []
        chat = str(allow[0]) if allow else DEFAULT_CHAT_ID
        return token, chat
    except Exception as e:
        log(f"tg_credentials error: {e}")
        return "", DEFAULT_CHAT_ID


def tg_send(text):
    """Send a Telegram message. Returns False if it could not go out now."""
    token, chat = tg_credentials()
    if not token:
        log("tg_send: no bot token available")
        return False
    body = json.dumps({"chat_id": chat, "text": text}).encode()
    url = "https://api.telegram.org/bot" + token + "/sendMessage"
    for proxy in (PROXY, None):  # proxy first; direct as last resort
        handlers = []
        if proxy:
            handlers.append(urllib.request.ProxyHandler(
                {"http": proxy, "https": proxy}))
        opener = urllib.request.build_opener(*handlers)
        req = urllib.request.Request(
            url, data=body,
            headers={"Content-Type": "application/json"}, method="POST")
        try:
            with opener.open(req, timeout=15) as resp:
                out = json.loads(resp.read().decode())
            if out.get("ok"):
                log(f"tg_send OK via {'proxy' if proxy else 'direct'}")
                return True
            log(f"tg_send api error: {str(out)[:120]}")
        except Exception as e:
            log(f"tg_send fail via {'proxy' if proxy else 'direct'}: {str(e)[:100]}")
    return False


def notify(st, text):
    """Queue-aware alert. Telegram is unreachable while the tunnel is down,
    so anything that fails to send is kept and flushed on a later run once
    the tunnel is back. Capped so a long outage cannot grow the state file."""
    if tg_send(text):
        return True
    q = st.setdefault("pending", [])
    q.append({"t": int(time.time()), "text": text})
    del q[:-20]
    log(f"alert queued ({len(q)} pending)")
    return False


def flush_pending(st):
    q = st.get("pending") or []
    if not q:
        return
    # one flush per run so a backlog cannot turn into a flood
    item = q.pop(0)
    age = int((time.time() - item.get("t", time.time())) // 60)
    text = f"[补发·{age}分钟前] " + item.get("text", "")
    if tg_send(text):
        log(f"pending alert flushed ({len(q)} left)")
    else:
        q.insert(0, item)


def main():
    lock = open(LOCK_F, "w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        return  # previous run still going

    st = load_state()
    sni = sni_of(read_cfg())
    cur_server, cur_port = current_target()

    # ---- 1) tunnel healthy? ----
    healthy, detail = tunnel_health()
    if healthy:
        st["dead_consec"] = 0
        flush_pending(st)  # the tunnel works again - deliver what we owed
        if st.get("mode") == "fallback":
            if cert_ok(POOL_SERVER, POOL_PORT, sni):
                st["consec_ok"] = st.get("consec_ok", 0) + 1
                if st["consec_ok"] >= RECOVERY_CONSEC:
                    log("pool recovered, switching back to cfnat entry")
                    if set_target(POOL_SERVER, POOL_PORT) and tunnel_health()[0]:
                        st["mode"] = "pool"
                        st["consec_ok"] = 0
                        notify(st, "✅ cfnat 池已恢复（连续探测通过），sing-box 已切回 "
                                f"{POOL_SERVER}:{POOL_PORT}，兜底入口下线。")
                    else:
                        st["consec_ok"] = 0
                        log("switch-back failed; staying on fallback")
            else:
                st["consec_ok"] = 0
            save_state(st)
        else:
            st["consec_ok"] = 0
            save_state(st)
        return

    # ---- 2) tunnel dead -> rotate fallback entries directly ----
    # There used to be a "probe the node first" gate here. It was wrong twice
    # over: (a) when the tunnel is down this host's direct path to Cloudflare
    # is blocked as well, so the gate could never pass; (b) scanning the
    # candidates cost minutes and overran the 1-minute cron window. Trying a
    # fallback IS the node liveness test - it only succeeds if the node is up.
    # Debounce: one slow probe is normal on CF direct-entry.
    st["dead_consec"] = st.get("dead_consec", 0) + 1
    save_state(st)
    if st["dead_consec"] < DEAD_CONSEC:
        log(f"tunnel probe failed ({cur_server}:{cur_port}) [{detail}], "
            f"{st['dead_consec']}/{DEAD_CONSEC} - waiting for confirmation")
        return
    log(f"tunnel DEAD via {cur_server}:{cur_port} ({st['dead_consec']} consecutive) [{detail}]")

    # node alive -> rotate fallbacks
    tried = 0
    for i in range(len(FALLBACKS)):
        idx = (st.get("fb_idx", 0) + i) % len(FALLBACKS)
        dom, port = FALLBACKS[idx]
        log(f"trying fallback {dom}:{port}")
        if not set_target(dom, port):
            st["fb_idx"] = (idx + 1) % len(FALLBACKS)
            save_state(st)
            continue
        fb_ok, fb_detail = tunnel_health()
        if fb_ok:
            st["mode"] = "fallback"
            st["fb_idx"] = (idx + 1) % len(FALLBACKS)
            st["consec_ok"] = 0
            log(f"fallback {dom}:{port} carries the tunnel [{fb_detail}]")
            now = time.time()
            if now - st.get("last_alert", 0) > ALERT_COOLDOWN:
                if notify(st, "⚠️ cfnat 隧道不通，sing-box 已切换兜底入口 " + dom + ":" + str(port) +
                           f"（候选 {idx + 1}/{len(FALLBACKS)}），代理与 NewAPI 上游已恢复。"
                           "池恢复后自动切回。（30 分钟冷却）"):
                    st["last_alert"] = now
            save_state(st)
            return
        st["fb_idx"] = (idx + 1) % len(FALLBACKS)
        save_state(st)
        tried += 1
        if tried >= ATTEMPTS_PER_RUN:
            log("attempt cap reached this run; next run continues rotation")
            break

    save_state(st)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        log(f"watchdog crashed: {type(e).__name__} {e}")
        sys.exit(1)
