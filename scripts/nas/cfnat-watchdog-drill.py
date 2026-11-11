#!/usr/bin/env python3
"""
LIVE DRILL for the cfnat watchdog.

Why not just stop the pool: sing-box's WS path embeds `proxyip=<domain>`,
and when the pool dies sing-box silently re-dials through that domain, so
the tunnel never actually goes down. Stopping the pool therefore proves
nothing. Same for pointing `server` at a dead IP.

The sabotage writes a config that CANNOT fall back:
  - vless server -> 192.168.1.50:1 (closed port, instant refusal)
  - ws path      -> "/drill-no-proxyip" (proxyip= parameter removed)

Phases: baseline -> sabotage -> watchdog detects + fails over to a fallback
domain + TG alert -> restore the real config -> watchdog switches back.
The original config is restored in a finally block no matter what.
"""
import json
import os
import subprocess
import sys
import time

SB_DIR = "/vol1/1000/Docker/sing-box"
CFG = f"{SB_DIR}/config.json"
CFG_BAK = f"{SB_DIR}/config.json.drill-bak"
STATE = f"{SB_DIR}/watchdog-state.json"
# sudo 口令只从运行时环境读，绝不写进仓库。未设 NAS_SUDO_PW 时退回
# `sudo -n`（要求已配 NOPASSWD，否则脚本会在第一次提权时明确失败）。
# 有口令时 sudo -S 从 stdin 读口令：旧写法 `echo 口令 | sudo -S` 把口令
# 拼进 sh -c 命令行，/proc/<pid>/cmdline 里明文可见。
_SUDO_PW = os.environ.get("NAS_SUDO_PW", "")
SUDO = "sudo -S -p '' " if _SUDO_PW else "sudo -n "


def run(cmd, timeout=90):
    kwargs = dict(capture_output=True, text=True, timeout=timeout)
    if _SUDO_PW and cmd.startswith("sudo -S "):
        kwargs["input"] = _SUDO_PW + "\n"
    return subprocess.run(cmd, shell=True, **kwargs)


def read_state():
    try:
        return json.load(open(STATE))
    except Exception as e:
        return {"err": str(e)}


def tail_log(n=8):
    return run(SUDO + f"tail -{n} {SB_DIR}/watchdog.log").stdout.strip()


def tunnel_ok():
    """Healthy = exit IP through the proxy differs from our own direct IP."""
    r = run("curl -s -m 20 -x http://127.0.0.1:39281 "
            "https://www.cloudflare.com/cdn-cgi/trace", timeout=30)
    eip = next((l[3:].strip() for l in r.stdout.splitlines() if l.startswith("ip=")), "")
    if not eip:
        return False, "no-egress"
    r = run("curl -s -m 10 https://www.cloudflare.com/cdn-cgi/trace", timeout=20)
    dip = next((l[3:].strip() for l in r.stdout.splitlines() if l.startswith("ip=")), "")
    if dip and eip == dip:
        return False, f"leaked-direct({eip})"
    return True, eip


def patch_vless(server, port, path):
    d = json.load(open(CFG))
    for o in d["outbounds"]:
        if o.get("type") == "vless":
            o["server"] = server
            o["server_port"] = port
            o.get("transport", {})["path"] = path
    json.dump(d, open(CFG, "w"), indent=2)
    run(SUDO + "docker restart sing-box", timeout=120)
    time.sleep(10)


def restore():
    if os.path.exists(CFG_BAK):
        # 恢复 cp 失败必须停下：旧代码不看返回值，cp 失败仍重启旧配置
        # 并删掉唯一备份，原配置永久丢失。
        cp = run(SUDO + f"cp {CFG_BAK} {CFG}")
        if cp.returncode != 0:
            print(f"RESTORE FAILED: cp backup back failed (rc={cp.returncode}), keeping backup {CFG_BAK}")
            sys.exit(1)
        run(SUDO + "docker restart sing-box", timeout=120)
        time.sleep(10)
        run(SUDO + f"rm -f {CFG_BAK}")


print("=== 0) baseline ===")
ok, info = tunnel_ok()
print("tunnel:", ("OK via " + info) if ok else ("DOWN: " + info))
print("state:", read_state())
if not ok:
    print("ABORT: baseline tunnel is not healthy - refusing to drill")
    sys.exit(1)

# stash the pristine config (watchdog never touches this file name)
run(SUDO + f"cp {CFG} {CFG_BAK}")
orig = json.load(open(CFG))
ov = [o for o in orig["outbounds"] if o.get("type") == "vless"][0]
orig_path = ov.get("transport", {}).get("path", "")
print("original vless:", ov["server"], ov["server_port"], "path=" + orig_path)

switched = back = False
try:
    print("\n=== 1) sabotage: closed port + strip proxyip fallback ===")
    # 路径只需「不含 proxyip= 参数」即可，不必用真实节点路径——
    # 真实 WS path 属于 VLESS 模板，不进仓库。
    patch_vless("192.168.1.50", 1, "/drill-no-proxyip")
    ok, info = tunnel_ok()
    print("tunnel after sabotage:", ("STILL OK via " + info + " (sabotage failed!)") if ok else "DOWN as intended")
    if ok:
        raise RuntimeError("sabotage did not break the tunnel")

    print("\n=== 2) wait for watchdog failover (needs 2 consecutive dead probes) ===")
    deadline = time.time() + 480
    while time.time() < deadline:
        time.sleep(20)
        st = read_state()
        ok, info = tunnel_ok()
        print(f"  {time.strftime('%H:%M:%S')} mode={st.get('mode')} fb_idx={st.get('fb_idx')} "
              f"dead={st.get('dead_consec')} tunnel={'OK ' + info if ok else 'down ' + info}")
        if st.get("mode") == "fallback" and ok:
            switched = True
            break
    print("FAILOVER:", "SUCCESS" if switched else "TIMEOUT")
    print(tail_log(12))

    print("\n=== 3) pool is healthy again - watchdog must decide to switch back itself ===")
    print("    (nothing is restored here; the watchdog compares the pool cert and rewrites)")
    ok, info = tunnel_ok()
    print("tunnel still on fallback:", ("OK via " + info) if ok else ("DOWN: " + info))
    deadline = time.time() + 300
    while time.time() < deadline:
        time.sleep(25)
        st = read_state()
        cur = json.load(open(CFG))
        cv = [o for o in cur["outbounds"] if o.get("type") == "vless"][0]
        print(f"  {time.strftime('%H:%M:%S')} mode={st.get('mode')} consec={st.get('consec_ok')} "
              f"target={cv['server']}:{cv['server_port']}")
        if st.get("mode") == "pool":
            back = True
            break
    print("SWITCH-BACK:", "SUCCESS" if back else "TIMEOUT")
    print(tail_log(12))
finally:
    restore()

print("\n=== 4) final ===")
print("state:", read_state())
ok, info = tunnel_ok()
print("tunnel:", ("OK via " + info) if ok else ("DOWN: " + info))
verdict = switched and back and ok
print("\nDRILL VERDICT:", "PASS" if verdict else "FAIL")
sys.exit(0 if verdict else 1)
