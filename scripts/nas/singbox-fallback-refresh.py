#!/usr/bin/env python3
"""刷新 sing-box 的机场兜底节点（urltest 组 auto 的 fb* 成员）。

背景：cfnat 出口（NAS:1234）是主通道，机场节点只在主通道彻底失效时兜底，
用于让 cf_inspect.py 还能把「当天 GFW 风控加强」的告警发出去。

行为：
  1. 拉取 ripaojiedian（多个镜像）与 laziji 订阅；直连失败再走本机 39281，最后读本地缓存；
  2. 先做 TCP 预筛（连不上就不起 sing-box，省掉死节点的等待）；
  3. 第一轮实测 gfw/tg；存活者第二轮复测，两轮都通才算稳定，延迟取两轮最大值；
  4. 前 10 个下载 1MB 测吞吐，低于 0.3 Mbps 淘汰（测不出保留，不据未知判死）；
  5. 按延迟排序，同类型最多 3 个、总量最多 8 个；
  6. 存活 < 2 个时保持现有 fb* 不动（宁可旧也不空），并发 Telegram 通知；
  7. 新配置先 `sing-box check` 验证，通过才写盘并重启容器，成功后推送刷新结果。

用法（NAS 上）：
  /usr/bin/python3 /vol1/1000/Docker/sing-box/scripts/singbox-fallback-refresh.py
  # 只看结果不落盘：加 --dry-run
"""
import base64
import json
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = '/vol1/1000/Docker/sing-box'
CFG = os.path.join(BASE, 'config.json')
SB = os.path.join(BASE, 'sing-box')
IMAGE = 'sing-box-sing-box:latest'
SUBS = [
    ('rip', ['https://raw.githubusercontent.com/ripaojiedian/freenode/main/sub',
             'https://down.nigx.cn/raw.githubusercontent.com/ripaojiedian/freenode/main/sub',
             'https://cdn.jsdelivr.net/gh/ripaojiedian/freenode@main/sub',
             'https://ghfast.top/https://raw.githubusercontent.com/ripaojiedian/freenode/main/sub',
             'https://gh-proxy.com/https://raw.githubusercontent.com/ripaojiedian/freenode/main/sub',
             'https://raw.kkgithub.com/ripaojiedian/freenode/main/sub']),
    ('lazi', ['https://gy.xiaozi.us.kg/sub?token=laziji']),
]
CACHE = os.path.join(BASE, 'scripts', 'cache')
PROXY = 'http://127.0.0.1:39281'
PREF = ('香港', 'HK', '日本', 'JP', '新加坡', 'SG', '台湾', 'TW', '韩国', 'KR', '美国', 'US')
TARGETS = [('gfw', 'https://www.gstatic.com/generate_204'),
           ('tg', 'https://api.telegram.org/')]
WORKERS = 12
MAX_CANDIDATES = 120
MAX_PICKED = 8
MAX_PER_TYPE = 3
MAX_SPEED_TESTS = 10
# 兜底只用来发告警文本，几十 kbps 就够；这里只淘汰「几乎不能传数据」的节点，
# 不拿带宽当准入门槛（真正决定顺序的是延迟）。
MIN_SPEED_MBPS = 0.05
SPEED_URL = 'https://speed.cloudflare.com/__down?bytes=1000000'
# 告警/通知：与 cf_inspect.py 用同一个 bot，凭据从 openclaw 配置读，读不到就静默跳过
OPENCLAW_CONFIG = os.environ.get(
    'CF_OPENCLAW_CONFIG',
    '/vol2/@apphome/trim.openclaw/data/home/.openclaw/openclaw.json')
DRY = '--dry-run' in sys.argv

CTX = ssl.create_default_context()
CTX.check_hostname = False
CTX.verify_mode = ssl.CERT_NONE


def b64d(s):
    s = s.strip().replace('-', '+').replace('_', '/')
    s += '=' * (-len(s) % 4)
    return base64.b64decode(s)


def qs(u):
    return dict(urllib.parse.parse_qsl(u.query))


def transport(net, host, path, service_name=None):
    net = (net or 'tcp').lower()
    if net == 'ws':
        t = {'type': 'ws', 'path': path or '/'}
        if host:
            t['headers'] = {'Host': host}
        return t
    if net == 'grpc':
        return {'type': 'grpc', 'service_name': service_name or path or ''}
    if net in ('h2', 'http'):
        return {'type': 'http', 'host': [host] if host else [], 'path': path or '/'}
    return None


