/* 皇位厅全部逻辑：单页三区（在位/供给/候选）+ 日志设备双抽屉 + 五个用户动作。
   零依赖。API 契约见 web/httpapi.go；mock 形状见 docs/ui-demo/index.html。 */
(() => {
  const $ = (id) => document.getElementById(id);
  let toastTimer;
  const toast = (msg) => {
    const t = $("toast");
    t.textContent = msg;
    t.classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.remove("show"), 3200);
  };
  async function api(path, opts = {}) {
    const res = await fetch(path, { headers: { "Content-Type": "application/json" }, ...opts });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    return res.json();
  }
  async function busy(button, label, task) {
    if (button.disabled) return;
    const text = $(label);
    const prev = text.textContent;
    button.disabled = true;
    button.classList.add("busy");
    text.textContent = "进行中…";
    try {
      return await task();
    } finally {
      button.disabled = false;
      button.classList.remove("busy");
      text.textContent = prev;
    }
  }
  const fmtTime = (iso) => {
    if (!iso || String(iso).startsWith("0001-")) return "--";
    const d = new Date(iso);
    return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  };
  const fmtAgo = (iso) => {
    if (!iso || String(iso).startsWith("0001-")) return "–";
    const mins = Math.max(0, Math.round((Date.now() - new Date(iso)) / 60000));
    if (mins < 1) return "刚刚";
    if (mins < 60) return `${mins}分钟前`;
    const hours = Math.floor(mins / 60);
    if (hours < 24) return `${hours}小时前`;
    return `${Math.floor(hours / 24)}天前`;
  };

  /* 来源徽标：现行四层 + 陈旧缓存的宽容映射（旧快照里的已废层名不断档显示）。 */
  const SRC_LABEL = {
    user: "手动", subscription: "订阅", bestcf: "BestCF",
    preferred: "优选解析", cfdata: "CFdata", official: "官方段", proxy: "社区",
  };
  const LIVE_TIERS = [["user", "手动"], ["subscription", "订阅"], ["bestcf", "BestCF"]];

  let poolSnapshot = null;   // proxy-candidates 快照（含 active）
  let upstreams = {};        // ip -> {connections, states, inPool}
  let domainLat = {};        // 优选域名 -> {ms, ok, at}
  let forward = null;        // /api/status.forward 在途转发实况
  let stickyOn = true;       // /api/config.cfnat.sticky 设备粘性
  let sourceHealth = {};     // /api/status.sources 健康账
  let qualityByIp = {};      // /api/status.qualityScheduler.records：最近一次探测成绩
  let vlessOn = false;       // /api/config.vlessProbe
  let coloPrefer = "";       // /api/config.poolColoPrefer
  let logLines = [];

  // Manual actions can overlap the regular timers. Ignore responses from an
  // older request so a slow poll cannot roll the screen back.
  const pollVersion = Object.create(null);
  const beginPoll = (name) => {
    const version = (pollVersion[name] || 0) + 1;
    pollVersion[name] = version;
    return version;
  };
  const invalidatePoll = (name) => {
    pollVersion[name] = (pollVersion[name] || 0) + 1;
    return pollVersion[name];
  };
  const isCurrentPoll = (name, version) => pollVersion[name] === version;

  /* ---------- 数据拉取 ---------- */
  async function pullConfig() {
    try {
      const cfg = await api("/api/config");
      vlessOn = !!cfg.vlessProbe;
      coloPrefer = String(cfg.poolColoPrefer || "");
      stickyOn = !(cfg.cfnat && cfg.cfnat.sticky === false);
    } catch (_) { /* 心跳容错 */ }
  }
  async function pullStatus() {
    const version = beginPoll("status");
    try {
      const st = await api("/api/status");
      if (!isCurrentPoll("status", version)) return;
      const running = !!st.cfnat?.running;
      $("cfnatDot").className = `st ${running ? "ok" : "idle"}`;
      $("cfnatDot").textContent = running ? "●" : "◌";
      $("cfnatState").textContent = running ? "转发运行中" : "转发已停止";
      if (st.sources && typeof st.sources === "object") {
        sourceHealth = st.sources;
        renderHealth();
      }
      forward = st.forward || null;
      renderForward();
      const recs = st.qualityScheduler?.records;
      qualityByIp = {};
      if (Array.isArray(recs)) {
        for (const rec of recs) if (rec && rec.ip) qualityByIp[rec.ip] = rec;
      }
    } catch (_) { /* 心跳容错 */ }
  }
  async function pullPool() {
    const version = beginPoll("pool");
    try {
      const snap = await api("/api/cfnat/proxy-candidates");
      if (!isCurrentPoll("pool", version)) return;
      poolSnapshot = snap;
      domainLat = snap.domainLatency || {};
      renderThrone();
      renderBench();
      renderHealth();
    } catch (_) { /* 心跳容错 */ }
  }
  async function pullUpstreams() {
    const version = beginPoll("upstreams");
    try {
      const snap = await api("/api/cfnat/upstreams");
      if (!isCurrentPoll("upstreams", version)) return;
      const map = {};
      for (const u of snap.upstreams || []) map[u.ip] = u;
      upstreams = map;
      renderThrone();
    } catch (_) { /* 心跳容错 */ }
  }
  async function pullLan() {
    const version = beginPoll("lan");
    try {
      const snap = await api("/api/cfnat/connections");
      if (!isCurrentPoll("lan", version)) return;
      renderLan(snap);
    } catch (_) { /* 心跳容错 */ }
  }

  /* ---------- 通用行渲染（在位 + 候选共用） ---------- */
  function buildRow({ addr, colo, label, connLive, connN, connUnit, scoreParts, actions, check }) {
    const row = document.createElement("div");
    row.className = check && check.off ? "ip-row off" : "ip-row";
    const main = document.createElement("div");
    main.className = "ip-main";
    if (check) {
      const lab = document.createElement("label");
      lab.className = "mempick";
      lab.title = "勾上＝参与转发；取消＝仍在位但不转发";
      const box = document.createElement("input");
      box.type = "checkbox";
      box.className = "memchk";
      box.dataset.addr = addr;
      box.dataset.group = check.group;
      box.checked = !check.off;
      box.addEventListener("change", () => queueSelection());
      lab.append(box);
      main.append(lab);
    }
    main.append(Object.assign(document.createElement("span"), { className: "ip-addr mono", textContent: addr }));
    if (colo) main.append(Object.assign(document.createElement("span"), { className: "chip mono", textContent: colo }));
    main.append(Object.assign(document.createElement("span"), { className: "src", textContent: label }));
    const conn = document.createElement("div");
    conn.className = connLive ? "ip-conn live" : "ip-conn quiet";
    const n = document.createElement("span");
    n.className = "n";
    n.textContent = connN;
    if (connLive) n.dataset.conn = addr;
    conn.append(n, Object.assign(document.createElement("span"), { className: "u", textContent: connUnit }));
    const score = document.createElement("div");
    score.className = "ip-score";
    for (const [big, unit] of scoreParts) {
      const item = document.createElement("span");
      item.className = "score-item";
      item.append(Object.assign(document.createElement("b"), { textContent: big }));
      if (unit) item.append(document.createTextNode(" " + unit));
      score.append(item);
    }
    row.append(main, conn, score);
    const act = document.createElement("div");
    act.className = "rowbtn";
    for (const action of actions) {
      const btn = document.createElement("button");
      btn.className = "btn mini";
      btn.type = "button";
      btn.textContent = action.text;
      if (action.title) btn.title = action.title;
      btn.addEventListener("click", () => action.run(btn));
      act.append(btn);
    }
    row.append(act);
    return row;
  }

  /* ---------- 渲染：在途转发目标（cfnat 日志实况） ---------- */
  function renderForward() {
    const el = $("forwardLine");
    if (!el) return;
    if (!forward || !forward.host) {
      el.textContent = "在途转发目标 –（等待首条转发日志）";
      return;
    }
    let txt = `在途转发目标 ${forward.host}`;
    if (forward.dialMs) txt += ` · 拨号 ${forward.dialMs}ms`;
    const dist = (forward.counts || []).map((c) => `${c.host}×${c.count}`).join(" · ");
    if (dist) txt += ` ｜ 近${forward.total} 次：${dist}`;
    el.textContent = txt;
  }

  /* ---------- 渲染：在位（壹） ---------- */
  function renderThrone() {
    const active = poolSnapshot?.active || {};
    const ips = active.ips || [];
    const domains = active.domains || [];
    const results = new Map((active.results || []).map((r) => [r.ip, r]));
    const byIp = poolSnapshot?.sourceByIp || {};
    const upstreamTotal = Object.values(upstreams)
      .filter((u) => u.inPool)
      .reduce((s, u) => s + (Number(u.connections) || 0), 0);
    $("tallyPool").textContent = ips.length;
    $("tallyDom").textContent = domains.length;
    $("tallyConn").textContent = upstreamTotal;
    $("coloLine").textContent = coloLineText(ips, results);
    const foot = $("throneFoot");
    if (foot) {
      foot.textContent = stickyOn
        ? "「测」= 重新体检（WS 延迟 + 可选 VLESS 测速）；踢后后台自动补位；设备粘性开：同设备恒定走同一上游。"
        : "「测」= 重新体检（WS 延迟 + 可选 VLESS 测速）；踢后后台自动补位；设备粘性关：按连接轮转铺开全部成员。";
    }
    const disabled = new Set(active.disabled || []);
    const prioSet = new Set(active.priority || []);
    const total = ips.length + domains.length;
    const enabledCount = [...ips, ...domains].filter((m) => !disabled.has(m)).length;
    $("throneLock").checked = !!active.lock;
    $("throneAll").checked = total > 0 && enabledCount === total;
    $("throneCtlHint").textContent = !total
      ? "皇位空悬。"
      : active.lock
        ? `锁位中：只转发 ${enabledCount} 个已勾成员，其余 ${total - enabledCount} 个不参与。`
        : `已勾 ${enabledCount} / ${total} 参与转发；扶持登基的自动置顶。`;

    const box = $("throneRows");
    if (!box) return;
    box.replaceChildren();
    if (!total) {
      const empty = document.createElement("p");
      empty.className = "foot";
      empty.textContent = "皇位空悬——点「立即优选」开考。候选会按供给层汇流；是否自动换池由 PROXY_AUTO_APPLY 控制。";
      box.append(empty);
      return;
    }
    const order = (list) => [...list.filter((m) => prioSet.has(m)), ...list.filter((m) => !prioSet.has(m))];
    const frag = document.createDocumentFragment();
    if (ips.length) frag.append(groupHead("优选 IP", "ip", ips.length));
    for (const ip of order(ips)) {
      const result = results.get(ip);
      const up = upstreams[ip];
      const connN = up?.inPool ? (Number(up.connections) || 0) : 0;
      const scoreParts = [];
      if (result?.downloadMbps) scoreParts.push([result.downloadMbps.toFixed(1), "Mbps"]);
      if (result?.dataLatency) scoreParts.push([`${result.dataLatency}`, "ms"]);
      if (!scoreParts.length) scoreParts.push(["在位", "体检随时点"]);
      const pinned = prioSet.has(ip);
      frag.append(buildRow({
        addr: ip,
        colo: result?.colo || "",
        label: (pinned ? "置顶 · " : "") + (SRC_LABEL[byIp[ip]] || byIp[ip] || "已登基"),
        connLive: connN > 0,
        connN: String(connN),
        connUnit: "连接",
        check: { group: "ip", off: disabled.has(ip) },
        scoreParts,
        actions: [
          { text: "测", title: "测 WS 延迟" + (vlessOn ? " + VLESS 速度" : ""), run: (btn) => probeIP(ip, btn) },
          { text: pinned ? "降" : "顶", title: pinned ? "取消置顶" : "置顶：排到 -fixed 最前，优先转发", run: () => pinMember(ip, !pinned) },
          { text: "踢", title: "把这个 IP 踢出在位并立即生效", run: (btn) => kickIP(ip, btn) },
        ],
      }));
    }
    // 优选域名行：升格为 -fixed 直连成员，拨号时按当前 DNS 活解析。
    if (domains.length) frag.append(groupHead("优选域名", "domain", domains.length));
    for (const domain of order(domains)) {
      const lat = domainLat[domain];
      const scoreParts = [];
      if (lat && lat.ok) scoreParts.push([String(lat.ms), "ms"]);
      else if (lat && !lat.ok) scoreParts.push(["不可达", ""]);
      else scoreParts.push(["—", "探测中"]);
      const pinned = prioSet.has(domain);
      frag.append(buildRow({
        addr: domain,
        colo: "DNS",
        label: (pinned ? "置顶 · " : "") + "域名直连",
        connLive: false,
        connN: "—",
        connUnit: "活解析",
        check: { group: "domain", off: disabled.has(domain) },
        scoreParts,
        actions: [
          { text: pinned ? "降" : "顶", title: pinned ? "取消置顶" : "置顶：排到 -fixed 最前，优先转发", run: () => pinMember(domain, !pinned) },
          { text: "踢", title: "把该域名踢出转发池", run: (btn) => kickIP(domain, btn) },
        ],
      }));
    }
    box.append(frag);
  }
  /* ---------- 分组表头：优选域名 / 优选 IP 各自的批量开关 ---------- */
  function groupHead(title, group, count) {
    const wrap = document.createElement("div");
    wrap.className = "group-head";
    const t = document.createElement("span");
    t.className = "group-title";
    t.textContent = `${title} · ${count}`;
    const acts = document.createElement("div");
    acts.className = "rowbtn";
    for (const [mode, text, tip] of [
      ["active", "只开活跃", "只勾最近一次探测可达的成员"],
      ["all", "全开", "全部勾上，全部参与转发"],
      ["none", "全关", "全部取消勾选：仍在位，但一律不转发"],
    ]) {
      const b = document.createElement("button");
      b.className = "btn mini";
      b.type = "button";
      b.textContent = text;
      b.title = tip;
      b.addEventListener("click", () => bulkGroup(group, mode));
      acts.append(b);
    }
    wrap.append(t, acts);
    return wrap;
  }

  /* ---------- 选择：谁承载流量 ---------- */
  let selTimer;
  function queueSelection() {
    clearTimeout(selTimer);
    selTimer = setTimeout(() => commitSelection(), 350);
  }
  // 「活跃」= 最近一次探测可达：域名看 TCP:443 探测，IP 看质量调度器记录。
  function isActiveMember(addr) {
    const lat = domainLat[addr];
    if (lat) return !!lat.ok;
    const q = qualityByIp[addr];
    if (q) return Number(q.averageLatencyMs) > 0 || Number(q.successRate) > 0;
    return !!(poolSnapshot?.active?.results || []).find((r) => r.ip === addr);
  }
  function currentSelection() {
    const enabled = [];
    for (const b of document.querySelectorAll(".memchk")) if (b.checked) enabled.push(b.dataset.addr);
    const priority = (poolSnapshot?.active?.priority || []).filter((m) => enabled.includes(m));
    return { enabled, priority, lock: $("throneLock").checked };
  }
  async function commitSelection(note) {
    const body = currentSelection();
    if (!body.enabled.length) {
      toast("至少要保留一个转发成员");
      await pullPool();
      return;
    }
    invalidatePoll("pool");
    try {
      await api("/api/cfnat/active/select", { method: "POST", body: JSON.stringify(body) });
      if (note) toast(note);
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`选择未生效：${e.message}`);
      await pullPool();
    }
  }
  async function bulkGroup(group, mode) {
    const boxes = [...document.querySelectorAll(`.memchk[data-group="${group}"]`)];
    if (!boxes.length) {
      toast("该组暂无成员");
      return;
    }
    for (const b of boxes) {
      if (mode === "all") b.checked = true;
      else if (mode === "none") b.checked = false;
      else b.checked = isActiveMember(b.dataset.addr);
    }
    await commitSelection(mode === "active" ? "已只勾活跃成员" : mode === "all" ? "已全部开启" : "已全部关闭");
  }
  async function pinMember(addr, on) {
    const priority = (poolSnapshot?.active?.priority || []).filter((m) => m !== addr);
    if (on) priority.unshift(addr);
    invalidatePoll("pool");
    try {
      await api("/api/cfnat/active/select", {
        method: "POST",
        body: JSON.stringify({ ...currentSelection(), priority }),
      });
      toast(on ? `${addr} 已置顶` : `${addr} 已取消置顶`);
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`置顶失败：${e.message}`);
    }
  }

  function coloLineText(ips, results) {
    if (!ips.length) return "机房 –";
    const dist = {};
    for (const ip of ips) {
      const c = results.get(ip)?.colo || "未知";
      dist[c] = (dist[c] || 0) + 1;
    }
    let line = "机房 " + Object.entries(dist).map(([k, v]) => `${k}×${v}`).join(" · ");
    if (coloPrefer) line += ` ｜ 偏好 ${coloPrefer}`;
    line += ` ｜ 下次殿试 ${fmtTime(poolSnapshot?.nextRefresh)}`;
    return line;
  }

  /* ---------- 渲染：候选榜（叁） ---------- */
  function renderBench() {
    const box = $("benchRows");
    if (!box) return;
    const active = poolSnapshot?.active || {};
    const inPool = new Set([...(active.ips || []), ...(active.domains || [])]);
    const byIp = poolSnapshot?.sourceByIp || {};
    const candidates = (poolSnapshot?.ips || []).filter((ip) => !inPool.has(ip)).slice(0, 24);
    const results = new Map((active.results || []).map((r) => [r.ip, r]));
    box.replaceChildren();
    if (!candidates.length) {
      const empty = document.createElement("p");
      empty.className = "foot";
      empty.textContent = "候选已全部登基或为空。";
      box.append(empty);
      return;
    }
    const frag = document.createDocumentFragment();
    for (const ip of candidates) {
      const result = results.get(ip);
      const scoreParts = [];
      if (result?.latency) scoreParts.push([`${result.latency}`, "ms"]);
      if (!scoreParts.length) scoreParts.push(["候选", "初筛已过"]);
      frag.append(buildRow({
        addr: ip,
        colo: result?.colo || "",
        label: SRC_LABEL[byIp[ip]] || byIp[ip] || "候选",
        connLive: false,
        connN: "—",
        connUnit: "候补",
        scoreParts,
        actions: [
          { text: "顶", title: "体检通过后顶替在位最差成员", run: (btn) => promoteIP(ip, btn) },
          { text: "测", title: "测 WS 延迟" + (vlessOn ? " + VLESS 测速" : ""), run: (btn) => probeIP(ip, btn) },
        ],
      }));
    }
    box.append(frag);
  }

  /* ---------- 渲染：供给心跳（贰）——只留活源 ---------- */
  function renderHealth() {
    const box = $("beacons");
    if (!box) return;
    const byIp = poolSnapshot?.sourceByIp || {};
    box.replaceChildren();
    const frag = document.createDocumentFragment();
    for (const [key, label] of LIVE_TIERS) {
      let n = 0;
      for (const ip of Object.keys(byIp)) if (byIp[ip] === key) n++;
      const b = document.createElement("span");
      b.className = "beacon";
      b.append(Object.assign(document.createElement("span"), { className: `st ${n ? "ok" : "idle"}`, textContent: n ? "●" : "◌" }));
      b.append(document.createTextNode(label + " "));
      b.append(Object.assign(document.createElement("b"), { className: "mono", textContent: String(n) }));
      frag.append(b);
    }
    const doms = (poolSnapshot?.active?.domains || []).length + (poolSnapshot?.forwardDomains || []).length;
    const d = document.createElement("span");
    d.className = "beacon";
    d.append(Object.assign(document.createElement("span"), { className: `st ${doms ? "ok" : "idle"}`, textContent: doms ? "●" : "◌" }));
    d.append(document.createTextNode("域名表 "));
    d.append(Object.assign(document.createElement("b"), { className: "mono", textContent: String(doms) }));
    frag.append(d);
    box.append(frag);
    // 渠道健康小表：按 IP 数降序取前 10（全量在 /api/status.sources）。
    const hb = $("healthBody");
    if (!hb) return;
    hb.replaceChildren();
    const entries = Object.entries(sourceHealth || {})
      .map(([k, v]) => ({ key: shortKey(k), ips: Number(v?.ips) || 0, f: Number(v?.fetches) || 0, s: Number(v?.successes) || 0, ago: v?.lastSuccess ? fmtAgo(v.lastSuccess) : (v?.lastFailed ? "败 " + fmtAgo(v.lastFailed) : "–") }))
      .sort((a, b) => b.ips - a.ips)
      .slice(0, 10);
    if (!entries.length) {
      const tr = document.createElement("tr");
      const td = document.createElement("td");
      td.colSpan = 4;
      td.textContent = "尚未刷新";
      tr.append(td);
      hb.append(tr);
      return;
    }
    for (const e of entries) {
      const tr = document.createElement("tr");
      tr.append(Object.assign(document.createElement("td"), { textContent: e.key }));
      tr.append(Object.assign(document.createElement("td"), { className: "mono", textContent: `${e.f}/${e.s}` }));
      const ipTd = document.createElement("td");
      ipTd.className = "mono";
      if (e.ips > 0) ipTd.classList.add("hl");
      ipTd.textContent = String(e.ips);
      tr.append(ipTd);
      tr.append(Object.assign(document.createElement("td"), { textContent: e.ago }));
      hb.append(tr);
    }
  }
  function shortKey(k) {
    return k
      .replace("https://bestcf.pages.dev/", "bestcf·")
      .replace("https://raw.githubusercontent.com/", "gh·")
      .replace("https://cf.junzhen.qzz.io/", "junzhen·");
  }

  /* ---------- 渲染：设备名册（伍） ---------- */
  function renderLan(snap) {
    const body = $("lanBody");
    if (!body) return;
    const sources = snap?.sources || [];
    body.replaceChildren();
    $("lanHead").textContent = `伍 · 设备名册 · ${sources.length} 台`;
    if (!sources.length) {
      const tr = document.createElement("tr");
      const td = document.createElement("td");
      td.colSpan = 3;
      if (snap?.error) {
        td.textContent = `读取失败：${snap.error}`;
      } else {
        td.textContent = `当前无连接（扫 ${snap?.scanned ?? 0} 行 / 端口命中 ${snap?.portHits ?? 0} 行）`;
      }
      tr.append(td);
      body.append(tr);
      return;
    }
    const frag = document.createDocumentFragment();
    for (const s of sources) {
      const tr = document.createElement("tr");
      const states = Object.entries(s.states || {}).map(([k, v]) => `${k} × ${v}`).join(" · ");
      const cells = [[s.ip || "--", "mono"], [String(s.connections ?? 0), "mono hl"], [states || "--", ""]];
      for (const [value, cls] of cells) {
        const td = document.createElement("td");
        td.textContent = value;
        if (cls) td.className = cls;
        tr.append(td);
      }
      frag.append(tr);
    }
    body.append(frag);
  }

  /* ---------- 动作一：立即优选 ---------- */
  async function optimize() {
    const version = invalidatePoll("pool");
    invalidatePoll("status");
    invalidatePoll("upstreams");
    try {
      const snap = await busy($("optimizeBtn"), "optimizeLabel", () =>
        api("/api/cfnat/proxy-candidates", { method: "POST", body: "{}" }));
      if (isCurrentPoll("pool", version)) {
        poolSnapshot = snap;
        renderThrone();
        renderBench();
      }
      await Promise.allSettled([pullPool(), pullStatus(), pullUpstreams()]);
      const n = snap.active?.ips?.length || 0;
      const err = snap.active?.error;
      toast(err ? `优选完成但保留旧池：${err}` : `优选完成，${n} 个 IP 在位`);
    } catch (e) {
      toast(`优选失败：${e.message}`);
    }
  }

  /* ---------- 动作二甲：扶持登基（免体检直入位） ---------- */
  async function endorse() {
    const text = $("endorseInput").value.trim();
    if (!text) {
      $("endorseInput").focus();
      toast("先粘贴公网 IP 或优选域名");
      return;
    }
    invalidatePoll("pool");
    invalidatePoll("status");
    invalidatePoll("upstreams");
    try {
      const data = await busy($("endorseBtn"), "endorseBtn", () =>
        api("/api/cfnat/active/endorse", { method: "POST", body: JSON.stringify({ members: text }) }));
      const parts = [];
      if (data.ips?.length) parts.push(`${data.ips.length} 个 IP`);
      if (data.domains?.length) parts.push(`${data.domains.length} 个域名`);
      let msg = parts.length ? `扶持登基：${parts.join(" + ")}` : "扶持登基：全部已在位";
      msg += `（现 ${data.ips_total} IP + ${data.domains_total} 域名在位）`;
      if (data.skipped) msg += ` · 跳过已在位 ${data.skipped}`;
      toast(msg);
      if (!data.unchanged) $("endorseInput").value = "";
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`扶持登基失败：${e.message}`);
    }
  }

  /* ---------- 动作二：智能供给（单个 IPv4 登基顶替，多行批量入池） ---------- */
  async function supply() {
    const text = $("supplyInput").value.trim();
    if (!text) {
      $("supplyInput").focus();
      toast("先粘贴 IP、域名或订阅内容");
      return;
    }
    if (/^\d{1,3}(\.\d{1,3}){3}$/.test(text)) {
      const ok = await doPromote(text, null);
      if (ok) { $("supplyInput").value = ""; updateSupplyHint(); }
      return;
    }
    invalidatePoll("pool");
    invalidatePoll("status");
    invalidatePoll("upstreams");
    try {
      const data = await busy($("supplyBtn"), "supplyBtn", () =>
        api("/api/cfnat/proxy-scan/apply", {
          method: "POST",
          body: JSON.stringify({ ips: text, subscription: text, limit: 500 }),
        }));
      const parts = [`${data.applied || 0} 个 IP`];
      if (data.domains) parts.push(`${data.domains} 个域名`);
      toast(`手动供给已应用：${parts.join(" + ")}（后台可继续复核）`);
      $("supplyInput").value = "";
      updateSupplyHint();
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`优选入池失败：${e.message}`);
    }
  }
  function updateSupplyHint() {
    const v = $("supplyInput").value.trim();
    const single = /^\d{1,3}(\.\d{1,3}){3}$/.test(v);
    $("supplyBtn").textContent = v === "" ? "批量入池" : (single ? "登基顶替" : "批量入池");
    $("supplyHint").textContent = single
      ? "就一个 IP：先送去体检，过了才有座位，顶掉在位最差的那位。"
      : "一堆 IP、优选域名或订阅都行：逐个体检，合格的才登基。";
  }

  /* ---------- 动作三：单 IP 体检 ---------- */
  async function probeIP(ip, button) {
    button.disabled = true;
    const prev = button.textContent;
    button.textContent = "…";
    try {
      const data = await api("/api/cfnat/proxy-probe", {
        method: "POST",
        body: JSON.stringify({ ip }),
      });
      if (!data.ok) {
        toast(`${ip} 体检失败：${data.error || "未知"}`);
        return;
      }
      let msg = `${ip} 延迟 ${data.wsLatencyMs} ms`;
      if (data.colo) msg += ` [${data.colo}]`;
      if (Number(data.wsFails) > 0) msg += ` · 3测${data.wsFails}失`;
      if (data.vless) {
        if (data.vless.ok) msg += ` · 测速 ${Number(data.vless.mbps).toFixed(1)} Mbps`;
        else msg += ` · 测速失败：${data.vless.error}`;
      }
      toast(msg);
    } catch (e) {
      toast(`体检失败：${e.message}`);
    } finally {
      button.disabled = false;
      button.textContent = prev;
    }
  }

  /* ---------- 动作四：顶替 / 指定登基 ---------- */
  async function promoteIP(ip, button) {
    if (!window.confirm(`让 ${ip} 顶替在位最差成员？\n体检通过即换座，立即生效。`)) return;
    await doPromote(ip, button);
  }
  async function doPromote(ip, button) {
    if (button) button.disabled = true;
    invalidatePoll("pool");
    invalidatePoll("status");
    invalidatePoll("upstreams");
    try {
      const data = await api("/api/cfnat/proxy-promote", {
        method: "POST",
        body: JSON.stringify({ ip }),
      });
      let msg = data.already
        ? `${ip} 已在位，无需顶替`
        : data.replaced ? `${ip} 已顶替 ${data.replaced} 登基` : `${ip} 体检通过，空池登基`;
      if (data.colo) msg += ` [${data.colo}]`;
      toast(msg);
      await Promise.allSettled([pullPool(), pullUpstreams()]);
      return true;
    } catch (e) {
      toast(`顶替失败：${e.message}`);
      return false;
    } finally {
      if (button) button.disabled = false;
    }
  }

  /* ---------- 动作五：踢 ---------- */
  async function kickIP(ip, button) {
    if (!window.confirm(`踢掉 ${ip}？\n在途连接会瞬断重连，剩余成员立即接管，后台随后自动补位。`)) return;
    button.disabled = true;
    invalidatePoll("pool");
    invalidatePoll("status");
    invalidatePoll("upstreams");
    try {
      const data = await api("/api/cfnat/active/kick", {
        method: "POST",
        body: JSON.stringify({ ip }),
      });
      toast(`已踢掉 ${ip}，剩 ${data.remaining} 个 IP 在位，后台补位中`);
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`踢除失败：${e.message}`);
    } finally {
      button.disabled = false;
    }
  }

  /* ---------- 日志抽屉 ---------- */
  async function pullLogs() {
    const version = beginPoll("logs");
    try {
      const data = await api(`/api/logs?target=${$("logTarget").value}&lines=200`);
      if (!isCurrentPoll("logs", version) || $("drawerLog").hidden) return;
      logLines = data.lines || [];
      renderLogs();
    } catch (_) { /* 容错 */ }
  }
  function renderLogs() {
    const box = $("logBox");
    if (!box) return;
    const q = $("logQuery").value.trim().toLowerCase();
    const lines = q ? logLines.filter((l) => String(l).toLowerCase().includes(q)) : logLines;
    box.textContent = lines.length ? lines.join("\n") : "暂无日志";
    if ($("logFollow").checked) box.scrollTop = box.scrollHeight;
  }

  /* ---------- 装配 ---------- */
  $("optimizeBtn").addEventListener("click", optimize);
  $("supplyBtn").addEventListener("click", supply);
  $("endorseBtn").addEventListener("click", endorse);
  $("supplyInput").addEventListener("input", updateSupplyHint);
  $("benchRefresh").addEventListener("click", pullPool);
  $("logToggle").addEventListener("click", () => { $("drawerLog").hidden = !$("drawerLog").hidden; if (!$("drawerLog").hidden) pullLogs(); });
  $("throneAll").addEventListener("change", () => {
    const on = $("throneAll").checked;
    for (const b of document.querySelectorAll(".memchk")) b.checked = on;
    commitSelection(on ? "已全选参与转发" : "已全部取消勾选");
  });
  $("throneLock").addEventListener("change", () =>
    commitSelection($("throneLock").checked ? "锁位已开：只转扶持登基的" : "锁位已关：已勾成员恢复轮转"));
  $("logTarget").addEventListener("change", pullLogs);
  $("logQuery").addEventListener("input", renderLogs);
  $("logCopy").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(logLines.join("\n"));
      toast("日志已复制");
    } catch (e) {
      toast(`复制失败：${e.message}`);
    }
  });

  /* ---------- 时钟 ---------- */
  function tick() {
    const d = new Date();
    const t = [d.getHours(), d.getMinutes(), d.getSeconds()].map((x) => String(x).padStart(2, "0")).join(":");
    $("clock").textContent = t;
    $("footClock").textContent = d.toLocaleDateString();
  }

  async function boot() {
    updateSupplyHint();
    tick();
    setInterval(tick, 1000);
    await pullConfig();
    await Promise.allSettled([pullStatus(), pullPool(), pullUpstreams(), pullLan()]);
    setInterval(pullStatus, 3000);
    setInterval(pullUpstreams, 4000);
    setInterval(pullPool, 9000);
    setInterval(pullLan, 12000);
    setInterval(() => { if (!$("drawerLog").hidden) pullLogs(); }, 5000);
  }
  boot();
})();
