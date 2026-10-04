/* 优选台全部逻辑：三个用户动作 + 一屏自动状态。无路由、无页面、无配置面板。 */
(() => {
  const SRC_LABEL = {
    user: "手动", subscription: "订阅", preferred: "优选解析",
    cfdata: "CFdata", official: "官方段", proxy: "社区",
  };
  let poolSnapshot = null;   // proxy-candidates 快照
  let upstreams = {};        // ip -> {connections, states, inPool}
  let lanPeers = -1;
  let logOpen = false;
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
  async function pullStatus() {
    const version = beginPoll("status");
    try {
      const st = await api("/api/status");
      if (!isCurrentPoll("status", version)) return;
      const running = !!st.cfnat?.running;
      $("cfnatPulse").className = `pulse ${running ? "ok" : "bad"}`;
      $("cfnatState").textContent = running ? "转发运行中" : "转发已停止";
      $("topbarSub").textContent = running ? "CFnat 转发已启动" : "转发已停止";
    } catch (_) { /* 心跳容错 */ }
  }

  async function pullPool() {
    const version = beginPoll("pool");
    try {
      const snap = await api("/api/cfnat/proxy-candidates");
      if (!isCurrentPoll("pool", version)) return;
      poolSnapshot = snap;
      renderPool();
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
    } catch (_) { /* 心跳容错 */ }
  }

  /* ---------- 渲染：在位皇族 ---------- */
  function renderPool() {
    const active = poolSnapshot?.active || {};
    const ips = active.ips || [];
    const results = new Map((active.results || []).map((r) => [r.ip, r]));
    const byIp = poolSnapshot?.sourceByIp || {};
    const upstreamTotal = Object.values(upstreams)
      .filter((u) => u.inPool)
      .reduce((s, u) => s + (Number(u.connections) || 0), 0);

    $("poolHeadline").textContent = ips.length
      ? `${ips.length} 个 IP 在位 · ${upstreamTotal} 条连接在途`
      : "皇位空悬——点「立即优选」开考";
    $("nextRefresh").textContent = `下次候选刷新 ${fmtTime(poolSnapshot?.nextRefresh)}`;

    const box = $("poolRows");
    box.replaceChildren();
    if (!ips.length) {
      const empty = document.createElement("p");
      empty.className = "hero-foot";
      empty.textContent = "无在位 IP。候选会按供给层汇流；是否自动换池由 PROXY_AUTO_APPLY 控制。";
      box.append(empty);
      return;
    }
    const frag = document.createDocumentFragment();
    for (const ip of ips) {
      const result = results.get(ip);
      const up = upstreams[ip];
      const connN = up?.inPool ? (Number(up.connections) || 0) : 0;

      const row = document.createElement("div");
      row.className = "ip-row";

      const main = document.createElement("div");
      main.className = "ip-main";
      const addr = document.createElement("span");
      addr.className = "ip-addr";
      addr.textContent = ip;
      const src = document.createElement("span");
      src.className = "ip-src";
      const badge = document.createElement("span");
      badge.className = `src-badge ${byIp[ip] || ""}`;
      badge.textContent = SRC_LABEL[byIp[ip]] || "已登基";
      src.append(badge);
      main.append(addr, src);

      const conn = document.createElement("div");
      conn.className = `ip-conn${connN ? " live" : ""}`;
      const n = document.createElement("span");
      n.className = "n";
      n.textContent = String(connN);
      const u = document.createElement("span");
      u.className = "u";
      u.textContent = "连接";
      conn.append(n, u);

      const score = document.createElement("div");
      score.className = "ip-score";
      if (result?.downloadMbps) {
        const m = document.createElement("span");
        m.className = "score-item";
        m.innerHTML = "";
        const b = document.createElement("b");
        b.textContent = `${result.downloadMbps.toFixed(1)}`;
        const s = document.createElement("span");
        s.textContent = "Mbps";
        m.append(b, s);
        score.append(m);
      }
      if (result?.dataLatency) {
        const l = document.createElement("span");
        l.className = "score-item";
        const b = document.createElement("b");
        b.textContent = `${result.dataLatency}`;
        const s = document.createElement("span");
        s.textContent = "ms";
        l.append(b, s);
        score.append(l);
      }
      if (up && !up.inPool) {
        const warn = document.createElement("span");
        warn.className = "score-item";
        const b = document.createElement("b");
        b.textContent = "池外";
        const s = document.createElement("span");
        s.textContent = "兜底/探针";
        warn.append(b, s);
        score.append(warn);
      }

      const kick = document.createElement("button");
      kick.className = "kick";
      kick.type = "button";
      kick.textContent = "踢";
      kick.title = "把这个 IP 踢出皇位并立即生效";
      kick.addEventListener("click", () => kickIP(ip, kick));

      row.append(main, conn, score, kick);
      frag.append(row);
    }
    box.append(frag);

    const domains = active.domains || [];
    $("poolFoot").textContent = domains.length
      ? `兜底域名 ${domains.length} 个在列（主池全部拨号失败才启用）`
      : "换池防抖已启用：后台只在更优时换血，卡了就手动踢。";
  }

  /* ---------- 渲染：自动化心跳 ---------- */
  function renderBeacons() {
    const byIp = poolSnapshot?.sourceByIp || {};
    const tiers = [
      ["user", "手动"],
      ["subscription", "订阅"],
      ["preferred", "优选解析"],
      ["cfdata", "CFdata"],
      ["official", "官方段"],
      ["proxy", "社区"],
    ];
    const box = $("sourceBeacons");
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
      toast(`手动供给已应用：${data.applied || 0} 个 IP（后台可继续复核）`);
      $("supplyInput").value = "";
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`优选入池失败：${e.message}`);
    }
  }

  /* ---------- 动作三：踢皇 ---------- */
  async function kickIP(ip, button) {
    if (!window.confirm(`踢掉 ${ip}？\n在途连接会瞬断重连，剩余 IP 立即接管。`)) return;
    button.disabled = true;
    invalidatePoll("pool");
    invalidatePoll("status");
    invalidatePoll("upstreams");
    try {
      const data = await api("/api/cfnat/active/kick", {
        method: "POST",
        body: JSON.stringify({ ip }),
      });
      toast(`已踢掉 ${ip}，剩 ${data.remaining} 个 IP 在位`);
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`踢除失败：${e.message}`);
    } finally {
      button.disabled = false;
    }
  }

  /* ---------- 日志抽屉 ---------- */
  async function pullLogs() {
    if (!logOpen) return;
    const version = beginPoll("logs");
    try {
      const data = await api(`/api/logs?target=${$("logTarget").value}&lines=200`);
      if (!isCurrentPoll("logs", version) || !logOpen) return;
      logLines = data.lines || [];
      renderLogs();
    } catch (_) { /* 容错 */ }
  }

  function renderLogs() {
    const box = $("logBox");
    const q = $("logQuery").value.trim().toLowerCase();
    const lines = q ? logLines.filter((l) => String(l).toLowerCase().includes(q)) : logLines;
    box.textContent = lines.length ? lines.join("\n") : "暂无日志";
    if ($("logFollow").checked) box.scrollTop = box.scrollHeight;
  }

  function toggleLogs() {
    logOpen = !logOpen;
    $("logDrawer").hidden = !logOpen;
    $("logToggle").setAttribute("aria-expanded", String(logOpen));
    $("logToggle").textContent = logOpen ? "收起日志" : "日志";
    if (logOpen) pullLogs();
  }

  /* ---------- 装配 ---------- */
  $("optimizeBtn").addEventListener("click", optimize);
  $("supplyBtn").addEventListener("click", supply);
  $("logToggle").addEventListener("click", toggleLogs);
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
    await Promise.allSettled([pullStatus(), pullPool(), pullUpstreams(), pullLan()]);
    setInterval(pullStatus, 3000);
    setInterval(pullUpstreams, 4000);
    setInterval(pullPool, 9000);
    setInterval(pullLan, 12000);
    setInterval(pullLogs, 5000);
  }
  boot();
})();