def vless(line):
    u = urllib.parse.urlparse(line)
    p = qs(u)
    o = {'type': 'vless', 'server': u.hostname,
         'server_port': int(u.port or 443), 'uuid': u.username}
    if p.get('flow'):
        o['flow'] = p['flow']
    sec = (p.get('security') or 'none').lower()
    if sec in ('tls', 'reality', 'xtls'):
        tls = {'enabled': True, 'server_name': p.get('sni') or p.get('host') or u.hostname}
        if p.get('fp'):
            tls['utls'] = {'enabled': True, 'fingerprint': p['fp']}
        if p.get('alpn'):
            tls['alpn'] = [a for a in p['alpn'].split(',') if a]
        if sec == 'reality':
            tls['reality'] = {'enabled': True, 'public_key': p.get('pbk', ''),
                              'short_id': p.get('sid', '')}
        o['tls'] = tls
    tr = transport(p.get('type'), p.get('host'), p.get('path'), p.get('serviceName'))
    if tr:
        o['transport'] = tr
    return o


def vmess(line):
    d = json.loads(b64d(line[8:]))
    o = {'type': 'vmess', 'server': d['add'], 'server_port': int(d['port']),
         'uuid': d['id'], 'security': d.get('scy') or 'auto',
         'alter_id': int(d.get('aid') or 0)}
    if (d.get('tls') or '').lower() == 'tls':
        tls = {'enabled': True, 'server_name': d.get('sni') or d.get('host') or d['add']}
        if d.get('alpn'):
            tls['alpn'] = [a for a in d['alpn'].split(',') if a]
        o['tls'] = tls
    tr = transport(d.get('net'), d.get('host'), d.get('path'),
                   d.get('path') if (d.get('net') or '').lower() == 'grpc' else None)
    if tr:
        o['transport'] = tr
    return o


def ss(line):
    body = line[5:]
    if '#' in body:
        body = body.split('#', 1)[0]
    if '@' in body:
        userinfo, hostport = body.rsplit('@', 1)
        try:
            method, password = b64d(userinfo).decode().split(':', 1)
        except Exception:
            method, password = urllib.parse.unquote(userinfo).split(':', 1)
    else:
        userinfo, hostport = b64d(body).decode().rsplit('@', 1)
        method, password = userinfo.split(':', 1)
    host, port = hostport.rsplit(':', 1)
    return {'type': 'shadowsocks', 'server': host.strip('[]'),
            'server_port': int(port), 'method': method, 'password': password}


def tuic(line):
    u = urllib.parse.urlparse(line)
    p = qs(u)
    return {'type': 'tuic', 'server': u.hostname, 'server_port': int(u.port),
            'uuid': u.username, 'password': urllib.parse.unquote(u.password or ''),
            'congestion_control': p.get('congestion_control') or 'bbr',
            'udp_relay_mode': p.get('udp_relay_mode') or 'native',
            'tls': {'enabled': True, 'server_name': p.get('sni') or u.hostname,
                    'alpn': (p.get('alpn') or 'h3').split(',')}}


def hysteria2(line):
    u = urllib.parse.urlparse(line)
    p = qs(u)
    tls = {'enabled': True, 'server_name': p.get('sni') or u.hostname}
    if p.get('insecure') in ('1', 'true'):
        tls['insecure'] = True
    if p.get('alpn'):
        tls['alpn'] = [a for a in p['alpn'].split(',') if a]
    o = {'type': 'hysteria2', 'server': u.hostname,
         'server_port': int(u.port or 443),
         'password': urllib.parse.unquote(u.username or ''), 'tls': tls}
    if p.get('obfs'):
        o['obfs'] = {'type': p['obfs'],
                     'password': urllib.parse.unquote(p.get('obfs-password') or '')}
    return o


def anytls(line):
    u = urllib.parse.urlparse(line)
    p = qs(u)
    tls = {'enabled': True, 'server_name': p.get('sni') or u.hostname}
    if p.get('insecure') in ('1', 'true'):
        tls['insecure'] = True
    return {'type': 'anytls', 'server': u.hostname,
            'server_port': int(u.port or 443),
            'password': urllib.parse.unquote(u.username or ''), 'tls': tls}


