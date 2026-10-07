/* 优选台全部逻辑：多页路由（主页/供给/日志）+ 五个用户动作。 */
(() => {
  const SRC_LABEL = {
    user: "手动", subscription: "订阅", preferred: "优选解析",
    cfdata: "CFdata", official: "官方段", proxy: "社区", bestcf: "BestCF",
  };
  let poolSnapshot = null;   // proxy-candidates 快照
  let upstreams = {};        // ip -> {connections, states, inPool}
  let lanPeers = -1;
  let logLines = [];
  let currentPage = "home";
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

  /* ---------- 路由 ---------- */
  function route() {
    const hash = location.hash || "#/";
    currentPage = hash.startsWith("#/supply") ? "supply"
      : hash.startsWith("#/logs") ? "logs"
      : "home";
    for (const page of ["home", "supply", "logs"]) {
      const el = $(`page-${page}`);
      if (el) el.hidden = page !== currentPage;
    }
    for (const link of document.querySelectorAll(".pagenav-link")) {
      link.classList.toggle("active", link.dataset.page === currentPage);
    }
    if (currentPage === "logs") pullLogs();
  }
  window.addEventListener("hashchange", route);

  /* ---------- 数据拉取 ---------- */
  async function pullStatus() {
    const version = beginPoll("status");
    try {
      const st = await api("/api/status");
      if (!isCurrentPoll("status", version)) return;
      const running = !!st.cfnat?.running;
      $("cfnatPulse").className = `pulse ${running ? "ok" : "bad"}`;
      $("cfnatState").textContent = running ? "转发运行中" : "转发已停止";
      $("topbarSub").textContent = running ? "CFnat 转发已启动 · 设备粘性" : "转发已停止";
    } catch (_) { /* 心跳容错 */ }
  }

  async function pullPool() {
    const version = beginPoll("pool");
    try {
      const snap = await api("/api/cfnat/proxy-candidates");
      if (!isCurrentPoll("pool", version)) return;
      poolSnapshot = snap;
      renderPool();
      renderBench();
      renderBeacons();
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
      renderPool();
    } catch (_) { /* 心跳容错 */ }
  }

  async function pullLan() {
    const version = beginPoll("lan");
    try {
      const snap = await api("/api/cfnat/connections");
      if (!isCurrentPoll("lan", version)) return;
      lanPeers = (snap.sources || []).length;
      $("lanPeers").textContent = `${lanPeers} 台局域网设备`;
      renderLan(snap.sources || []);
    } catch (_) { /* 心跳容错 */ }
  }

  /* ---------- 局域网设备明细 ----------
     cfnat 是四层哑管道：TLS 加密后无 UA/Host 可读，设备识别以局域网 IP 为准。 */
  function renderLan(sources) {
    const body = $("lanTableBody");
    if (!body) return;
    body.replaceChildren();
    if (!sources.length) {
      const tr = document.createElement("tr");
      const td = document.createElement("td");
      td.colSpan = 3;
      td.className = "inline-status";
      td.textContent = "当前无局域网连接";
      tr.append(td);
      body.append(tr);
      return;
    }
    const frag = document.createDocumentFragment();
    for (const s of sources) {
      const tr = document.createElement("tr");
      const states = Object.entries(s.states || {}).map(([k, v]) => `${k} × ${v}`).join(" · ");
      for (const [value, cls] of [[s.ip || "--", "mono"], [s.connections ?? 0, ""], [states || "--", ""]]) {
        const td = document.createElement("td");
        td.textContent = String(value);
        if (cls) td.className = cls;
        tr.append(td);
      }
      frag.append(tr);
    }
    body.append(frag);
  }

  /* ---------- 通用行渲染（在位 + B 榜共用） ---------- */
  function buildRow({ addr, label, badgeCls, connN, connUnit, scoreParts, actions }) {
    const row = document.createElement("div");
    row.className = "ip-row";

    const main = document.createElement("div");
    main.className = "ip-main";
    const a = document.createElement("span");
    a.className = "ip-addr";
    a.textContent = addr;
    const src = document.createElement("span");
    src.className = "ip-src";
    const badge = document.createElement("span");
    badge.className = `src-badge ${badgeCls}`;
    badge.textContent = label;
    src.append(badge);
    main.append(a, src);

    const conn = document.createElement("div");
    conn.className = `ip-conn${connN ? " live" : ""}`;
    const n = document.createElement("span");
    n.className = "n";
    n.textContent = connN;
    const u = document.createElement("span");
    u.className = "u";
    u.textContent = connUnit;
    conn.append(n, u);

    const score = document.createElement("div");
    score.className = "ip-score";
    for (const [big, unit] of scoreParts) {
      const item = document.createElement("span");
      item.className = "score-item";
      const b = document.createElement("b");
      b.textContent = big;
      item.append(b);
      if (unit) {
        const s = document.createElement("span");
        s.textContent = unit;
        item.append(s);
      }
      score.append(item);
    }

    row.append(main, conn, score);
    for (const action of actions) {
      const btn = document.createElement("button");
      btn.className = action.cls || "kick";
      btn.type = "button";
      btn.textContent = action.text;
      if (action.title) btn.title = action.title;
      btn.addEventListener("click", () => action.run(btn));
      row.append(btn);
    }
    return row;
  }

  /* ---------- 渲染：在位皇族（1 层） ---------- */
  function renderPool() {
    const active = poolSnapshot?.active || {};
    const ips = active.ips || [];
    const results = new Map((active.results || []).map((r) => [r.ip, r]));
    const byIp = poolSnapshot?.sourceByIp || {};
    const upstreamTotal = Object.values(upstreams)
      .filter((u) => u.inPool)
      .reduce((s, u) => s + (Number(u.connections) || 0), 0);

    $("poolHeadline").textContent = ips.length
      ? `${ips.length} IP + ${(active.domains || []).length} 域名在位 · ${upstreamTotal} 条连接在途`
      : "皇位空悬——点「立即优选」开考";
    $("nextRefresh").textContent = `下次候选刷新 ${fmtTime(poolSnapshot?.nextRefresh)}`;

    const box = $("poolRows");
    if (!box) return;
    box.replaceChildren();
    if (!ips.length && !(active.domains || []).length) {
      const empty = document.createElement("p");
      empty.className = "hero-foot";
      empty.textContent = "无在位成员。候选会按供给层汇流；是否自动换池由 PROXY_AUTO_APPLY 控制。";
      box.append(empty);
      return;
    }
    const frag = document.createDocumentFragment();
    for (const ip of ips) {
      const result = results.get(ip);
      const up = upstreams[ip];
      const connN = up?.inPool ? (Number(up.connections) || 0) : 0;
      const scoreParts = [];
      if (result?.downloadMbps) scoreParts.push([result.downloadMbps.toFixed(1), "Mbps"]);
      if (result?.dataLatency) scoreParts.push([`${result.dataLatency}`, "ms"]);
      if (!scoreParts.length) scoreParts.push(["在位", "体检随时点"]);
      frag.append(buildRow({
        addr: ip,
        label: SRC_LABEL[byIp[ip]] || "已登基",
        badgeCls: byIp[ip] || "",
        connN: String(connN),
        connUnit: "连接",
        scoreParts,
        actions: [
          { text: "测", cls: "kick probe", title: "测 WS 延迟" + (vlessEnabled() ? " + VLESS 速度" : ""), run: (btn) => probeIP(ip, btn) },
          { text: "踢", title: "把这个 IP 踢出皇位并立即生效", run: (btn) => kickIP(ip, btn) },
        ],
      }));
    }
    // 优选域名行：升格为 -fixed 直连成员，拨号时按当前 DNS 活解析。
    for (const domain of active.domains || []) {
      frag.append(buildRow({
        addr: domain,
        label: "域名直连",
        badgeCls: "preferred",
        connN: "—",
        connUnit: "活解析",
        scoreParts: [["跟随上游", "DNS 活解析"]],
        actions: [
          { text: "踢", title: "把该域名踢出转发池", run: (btn) => kickIP(domain, btn) },
        ],
      }));
    }
    box.append(frag);
    $("poolFoot").textContent = "「测」= 重新体检（WS 延迟 + VLESS 测速）；踢后后台自动补位；设备粘性下同设备恒走同一上游。";
  }

  /* ---------- 渲染：池候选 B 榜 ---------- */
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
      empty.className = "hero-foot";
      empty.textContent = "候选已全部登基或为空。";
      box.append(empty);
      return;
    }
    const frag = document.createDocumentFragment();
    for (const ip of candidates) {
      const result = results.get(ip);
      const scoreParts = [];
      if (result?.latency) scoreParts.push([`${result.latency}`, "ms"]);
      if (!scoreParts.length) scoreParts.push(["候选", "3层已过筛"]);
      frag.append(buildRow({
        addr: ip,
        label: SRC_LABEL[byIp[ip]] || "候选",
        badgeCls: byIp[ip] || "",
        connN: "—",
        connUnit: "候补",
        scoreParts,
        actions: [
          { text: "测", cls: "kick probe", title: "测 WS 延迟" + (vlessEnabled() ? " + VLESS 速度" : ""), run: (btn) => probeIP(ip, btn) },
          { text: "顶", cls: "kick promote", title: "体检通过后顶替在位最差成员", run: (btn) => promoteIP(ip, btn) },
        ],
      }));
    }
    box.append(frag);
  }

  function vlessEnabled() {
    return false; // 由 /api/config 的 PROXY_VLESS_PROBE 决定；先乐观显示，后端实测为准
  }

  /* ---------- 渲染：供给心跳 ---------- */
  function renderBeacons() {
    const byIp = poolSnapshot?.sourceByIp || {};
    const tiers = [
      ["user", "手动"],
      ["subscription", "订阅"],
      ["bestcf", "BestCF"],
      ["official", "官方段"],
      ["proxy", "社区"],
    ];
    const box = $("sourceBeacons");
    if (!box) return;
    box.replaceChildren();
    for (const [key, label] of tiers) {
      let n = 0;
      for (const ip of Object.keys(byIp)) if (byIp[ip] === key) n++;
      const el = document.createElement("span");
      el.className = `beacon ${n ? "ok" : "warn"}`;
      const dot = document.createElement("span");
      dot.className = `pulse ${n ? "ok" : ""}`;
      const text = document.createElement("span");
      text.textContent = `${label} `;
      const b = document.createElement("b");
      b.textContent = n;
      text.append(b);
      el.append(dot, text);
      box.append(el);
    }
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
        renderPool();
        renderBeacons();
      }
      await Promise.allSettled([pullPool(), pullStatus(), pullUpstreams()]);
      const n = snap.active?.ips?.length || 0;
      const err = snap.active?.error;
      toast(err ? `优选完成但保留旧池：${err}` : `优选完成，${n} 个 IP 在位`);
    } catch (e) {
      toast(`优选失败：${e.message}`);
    }
  }

  /* ---------- 动作二：手动供 IP ---------- */
  async function supply() {
    const text = $("supplyInput").value.trim();
    if (!text) {
      $("supplyInput").focus();
      toast("先粘贴 IP、域名或订阅内容");
      return;
    }
    invalidatePoll("pool");
    invalidatePoll("status");
    invalidatePoll("upstreams");
    try {
      const data = await busy($("supplyBtn"), "supplyLabel", () =>
        api("/api/cfnat/proxy-scan/apply", {
          method: "POST",
          body: JSON.stringify({ ips: text, subscription: text, limit: 500 }),
        }));
      const parts = [`${data.applied || 0} 个 IP`];
      if (data.domains) parts.push(`${data.domains} 个域名`);
      toast(`手动供给已应用：${parts.join(" + ")}（后台可继续复核）`);
      $("supplyInput").value = "";
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`优选入池失败：${e.message}`);
    }
  }

  /* ---------- 动作三：单 IP 体检（测延迟 + 可选测速） ---------- */
  async function probeIP(ip, button) {
    button.disabled = true;
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
      if (data.vless) {
        if (data.vless.ok) msg += ` · 测速 ${Number(data.vless.mbps).toFixed(1)} Mbps`;
        else msg += ` · 测速失败：${data.vless.error}`;
      }
      toast(msg);
    } catch (e) {
      toast(`体检失败：${e.message}`);
    } finally {
      button.disabled = false;
      button.textContent = "测";
    }
  }

  /* ---------- 动作四：单 IP 顶替 ---------- */
  async function promoteIP(ip, button) {
    if (!window.confirm(`让 ${ip} 顶替在位最差成员？\n体检通过即换座，立即生效。`)) return;
    await doPromote(ip, button);
  }

  // doPromote 是顶替的实际动作，供 B 榜按钮与「指定登基」输入框共用。
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
      toast(data.already
        ? `${ip} 已在位，无需顶替`
        : `${ip} 已顶替 ${data.replaced} 登基`);
      await Promise.allSettled([pullPool(), pullUpstreams()]);
      return true;
    } catch (e) {
      toast(`顶替失败：${e.message}`);
      return false;
    } finally {
      if (button) button.disabled = false;
    }
  }

  /* ---------- 动作四之一：指定任意 IP 登基 ---------- */
  async function pinSpecifiedIP() {
    const input = $("pinIp");
    const button = $("pinBtn");
    const ip = input.value.trim();
    if (!ip) {
      toast("先填一个公网 IPv4");
      input.focus();
      return;
    }
    if (!/^\d{1,3}(\.\d{1,3}){3}$/.test(ip)) {
      toast("格式不对，要 IPv4 四段，如 104.16.0.1");
      return;
    }
    button.disabled = true;
    try {
      const ok = await doPromote(ip, null);
      if (ok) input.value = "";
    } finally {
      button.disabled = false;
    }
  }

  /* ---------- 动作五：踢皇 ---------- */
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

  /* ---------- 日志页 ---------- */
  async function pullLogs() {
    const version = beginPoll("logs");
    try {
      const data = await api(`/api/logs?target=${$("logTarget").value}&lines=200`);
      if (!isCurrentPoll("logs", version) || currentPage !== "logs") return;
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
  $("benchRefresh").addEventListener("click", pullPool);
  $("pinBtn").addEventListener("click", pinSpecifiedIP);
  $("pinIp").addEventListener("keydown", (e) => {
    if (e.key === "Enter") pinSpecifiedIP();
  });
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

  async function boot() {
    $("footClock").textContent = new Date().toLocaleDateString();
    route();
    await Promise.allSettled([pullStatus(), pullPool(), pullUpstreams(), pullLan()]);
    setInterval(pullStatus, 3000);
    setInterval(pullUpstreams, 4000);
    setInterval(pullPool, 9000);
    setInterval(pullLan, 12000);
    setInterval(() => { if (currentPage === "logs") pullLogs(); }, 5000);
  }
  boot();
})();