PARSERS = {'vless://': vless, 'vmess://': vmess, 'ss://': ss, 'tuic://': tuic,
           'hysteria2://': hysteria2, 'hy2://': hysteria2, 'anytls://': anytls}
OK_TYPES = {'vless', 'vmess', 'shadowsocks', 'trojan', 'tuic', 'hysteria2', 'anytls'}


def name_of(line):
    if '#' in line:
        return urllib.parse.unquote(line.split('#', 1)[1]).strip()
    return line.split('://')[0] + '-unnamed'


def _get(url, proxy=None, timeout=20):
    handlers = [urllib.request.HTTPSHandler(context=CTX)]
    if proxy:
        handlers.append(urllib.request.ProxyHandler({'http': proxy, 'https': proxy}))
    else:
        handlers.append(urllib.request.ProxyHandler({}))
    op = urllib.request.build_opener(*handlers)
    req = urllib.request.Request(url, headers={'User-Agent': 'v2rayN/6.42'})
    return op.open(req, timeout=timeout).read().decode('utf-8', 'replace')


def fetch(tag, urls):
    """先直连，再走本机 39281 代理，最后退回上次成功的本地缓存。

    GFW 收紧时直连订阅域名经常正好是断的，而这时 39281 的 urltest 会落到机场节点上，
    反而能出去——所以代理不是备用，是关键时刻的主路。
    """
    os.makedirs(CACHE, exist_ok=True)
    cache = os.path.join(CACHE, tag + '.sub')
    last = None
    for via, proxy in (('direct', None), ('proxy', PROXY)):
        for u in urls:
            try:
                body = _get(u, proxy)
                with open(cache, 'w') as f:
                    f.write(body)
                print('%s: via %s %s' % (tag, via, u.split('/')[2]))
                return body
            except Exception as e:
                last = e
    if os.path.exists(cache):
        print('%s: 所有源都失败，使用本地缓存 %s' % (tag, cache))
        return open(cache).read()
    raise last or RuntimeError('all subscription sources failed')


def collect():
    texts = []
    for tag, urls in SUBS:
        try:
            raw = fetch(tag, urls).strip()
        except Exception as e:
            print('%s FETCH FAIL %s' % (tag, e))
            continue
        t = raw
        try:
            dec = base64.b64decode(raw + '=' * (-len(raw) % 4)).decode('utf-8', 'replace')
            if '://' in dec:
                t = dec
        except Exception:
            pass
        texts.append((tag, t))
        print('%s: %d lines' % (tag, len(t.splitlines())))

    def rank(name):
        for i, k in enumerate(PREF):
            if k in name:
                return i
        return 99

    cands, seen = [], set()
    for tag, t in texts:
        for line in t.splitlines():
            line = line.strip()
            if '://' not in line:
                continue
            p = PARSERS.get(line.split('://', 1)[0].lower() + '://')
            if not p:
                continue
            name = name_of(line)
            try:
                out = p(line)
            except Exception:
                continue
            if out.get('type') not in OK_TYPES:
                continue
            if ':' in (out.get('server') or ''):
                continue  # 订阅里的 IPv6 节点：容器虽已开 IPv6，但可用率太低，先不纳入
            key = (out.get('type'), out.get('server'), out.get('server_port'))
            if key in seen:
                continue
            seen.add(key)
            cands.append((rank(name), tag, name, out))
    cands.sort(key=lambda x: x[0])
    return cands[:MAX_CANDIDATES]


def tcp_ok(out, timeout=4):
    """拨号前的粗筛：连不上端口就不必再起 sing-box，省掉每个死节点 2 秒。"""
    try:
        s = socket.create_connection((out['server'], int(out['server_port'])), timeout=timeout)
        s.close()
        return True
    except Exception:
        return False


def _with_node(idx, out, fn):
    port = 19000 + idx
    tmpdir = tempfile.mkdtemp(prefix='fbprobe-')
    path = os.path.join(tmpdir, 'c.json')
    with open(path, 'w') as f:
        json.dump({'log': {'level': 'error'},
                   'inbounds': [{'type': 'mixed', 'listen': '127.0.0.1', 'listen_port': port}],
                   'outbounds': [out]}, f)
    p = subprocess.Popen([SB, 'run', '-c', path],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(2)
    try:
        return fn(port)
    finally:
        p.terminate()
        try:
            p.wait(timeout=3)
        except Exception:
            p.kill()
        shutil.rmtree(tmpdir, ignore_errors=True)


def probe(idx, item):
    _, tag, name, out = item
    if not tcp_ok(out):
        return name, {'gfw': 'DEAD', 'tg': 'DEAD'}

    def run(port):
        res = {}
        for t, url in TARGETS:
            try:
                r = subprocess.run(
                    ['curl', '-s', '-o', '/dev/null', '-m', '8', '-w', '%{http_code} %{time_total}',
                     '-x', 'http://127.0.0.1:%d' % port, url],
                    capture_output=True, text=True, timeout=12)
                res[t] = r.stdout.strip() or 'FAIL'
            except Exception:
                res[t] = 'FAIL'
        return res

    return name, _with_node(idx, out, run)


def speed_mbps(idx, item):
    """下载 1MB 估算吞吐。失败返回 0（=未知，不据此淘汰）。"""
    out = item[3]
    if not tcp_ok(out):
        return 0.0

    def run(port):
        try:
            r = subprocess.run(
                ['curl', '-s', '-o', '/dev/null', '-m', '25', '-w', '%{speed_download} %{http_code}',
                 '-x', 'http://127.0.0.1:%d' % port, SPEED_URL],
                capture_output=True, text=True, timeout=30)
            parts = r.stdout.strip().split()
            if len(parts) == 2 and parts[1] == '200':
                return float(parts[0]) * 8 / 1e6
        except Exception:
            pass
        return 0.0

    return _with_node(idx, out, run)


def _tg_seconds(r):
    tg = str(r.get('tg', ''))
    if not tg.startswith(('302', '200', '204')):
        return None
    try:
        return float(tg.split()[1])
    except Exception:
        return 99.0


def _round(items, offset):
    res = list(ThreadPoolExecutor(max_workers=WORKERS).map(
        probe, range(offset, offset + len(items)), items))
    return res


def pick(cands):
    res = _round(cands, 0)
    alive = []
    for (name, r), item in zip(res, cands):
        sec = _tg_seconds(r)
        if sec is None:
            continue
        alive.append((sec, item))
    alive.sort(key=lambda x: x[0])
    print('round1 alive %d / %d' % (len(alive), len(cands)))

    # 第二轮复测：只留两轮都通的，延迟取两轮最大值（更接近持续性，不被单次抖动骗）
    items = [it for _, it in alive]
    res2 = _round(items, 300)
    stable = []
    for (name, r), item in zip(res2, items):
        sec2 = _tg_seconds(r)
        if sec2 is None:
            continue
        sec1 = next(s for s, it in alive if it is item)
        stable.append((max(sec1, sec2), item))
    stable.sort(key=lambda x: x[0])
    print('round2 stable %d' % len(stable))
    if len(stable) < 2:
        return stable, []

    # 吞吐只测前 MAX_SPEED_TESTS 个，低于 MIN_SPEED_MBPS 的直接淘汰；未知(0)保留
    head = stable[:MAX_SPEED_TESTS]
    speeds = list(ThreadPoolExecutor(max_workers=5).map(
        speed_mbps, range(600, 600 + len(head)), [it for _, it in head]))
    ok = []
    for (sec, item), mbps in zip(head, speeds):
        if mbps and mbps < MIN_SPEED_MBPS:
            print('  drop slow %.2f Mbps %s' % (mbps, item[2][:24]))
            continue
        ok.append((sec, mbps, item))
    picked, per = [], {}
    for sec, mbps, item in ok:
        t = item[3].get('type')
        if per.get(t, 0) >= MAX_PER_TYPE:
            continue
        per[t] = per.get(t, 0) + 1
        picked.append((sec, mbps, item))
        if len(picked) >= MAX_PICKED:
            break
    print('picked %d' % len(picked))
    for sec, mbps, (_, tag, name, out) in picked:
        print('  %.2fs %6.1fMbps %-6s %-24s %s %s:%s' %
              (sec, mbps, tag, name[:24], out['type'], out['server'], out['server_port']))
    return stable, picked


def build(picked):
    c = json.load(open(CFG))
    base = [o for o in c['outbounds']
            if not (o.get('tag') == 'auto' or str(o.get('tag', '')).startswith('fb'))]
    members = ['proxy']
    for k, (_, _, (_, _, _, out)) in enumerate(picked):
        o = dict(out)
        o['tag'] = 'fb%d' % k
        base.append(o)
        members.append('fb%d' % k)
    base.append({'type': 'urltest', 'tag': 'auto', 'outbounds': members,
                 'url': 'https://api.telegram.org/', 'interval': '2m', 'tolerance': 100})
    c['outbounds'] = base
    c['route']['final'] = 'auto'
    c['route']['auto_detect_interface'] = False
    for s in c.get('dns', {}).get('servers', []):
        if s.get('detour') == 'proxy':
            s['detour'] = 'auto'
    return c, members


def validate(cfg_path):
    tmp = tempfile.mkdtemp(prefix='fbcheck-')
    try:
        r = subprocess.run(
            ['docker', 'run', '--rm',
             '-v', '%s:/tmp/c.json:ro' % cfg_path,
             '-v', '%s:/etc/sing-box/geosite-cn.srs:ro' % os.path.join(BASE, 'geosite-cn.srs'),
             '-v', '%s:/etc/sing-box/geoip-cn.srs:ro' % os.path.join(BASE, 'geoip-cn.srs'),
             '-v', '%s:/var/lib/sing-box' % tmp,
             '--entrypoint', '/usr/local/bin/sing-box', IMAGE, 'check', '-c', '/tmp/c.json'],
            capture_output=True, text=True, timeout=120)
        if r.returncode != 0:
            print('check failed:\n%s\n%s' % (r.stdout, r.stderr))
            return False
        return True
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def notify(text):
    """刷新结果推一条 Telegram；凭据读不到就静默跳过（不影响主流程）。"""
    try:
        cfg = json.load(open(OPENCLAW_CONFIG, encoding='utf-8'))
        channel = ((cfg.get('channels') or {}).get('telegram') or {})
        token = channel.get('botToken') or ''
        chat = str((channel.get('allowFrom') or [''])[0])
    except Exception:
        return
    if not token or not chat:
        return
    data = urllib.parse.urlencode({'chat_id': chat, 'text': text,
                                   'disable_web_page_preview': 'true'}).encode()
    op = urllib.request.build_opener(
        urllib.request.ProxyHandler({'http': PROXY, 'https': PROXY}),
        urllib.request.HTTPSHandler(context=CTX))
    # 刚重启完 39281 还没起来，重试几次再放弃
    last = None
    for attempt in range(5):
        try:
            op.open('https://api.telegram.org/bot%s/sendMessage' % token,
                    data=data, timeout=20).read()
            print('notify sent')
            return
        except Exception as e:
            last = e
            time.sleep(3)
    print('notify failed: %s' % last)


def main():
    cands = collect()
    print('candidates %d' % len(cands))
    stable, picked = pick(cands)
    if len(picked) < 2:
        print('存活不足 2 个，保持现有 fb* 不变')
        notify('机场兜底刷新失败：可用节点 %d 个，保持旧配置' % len(picked))
        return 1
    c, members = build(picked)
    print('members %s' % members)
    if DRY:
        print('dry-run，不写盘')
        return 0
    tmp = os.path.join(BASE, 'config.fallback.next.json')
    json.dump(c, open(tmp, 'w'), ensure_ascii=False, indent=2)
    if not validate(tmp):
        print('新配置校验失败，保留旧配置')
        return 1
    shutil.copy(CFG, CFG + '.bak-refresh-' + time.strftime('%Y%m%d-%H%M%S'))
    shutil.copy(tmp, CFG)
    shutil.copy(tmp, os.path.join(BASE, 'config.fallback.json'))
    os.remove(tmp)
    subprocess.run(['docker', 'restart', 'sing-box'], check=True)
    print('已应用并重启 sing-box')
    lines = ['机场兜底已刷新（两轮复测 + 测速）']
    for sec, mbps, (_, tag, name, out) in picked:
        lines.append('%s %s %.1fs %.1fMbps' % (tag, name[:18], sec, mbps))
    lines.append('候选 %d，稳定 %d，采用 %d' % (len(cands), len(stable), len(picked)))
    notify('\n'.join(lines))
    return 0


if __name__ == '__main__':
    sys.exit(main())
