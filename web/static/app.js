/* 皇位厅全部逻辑：主页（在位/供给/候选/日志/设备）+ 订阅页（维护者/本子）。
   两个页面共用这一份脚本，按「页面上有没有对应元素」决定拉什么、渲染什么。
   零依赖。API 契约见 web/httpapi.go；mock 形状见 docs/ui-demo/index.html。 */
(() => {
  const $ = (id) => document.getElementById(id);
  // 本脚本同时服务主页与 /subscription。有些元素只在其中一页存在，
  // 直接取属性会抛 TypeError 把整份脚本带崩——所有按页可选的东西都先问它。
  const has = (id) => !!$(id);
  // 页面身份：主页有在位表（throneRows），订阅页有本子表（subEntryBody）。
  // 用元素存在与否判断，而不是看 location.pathname——同一份脚本被两页共用，
  // 路由前缀以后怎么改都不影响这里。
  const HOME_PAGE = has("throneRows");
  const SUB_PAGE = has("subEntryBody");
  // 按页可选的事件绑定。元素不在这一页上就静默跳过，别让一个 null 把整份脚本带崩。
  const on = (id, evt, fn) => { const el = $(id); if (el) el.addEventListener(evt, fn); };
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
  const LIVE_TIERS = [["user", "手动"], ["cfdata", "CFdata"], ["subscription", "订阅"], ["bestcf", "BestCF"]];

  let poolSnapshot = null;   // proxy-candidates 快照（含 active）
  let upstreams = {};        // ip -> {connections, states, inPool}
  let domainLat = {};        // 优选域名 -> {ms, ok, at}
  let forward = null;        // /api/status.forward 在途转发实况
  let lanSnapshot = null;    // /api/cfnat/connections 设备名册（在途连接数）
  let stickyOn = true;       // /api/config.cfnat.sticky 设备粘性
  let sourceHealth = {};     // /api/status.sources 健康账
  let sourceChannels = [];   // /api/status.sourceChannels 四层渠道汇总
  let speedPath = null;      // /api/status.speedPath 测速通路自检
  let poolSegments = {};     // /api/status.poolSegments：当前池的 CF 段分布
  let domainFallback = [];   // /api/status.domainFallback：非空＝域名闸正在降级兜底
  let poolBlock = null;      // /api/status.poolBlock：最近一次「换池被拦」的结构化原因
  let qualityByIp = {};      // /api/status.qualityScheduler.records：最近一次探测成绩
  let probeByIp = {};        // /api/status.activeProbe.members：在位验活（WS 握手）成绩
  let activeProbeThreshold = 3;  // /api/status.activeProbe.failThreshold：连败几次请下台
  let activeProbeSlowMs = 1500;  // /api/status.activeProbe.slowMs：握手超过它就判慢
  let activeProbeSlowStrikeLimit = 2;  // 连续慢几轮才动手
  let pinnedLost = [];       // /api/status.activeProbe.pinnedLost：原固定成员被摘的记录
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
      if (has("cfnatDot")) {
        $("cfnatDot").className = `st ${running ? "ok" : "idle"}`;
        $("cfnatDot").textContent = running ? "●" : "◌";
        $("cfnatState").textContent = running ? "转发运行中" : "转发已停止";
      }
      if (st.sources && typeof st.sources === "object") {
        sourceHealth = st.sources;
        sourceChannels = Array.isArray(st.sourceChannels) ? st.sourceChannels : [];
        speedPath = st.speedPath || null;
        renderHealth();
      }
      forward = st.forward || null;
      renderForward();
      // 设备行的出口来自 forward.clients（日志），连接数来自设备名册。
      // 两边都在刷新，拿到任何一边都要重画一次，否则新上线的设备要等到
      // 下一次全量 status 轮询才出现。
      if (lanSnapshot) renderLan(lanSnapshot);
      poolSegments = st.poolSegments || {};
      domainFallback = Array.isArray(st.domainFallback) ? st.domainFallback : [];
      poolBlock = st.poolBlock || null;
      renderPoolHealth();
      const recs = st.qualityScheduler?.records;
      qualityByIp = {};
      if (Array.isArray(recs)) {
        for (const rec of recs) if (rec && rec.ip) qualityByIp[rec.ip] = rec;
      }
      // 在位验活是唯一「对生效池每个 IP 定时体检」的账本：质量调度器只测候选、
      // 不碰在位成员，所以快照里 results 的延迟可能是几轮前的旧账。这一份才是
      // 「它现在到底多慢」。不读它，页面上就没有任何东西能指出「这个成员在拖后腿」。
      const members = st.activeProbe?.members;
      probeByIp = members && typeof members === "object" ? members : {};
      activeProbeThreshold = Number(st.activeProbe?.failThreshold) || 3;
      activeProbeSlowMs = Number(st.activeProbe?.slowMs) || 1500;
      activeProbeSlowStrikeLimit = Number(st.activeProbe?.slowStrikeLimit) || 2;
      pinnedLost = Array.isArray(st.activeProbe?.pinnedLost) ? st.activeProbe.pinnedLost : [];
      // 别处（另一个标签页、脚本、定时任务）可能正在跑验证。页面刷新后
      // 必须接上这轮进度，否则按钮会一直显示可点，点下去收到 429 才明白。
      const rv = st.revalidate;
      if (rv && rv.running) {
        renderRevalidateProgress(rv);
        revalidateWasRunning = true;
        if (!revalidateTimer) revalidateTimer = setInterval(pollRevalidate, 2000);
      } else if (!revalidateTimer) {
        renderRevalidateProgress(rv);
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
      lanSnapshot = snap;
      renderLan(snap);
    } catch (_) { /* 心跳容错 */ }
  }

  /* ---------- 通用行渲染（在位 + 候选共用） ---------- */
  function buildRow({ addr, colo, label, connLive, connN, connUnit, scoreParts, actions, check, flag }) {
    const row = document.createElement("div");
    row.className = check && check.off ? "ip-row off" : flag?.tone === "live" ? "ip-row serving" : "ip-row";
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
    // 「在转」/「近用」不是装饰：粘性布局下只有很少几个成员真的承载流量，
    // 其余全是待命的。不标出来，用户看到的是一排等价的行，分不清谁在干活。
    if (flag) {
      const f = Object.assign(document.createElement("span"), { className: `chip mono ${flag.tone}`, textContent: flag.text });
      if (flag.title) f.title = flag.title;
      main.append(f);
    }
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
    for (const [big, unit, tone] of scoreParts) {
      const item = document.createElement("span");
      item.className = "score-item" + (tone ? " " + tone : "");
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

  /* ---------- 渲染：在途转发目标（cfnat 日志实况） ----------
     一设备一行，形如「192.168.1.11 转发目标 47.242.15.101」。
     用户要回答的问题是「我这台设备现在走哪个出口」，全池分布那一行回答不了。
     没有客户端字段（旧版 cfnat 日志）时退回旧的整体分布，不显示空表。 */
  function renderForward() {
    const el = $("forwardLine");
    if (!el) return;
    el.replaceChildren();
    const clients = forward?.clients || [];
    if (clients.length) {
      for (const c of clients) {
        const row = document.createElement("span");
        row.className = "fwd-row";
        row.append(Object.assign(document.createElement("b"), { className: "mono", textContent: c.client || "?" }));
        row.append(document.createTextNode(" 转发目标 "));
        row.append(Object.assign(document.createElement("span"), { className: "mono fwd-host", textContent: c.host || "–" }));
        // 出口不止一个，说明这台设备走到过回退成员上。不是错误，但值得看见。
        const alt = (c.counts || []).length;
        if (alt > 1) {
          row.append(Object.assign(document.createElement("em"), {
            className: "fwd-note",
            textContent: `（近 ${c.total} 次走过 ${alt} 个出口）`,
          }));
        }
        el.append(row);
      }
      return;
    }
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
  // 段分布与降级标记：2026-10-22 事故里池子只有一个段，该段一挂全灭，
  // 而域名闸在降级兜底时没有任何界面提示——两者都必须一眼可见。
  function renderPoolHealth() {
    const segs = Object.entries(poolSegments || {});
    const total = segs.reduce((s, [, n]) => s + (Number(n) || 0), 0);
    const box = $("segLine");
    if (box) {
      if (!segs.length) {
        box.innerHTML = '<span class="foot">段分布 –</span>';
      } else {
        const sorted = segs.sort((a, b) => b[1] - a[1]);
        const segCount = sorted.length;
        const weak = segCount < 2;
        const bars = sorted.map(([seg, n]) => {
          const pct = total ? Math.round((n / total) * 100) : 0;
          return `<span class="seg-chip" title="${seg} · ${n} 个 · ${pct}%">` +
            `<b class="mono">${seg.split("/")[0].replace(/\.0$/, "")}</b>` +
            `<i style="width:${Math.max(pct, 4)}%"></i><em class="mono">${n}</em></span>`;
        }).join("");
        box.innerHTML =
          `<span class="foot">段分布 · ${segCount} 段${weak ? ' <b class="warn">（单段，抗风险弱）</b>' : ""}</span>` +
          `<span class="seg-chips">${bars}</span>`;
      }
    }
    const flag = $("degradedFlag");
    if (flag) {
      if (domainFallback.length) {
        flag.hidden = false;
        flag.innerHTML = `<b>降级运行</b>：${domainFallback.length} 个转发域名体检未达标，` +
          `为避免空池已兜底放行（${domainFallback.join("、")}）`;
      } else {
        flag.hidden = true;
        flag.textContent = "";
      }
    }

    // 换池被拦的说明。后端一直在算 poolBlock（reason/source/at/nextAllowedAt），
    // 但前端此前从没读过它——于是「为什么没换线」只有钻接口才看得见，页面
    // 一片平静。这正是「静默吞单」在界面上的样子，必须写出来。
    const block = $("poolBlockFlag");
    if (block) {
      const text = poolBlockText(poolBlock);
      if (text) {
        block.hidden = false;
        block.innerHTML = text;
      } else {
        block.hidden = true;
        block.textContent = "";
      }
    }
  }

  // 换池拦截的中文说明。reason 是后端固定枚举，这里只做翻译与补充时限。
  // rescue 不会出现在这里：救援是「放行」，被放行的换池不记 poolBlock。
  const POOL_BLOCK_REASON = {
    cooldown: "冷却期未过",
    healthy: "现生效池全部健康，不必换",
  };
  function poolBlockText(pb) {
    if (!pb || !pb.blocked) return "";
    const src = pb.source === "scheduler" ? "调度器"
      : pb.source === "auto-apply" ? "自动终审" : (pb.source || "未知入口");
    const reason = POOL_BLOCK_REASON[pb.reason] ?? pb.reason ?? "原因未知";
    let when = "";
    if (pb.nextAllowedAt) {
      const until = new Date(pb.nextAllowedAt).getTime();
      if (!isNaN(until)) {
        // nextAllowedAt 是「什么时候可以再试」，与 fmtAgo 的方向相反，
        // 所以这里自己算，不能复用那个「多久以前」的函数。
        const mins = Math.ceil((until - Date.now()) / 60000);
        if (mins > 0) when = `，约 ${mins} 分钟后可再试`;
      }
    } else if (pb.at) {
      when = `（${fmtAgo(pb.at)}）`;
    }
    return `<b>本轮没有换线</b>：${src}想换，被「${reason}」拦下${when}。` +
      `地址本身可能没问题，只是防抖闸没放行。`;
  }

  /* ---------- 「谁在转」：在效成员高亮的判据 ----------
     用户的原话是「将在生效的 ip 特别标注出来」。这里要回答的不是「池里有谁」——
     那一列本来就是池，全都「在位」——而是「此刻流量真的落在谁身上」。
     两条证据合并成三档：
       live   此刻 cfnat 有活着的出站连接（/proc 实况，最强证据）
       recent 最近一段时间实际拨通过（cfnat 日志尾部，含刚断开的）
       其余   在位但没在干活（待命 / 用户关掉 / 被判死）
     粘性分发下通常只有设备数那么几个成员是 live，这正是用户想一眼看出来的事。 */
  function memberTraffic() {
    const live = new Map();
    const clients = new Map();
    for (const [ip, up] of Object.entries(upstreams)) {
      if (!up?.inPool) continue;
      const n = Number(up.connections) || 0;
      if (n > 0) live.set(ip, n);
    }
    for (const c of forward?.clients || []) {
      if (!c?.host) continue;
      if (!clients.has(c.host)) clients.set(c.host, []);
      clients.get(c.host).push(c.client || "?");
    }
    const recent = new Map();
    for (const item of forward?.counts || []) {
      if (item?.host) recent.set(item.host, Number(item.count) || 0);
    }
    return { live, recent, clients };
  }

  // 给一行挑徽标：优先「在转」，其次「近用」；都没有就不加，避免一派徽标把重点冲淡。
  function trafficFlag(addr, traffic) {
    const n = traffic.live.get(addr) || 0;
    const who = traffic.clients.get(addr) || [];
    if (n > 0) {
      return {
        tone: "live",
        text: "在转",
        title: `${n} 条出站连接正指向它` + (who.length ? `，来自 ${who.join("、")}` : ""),
      };
    }
    const recent = traffic.recent.get(addr) || 0;
    if (recent > 0) {
      return {
        tone: "recent",
        text: "近用",
        title: `最近窗口内拨通过 ${recent} 次，当前没有活连接` + (who.length ? `；来自 ${who.join("、")}` : ""),
      };
    }
    return null;
  }

  // 慢的默认标准是后端 PROXY_ACTIVE_PROBE_SLOW_MS（默认 1500ms），由 /api/status
  // 带下来。这里只用于标注，不参与任何决策——决策在后端，页面只负责说实话。

  // 在位 IP 的成绩栏。顺序上「在位验活」优先：「体检」按钮读的是质量调度器，
  // 而那个调度器只测候选、不碰在位成员，快照里的延迟可能是几轮前的旧账。
  // 只有验活账本回答得了「它此刻到底多慢」。
  //
  // 三种状态必须分开说，因为它们会走向不同的结局：
  //   连败     → 到阈值就换掉；
  //   连续慢   → 到阈值换掉，但固定成员只提示不动手；
  //   暂留     → 判死或判慢，却找不到合格替补，池子先不缩水，理由写在账上。
  function memberScore(ip, result, pinned) {
    const probe = probeByIp[ip];
    const parts = [];
    const fails = Number(probe?.consecutiveFailures) || 0;
    const threshold = Number(activeProbeThreshold) || 3;
    const slowMs = Number(activeProbeSlowMs) || 1500;
    const slowLimit = Number(activeProbeSlowStrikeLimit) || 2;
    if (probe?.held) {
      parts.push(["暂留", probe.heldReason || "无合格替补", "warn"]);
      if (probe.lastLatencyMs) parts.push([String(probe.lastLatencyMs), "ms", "bad"]);
      return parts;
    }
    if (fails > 0) {
      parts.push([`连败 ${fails}`, fails + 1 >= threshold ? "下轮请下台" : `满 ${threshold} 次下台`, "bad"]);
      if (probe?.lastLatencyMs) parts.push([String(probe.lastLatencyMs), "ms", "bad"]);
      return parts;
    }
    const strikes = Number(probe?.slowStrikes) || 0;
    const latency = Number(probe?.lastLatencyMs) || Number(result?.dataLatency) || 0;
    if (result?.downloadMbps) parts.push([result.downloadMbps.toFixed(1), "Mbps"]);
    if (strikes > 0) {
      const due = strikes + 1 >= slowLimit;
      const verdict = pinned ? "固定成员不自动换" : due ? "下轮请下台" : `连续 ${slowLimit} 轮请下台`;
      parts.push([`慢 ${strikes}`, verdict, "bad"]);
      if (latency > 0) parts.push([String(latency), "ms", "bad"]);
      return parts;
    }
    if (latency > 0) {
      const tone = latency > slowMs ? "bad" : latency > slowMs / 2 ? "warn" : "";
      parts.push([String(latency), "ms", tone]);
    }
    if (!parts.length) parts.push(["在位", "体检随时点"]);
    return parts;
  }

  function renderThrone() {
    if (!HOME_PAGE) return;   // 订阅页没有在位表，不必渲染
    const active = poolSnapshot?.active || {};
    const ips = active.ips || [];
    const domains = active.domains || [];
    const results = new Map((active.results || []).map((r) => [r.ip, r]));
    const byIp = poolSnapshot?.sourceByIp || {};
    const upstreamTotal = Object.values(upstreams)
      .filter((u) => u.inPool)
      .reduce((s, u) => s + (Number(u.connections) || 0), 0);
    // 「谁真的在转」有两个证据源，口径不同、都有用：
    //   liveConn  = 此刻 cfnat 到该 IP 有活着的出站连接（/proc 实况）；
    //   recent    = 最近 2000 行 cfnat 日志里出现过该出口（近窗，包含刚断开的）。
    // 只看连接数会让刚切走的目标瞬间「消失」，只看日志又会让早已死掉的留着亮。
    const traffic = memberTraffic();
    $("tallyPool").textContent = ips.length;
    $("tallyDom").textContent = domains.length;
    $("tallyConn").textContent = upstreamTotal;
    $("coloLine").textContent = coloLineText(ips, results);
    const foot = $("throneFoot");
    if (foot) {
      const legend = "「在转」= 此刻有出站连接指向它；「近用」= 最近拨通过、当前无活连接；「固定」= 排到转发最前。"
        + "判慢/判死会自动换人，换不到合格替补时暂留原位并标「暂留」，不缩池。";
      // 原固定成员被系统摘掉必须明说：页面上还标着「固定」而脚下已经没它，
      // 等于让用户对着一份过期的配置做判断。
      const lost = pinnedLost.length
        ? "；已失效下线：" + pinnedLost.slice(0, 3).map((p) => p.member || p.Member).join("、")
        : "";
      foot.textContent = legend + lost + (stickyOn
        ? "设备粘性开：一台设备钉一个出口，设备之间错开。"
        : "设备粘性关：按连接轮转，同一设备的请求会散到不同出口。");
    }
    const disabled = new Set(active.disabled || []);
    const prioSet = new Set(active.priority || []);
    const total = ips.length + domains.length;
    const enabledCount = [...ips, ...domains].filter((m) => !disabled.has(m)).length;
    $("throneLock").checked = !!active.lock;
    $("throneAll").checked = total > 0 && enabledCount === total;
    // 「只转固定的」开着时，真正被转发的只有「既固定、又没被关掉」的那几个。
    // 提示必须按这个交集算，否则显示 5 个固定成员、实际只转 3 个，又是一个
    // 页面与转发不符的口子。
    const pinnedOn = [...ips, ...domains].filter((m) => prioSet.has(m) && !disabled.has(m)).length;
    $("throneCtlHint").textContent = !total
      ? "皇位空悬。"
      : active.lock
        ? `只转固定的：当前只转发 ${pinnedOn} 个（固定且未关闭），其余 ${total - pinnedOn} 个不参与。`
        : `已勾 ${enabledCount} / ${total} 参与转发；固定的成员排在转发最前。`;

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
      const scoreParts = memberScore(ip, result, prioSet.has(ip));
      const pinned = prioSet.has(ip);
      frag.append(buildRow({
        addr: ip,
        flag: trafficFlag(ip, traffic),
        colo: result?.colo || "",
        label: (pinned ? "固定 · " : "") + (SRC_LABEL[byIp[ip]] || byIp[ip] || "已登基"),
        connLive: connN > 0,
        connN: String(connN),
        connUnit: "连接",
        check: { group: "ip", off: disabled.has(ip) },
        scoreParts,
        actions: [
          { text: "测", title: "测 WS 延迟" + (vlessOn ? " + VLESS 速度" : ""), run: (btn) => probeIP(ip, btn) },
          { text: pinned ? "取消固定" : "固定", title: pinned ? "取消固定：回到普通成员" : "固定：排到转发最前；再勾「只转固定的」即只转它", run: () => pinMember(ip, !pinned) },
          { text: "踢", title: "把这个 IP 踢出在位并立即生效", run: (btn) => kickIP(ip, btn) },
        ],
      }));
    }
    // 优选域名行：升格为 -fixed 直连成员，拨号时按当前 DNS 活解析。
    // 成绩栏显示的是「该域名此刻当入口跑我这条隧道有多快、稳不稳」：
    // 速度 / TCP 握手 / 成功率 / 抖动（P95÷P50）。
    if (domains.length) frag.append(groupHead("优选域名", "domain", domains.length));
    for (const domain of order(domains)) {
      const lat = domainLat[domain];
      const scoreParts = [];
      // 域名现在也在位验活（每轮 WS 握手），死/慢的结论比 30 分钟一轮的体检快得多。
      // 两个账本一起看：验活管「此刻通不通」，体检管「这一段时间稳不稳」。
      const probe = probeByIp[domain];
      if (probe?.held) {
        scoreParts.push(["暂留", probe.heldReason || "无合格替补", "warn"]);
      } else if ((Number(probe?.consecutiveFailures) || 0) > 0) {
        const threshold = Number(activeProbeThreshold) || 3;
        scoreParts.push(["验活连败 " + probe.consecutiveFailures, `${threshold} 次下线进观察期`, "bad"]);
      } else if (!lat) {
        scoreParts.push(["—", "探测中"]);
      } else if (lat.state === "dead") {
        scoreParts.push(["已死", lat.consecutiveFailures ? `连败${lat.consecutiveFailures}` : "", "bad"]);
      } else if (!lat.ok) {
        // 区分「握手不通」与「连得上、只是测速没跑完」：后者不是可达性问题，
        // 报成「不可达」会冤枉好域名，也让人误判该不该换掉它。
        if (lat.ms > 0) {
          scoreParts.push([String(lat.ms), "ms"]);
          scoreParts.push(["测速超时", "", "warn"]);
        } else {
          scoreParts.push(["不可达", "", "bad"]);
        }
      } else {
        if (lat.mbps > 0) scoreParts.push([lat.mbps.toFixed(1), "Mbps"]);
        if (lat.ms > 0) scoreParts.push([String(lat.ms), "ms"]);
        if (lat.successRate > 0) {
          const pct = Math.round(lat.successRate * 100);
          scoreParts.push([pct + "%", "成", pct < 80 ? "warn" : ""]);
        }
        if (lat.jitterRatio > 1.3) {
          scoreParts.push(["×" + lat.jitterRatio.toFixed(1), "抖", lat.jitterRatio > 1.8 ? "bad" : "warn"]);
        }
        if (lat.stage === "tcp-only") scoreParts.push(["仅TCP", "", "warn"]);
        if (!scoreParts.length) scoreParts.push(["—", "待测"]);
      }
      const pinned = prioSet.has(domain);
      frag.append(buildRow({
        addr: domain,
        flag: trafficFlag(domain, traffic),
        colo: "DNS",
        label: (pinned ? "固定 · " : "") + "域名直连",
        connLive: false,
        connN: "—",
        connUnit: "活解析",
        check: { group: "domain", off: disabled.has(domain) },
        scoreParts,
        actions: [
          { text: pinned ? "取消固定" : "固定", title: pinned ? "取消固定：回到普通成员" : "固定：排到转发最前；再勾「只转固定的」即只转它", run: () => pinMember(domain, !pinned) },
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
  // 「活跃」= 最近一次探测可达：
  //   域名看稳定性账（连续失败达阈值判死；样本不足时不误杀）；
  //   IP 看质量调度器记录。
  function isActiveMember(addr) {
    const lat = domainLat[addr];
    if (lat) return lat.state !== "dead";
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
    // 「固定」的承诺是「它开始转发」。若这个成员正被关闭着，只写 Priority 是
    // 不够的：planFixedMembers 先看 Disabled，被关的成员永远不会进 -fixed。
    // 用户会看到行上写着「固定」、连接数始终是 0。所以固定时顺手把它勾上。
    if (on) {
      const box = [...document.querySelectorAll(".memchk")].find((b) => b.dataset.addr === addr);
      if (box) box.checked = true;
    }
    invalidatePoll("pool");
    try {
      await api("/api/cfnat/active/select", {
        method: "POST",
        body: JSON.stringify({ ...currentSelection(), priority }),
      });
      toast(on ? `${addr} 已固定并参与转发` : `${addr} 已取消固定`);
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`固定失败：${e.message}`);
      await pullPool();   // 把界面拉回后端真实状态，别让失败留下一个假的勾
    }
  }

  /* ---------- 固定转发：用户亲手指定 IP 或域名 ----------
     两个入口回答同一个问题，但起点不同：
       · 行上的「固定」——已经在这个池里，只是要它排最前、别被轮换稀释；
       · 这里的输入框——不在池里，要它现在就在。
     后者走「扶持登基」的通道：不体检、直接并入生效池并置顶。用户说「我要用这个」
     时再追问「但它握手 1.2 秒，确定吗」是把责任往外推，不是把关。
     已在位且已固定的成员幂等跳过，不会白重启一次转发。 */
  async function pinTyped() {
    const text = $("pinInput").value.trim();
    if (!text) {
      $("pinInput").focus();
      toast("先填要固定的 IP 或域名");
      return;
    }
    invalidatePoll("pool");
    invalidatePoll("status");
    invalidatePoll("upstreams");
    try {
      const data = await busy($("pinBtn"), "pinBtn", () =>
        api("/api/cfnat/active/endorse", { method: "POST", body: JSON.stringify({ members: text }) }));
      if (data.unchanged) {
        toast(`${text} 已在位并已固定`);
      } else {
        const parts = [];
        if (data.ips?.length) parts.push(`${data.ips.length} 个 IP`);
        if (data.domains?.length) parts.push(`${data.domains.length} 个域名`);
        const added = parts.length ? `新增 ${parts.join(" + ")}` : "已在位";
        toast(`已固定：${added}，现在排到转发最前（共 ${data.ips_total} IP + ${data.domains_total} 域名在位）`);
        $("pinInput").value = "";
      }
      await Promise.allSettled([pullPool(), pullUpstreams()]);
    } catch (e) {
      toast(`固定失败：${e.message}`);
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
  // 渠道色阶（灰阶）：颜色跟着渠道走，不跟排名走，刷新后同一层永远同一色。
  const CHAN_INK = { user: "#ffffff", cfdata: "#c9cad2", subscription: "#8f9099", bestcf: "#5b5c64" };
  function renderChannelChart() {
    const svg = $("chanDonut");
    const legend = $("chanLegend");
    if (!svg || !legend) return;
    const rows = Array.isArray(sourceChannels) ? sourceChannels : [];
    const total = rows.reduce((n, row) => n + (Number(row.inPool) || 0), 0);
    const doms = Array.isArray(poolSnapshot?.active?.domains) ? poolSnapshot.active.domains.length : 0;
    // 状态每 3 秒轮询一次，数据没变就别重建 SVG——否则正在悬停的扇区会被替换掉。
    const sig = JSON.stringify(rows) + "|" + doms + "|" + JSON.stringify(speedPath || null);
    if (svg.dataset.sig === sig) return;
    svg.dataset.sig = sig;
    const NS = "http://www.w3.org/2000/svg";
    const cx = 60, cy = 60, r = 44, C = 2 * Math.PI * r;
    svg.replaceChildren();
    // 底环：即使一个成员都没有，也让人看出这是个环而不是加载失败。
    const base = document.createElementNS(NS, "circle");
    base.setAttribute("cx", cx); base.setAttribute("cy", cy); base.setAttribute("r", r);
    base.setAttribute("fill", "none");
    base.setAttribute("stroke", "rgb(255 255 255 / .10)");
    base.setAttribute("stroke-width", "16");
    svg.append(base);
    let acc = 0;
    for (const row of rows) {
      const n = Number(row.inPool) || 0;
      if (n <= 0 || total <= 0) continue;
      const frac = n / total;
      // 2px 的表面缝：相邻扇区不粘在一起，灰阶相邻时才分得清。
      const gap = Math.min(C * frac * 0.4, 3);
      const seg = document.createElementNS(NS, "circle");
      seg.setAttribute("cx", cx); seg.setAttribute("cy", cy); seg.setAttribute("r", r);
      seg.setAttribute("fill", "none");
      seg.setAttribute("stroke", CHAN_INK[row.channel] || "#8f9099");
      seg.setAttribute("stroke-width", "16");
      seg.setAttribute("stroke-dasharray", `${Math.max(0.5, C * frac - gap)} ${C}`);
      seg.setAttribute("stroke-dashoffset", String(-acc));
      seg.setAttribute("transform", "rotate(-90 60 60)");
      seg.setAttribute("class", "chan-seg");
      const title = document.createElementNS(NS, "title");
      title.textContent = `${row.label}：在位 ${n} 席（${Math.round(frac * 100)}%），候选 ${row.candidates || 0}，活性 ${row.alive}/${row.inPool}`;
      seg.append(title);
      svg.append(seg);
      acc += C * frac;
    }
    const big = document.createElementNS(NS, "text");
    big.setAttribute("x", cx); big.setAttribute("y", cy + 2);
    big.setAttribute("text-anchor", "middle");
    big.setAttribute("class", "chan-total mono");
    big.textContent = String(total);
    svg.append(big);
    const cap = document.createElementNS(NS, "text");
    cap.setAttribute("x", cx); cap.setAttribute("y", cy + 16);
    cap.setAttribute("text-anchor", "middle");
    cap.setAttribute("class", "chan-cap");
    cap.textContent = "在位";
    svg.append(cap);

    legend.replaceChildren();
    // 图例按活性分排序：谁最近跑得好，一眼看到；色块仍按渠道固定取色。
    const ranked = rows.slice().sort((a, b) => (Number(b.score) || 0) - (Number(a.score) || 0));
    const topChannel = ranked.length ? ranked[0].channel : "";
    for (const row of ranked) {
      const li = document.createElement("li");
      const dot = document.createElement("span");
      dot.className = "chan-dot";
      dot.style.background = CHAN_INK[row.channel] || "#8f9099";
      const name = document.createElement("span");
      name.className = "chan-name";
      name.textContent = row.label + (row.channel === topChannel && (Number(row.score) || 0) > 0 ? " · 最佳" : "");
      const num = document.createElement("span");
      num.className = "chan-num mono";
      num.textContent = String(row.inPool || 0);
      const sub = document.createElement("span");
      sub.className = "chan-sub mono";
      const share = total > 0 ? Math.round(((Number(row.inPool) || 0) / total) * 100) : 0;
      const detail = [`${share}%`, `候 ${row.candidates || 0}`, `活 ${row.alive || 0}`];
      if (row.slow) detail.push(`慢 ${row.slow}`);
      if (row.failing) detail.push(`败 ${row.failing}`);
      if (row.backoff) detail.push(`退避 ${row.backoff}`);
      sub.textContent = detail.join(" · ") + ` · 抓成 ${Math.round((Number(row.successRate) || 0) * 100)}%`;
      const meter = document.createElement("span");
      meter.className = "chan-meter";
      const fill = document.createElement("i");
      fill.style.width = `${Math.max(0, Math.min(100, Number(row.score) || 0))}%`;
      meter.append(fill);
      const score = document.createElement("span");
      score.className = "chan-score mono";
      score.textContent = String(Math.round(Number(row.score) || 0));
      li.append(dot, name, num, sub, meter, score);
      if (row.lastError) li.title = `${row.channel} 最近一次失败：${row.lastError}`;
      legend.append(li);
    }

    const dn = $("chanDomains");
    if (dn) {
      dn.hidden = doms === 0;
      dn.textContent = `另 ${doms} 个优选域名随驾，不占 IP 席位`;
    }

    const note = $("speedNote");
    if (note) {
      const sp = speedPath || {};
      const zero = Number(sp.speedOk) || 0;
      const tested = Number(sp.domains) || 0;
      const tcp = Number(sp.tcpOk) || 0;
      // 零时间的 JSON 是 "0001-01-01T00:00:00Z"，不能用真值判断。
      const started = sp.lastRound && !String(sp.lastRound).startsWith("0001");
      let text = "";
      let warn = false;
      if (!started) {
        note.hidden = true;
        return;
      } else if (sp.suspect) {
        warn = true;
        text = "测速通路可疑 · " + (sp.reason || "端点或隧道失效");
      } else if (tested === 0) {
        text = "测速通路 · 本轮无域名可测";
      } else if (tcp === 0) {
        text = `测速通路 · 域名全部 TCP 不通（${tested} 个），未计入`;
      } else if (zero === 0) {
        warn = true;
        text = `测速本轮零成绩 · ${tcp}/${tested} 可达 · 连续 ${Number(sp.zeroRounds) || 0} 轮`;
      } else {
        text = `测速通路正常 · ${fmtAgo(sp.lastRound)} · ${zero}/${tested} 出成绩`;
      }
      note.hidden = false;
      note.classList.toggle("warn", warn);
      note.textContent = text;
    }
  }

  function renderHealth() {
    renderChannelChart();
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

  /* ---------- 渲染：设备名册（陆） ----------
     每台设备多一列「转发目标」：从 cfnat 日志的 IP→出口映射里取。
     这是把两张互不相干的表（/proc 的连接数、日志里的拨号目标）按客户端
     IP 拼起来——四层转发没有别的办法知道这件事。 */
  function clientTargetMap() {
    const map = {};
    for (const c of forward?.clients || []) {
      if (c && c.client) map[c.client] = c;
    }
    return map;
  }
  function renderLan(snap) {
    const body = $("lanBody");
    if (!body) return;
    const sources = snap?.sources || [];
    body.replaceChildren();
    if (has("lanHead")) $("lanHead").textContent = `陆 · 设备名册 · ${sources.length} 台`;
    const targets = clientTargetMap();
    if (!sources.length) {
      const tr = document.createElement("tr");
      const td = document.createElement("td");
      td.colSpan = 4;
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
      // 没有该设备的拨号日志（例如刚好还没建立过新连接）就写「–」，
      // 不要拿别的设备的目标来凑数。
      const hit = targets[s.ip];
      let target = "–";
      let targetTitle = "";
      if (hit) {
        target = hit.host || "–";
        const dist = (hit.counts || []).map((c) => `${c.host}×${c.count}`).join(" · ");
        if (dist) targetTitle = `近 ${hit.total} 次：${dist}`;
      }
      const cells = [
        [s.ip || "--", "mono", ""],
        [String(s.connections ?? 0), "mono hl", ""],
        [states || "--", "", ""],
        [target, "mono", targetTitle],
      ];
      for (const [value, cls, title] of cells) {
        const td = document.createElement("td");
        td.textContent = value;
        if (cls) td.className = cls;
        if (title) td.title = title;
        tr.append(td);
      }
      frag.append(tr);
    }
    body.append(frag);
  }

  /* ---------- 动作一：立即优选（暂停 → 验证在位 → 剔除垃圾 → 恢复） ----------
     语义变了：以前点它是「去外面捞一批更好的候选」，现在是「把现在占着位置的
     这些逐个拉到真实数据面上验一遍，不合格的当场剔除」。用户点这个按钮时的
     真实处境通常是「现在很卡，清理一下」，而不是「去扩张」。

     一轮验证最长四分钟（域名要跑 VLESS 下载），后端立刻返回 202，这里轮询
     进度。同步干等的话，中间任何一层超时都会让用户以为点坏了——而那时转发
     其实已经停了、验证还在跑，正好是最不该被误判的时刻。 */
  let revalidateTimer = null;
  let revalidateWasRunning = false;

  const stopRevalidatePoll = () => {
    if (revalidateTimer) { clearInterval(revalidateTimer); revalidateTimer = null; }
  };

  function renderRevalidateProgress(st) {
    const btn = $("optimizeBtn");
    if (!btn) return;
    const label = $("optimizeLabel");
    if (!st || !st.running) {
      btn.disabled = false;
      btn.classList.remove("busy");
      if (label) label.textContent = "立即优选";
      return;
    }
    btn.disabled = true;
    btn.classList.add("busy");
    if (label) label.textContent = `${st.phase || "验证中"}${st.total ? ` ${st.tested}/${st.total}` : ""}`;
  }

  function revalidateToast(rep) {
    if (!rep) return "验证结束，但没有拿到报告";
    if (rep.aborted) return `验证中止，未作改动：${rep.note || "原因未知"}`;
    const removed = Array.isArray(rep.removed) ? rep.removed : [];
    if (!removed.length) return `验证完成：${rep.passed}/${rep.tested} 通过，未剔除任何成员`;
    const head = removed.slice(0, 3).join("、");
    const more = removed.length > 3 ? ` 等 ${removed.length} 个` : "";
    return `验证完成：剔除 ${removed.length} 个（${head}${more}），保留 ${rep.passed} 个`;
  }

  async function pollRevalidate() {
    let st;
    try { st = await api("/api/cfnat/revalidate"); } catch (_) { return; }
    const wasRunning = revalidateWasRunning;
    renderRevalidateProgress(st);
    if (st.running) { revalidateWasRunning = true; return; }
    // 收尾：停轮询、解锁按钮。只有「确实看着它跑完」的页面才弹结论，
    // 否则每次刷新主页都会重播一遍上一次的旧报告。
    stopRevalidatePoll();
    revalidateWasRunning = false;
    if (!wasRunning) return;
    await Promise.allSettled([pullPool(), pullStatus(), pullUpstreams()]);
    toast(revalidateToast(st.last));
  }

  async function optimize() {
    const btn = $("optimizeBtn");
    if (btn.disabled) return;
    let res;
    try {
      res = await fetch("/api/cfnat/revalidate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: "{}",
      });
    } catch (e) {
      toast(`验证未能启动：${e.message}`);
      return;
    }
    if (res.status === 429) { toast("已有一轮验证在进行中，等它跑完"); return; }
    if (!res.ok) {
      toast(`验证未能启动：${(await res.text()).trim() || `HTTP ${res.status}`}`);
      return;
    }
    // 后端已经开跑了。先锁按钮，避免这 2 秒的空窗里被连点第二次。
    invalidatePoll("pool");
    invalidatePoll("status");
    invalidatePoll("upstreams");
    revalidateWasRunning = true;
    renderRevalidateProgress({ running: true, phase: "暂停转发" });
    stopRevalidatePoll();
    revalidateTimer = setInterval(pollRevalidate, 2000);
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
      : "一堆 IP、优选域名或订阅都行：逐个做 WS 体检，握手在进池上限内才入池，池大小按配置封顶。";
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

  /* ---------- 订阅区（肆） ----------
     这一块与上面的「候选」不是一回事：上面是已经考过、可供建池的名单；
     这里是本子——所有被维护者供出来的地址都在，考没考过、考成什么样，
     分开记。分开的理由很实际：合成一个数就看不出「收不进来」和
     「收进来了但考不过」的区别，而这两种故障的处理办法完全不同。 */
  let subSettings = { maintainers: [], converter: "", tokenSet: false };
  let subSummary = {};
  let subEntries = [];
  let subState = "all";
  let subHistory = {};       // 维护者 -> 最近拉取记录，单独取、单独缓存
  let subLastFetch = {};     // 维护者 -> 最近一条

  // 拉取历史与候选列表分开取：历史按维护者存，与当前筛选无关，体积也大得多，
  // 没必要跟着 15 秒轮询每回都拉。只在开机、手动刷新、改配置、触发采集后取一次。
  async function pullSubscriptions(withHistory = false) {
    const version = beginPoll("subs");
    try {
      const [settings, inv] = await Promise.all([
        withHistory ? api("/api/subscriptions/settings") : Promise.resolve(null),
        api(`/api/subscriptions/inventory?state=${encodeURIComponent(subState)}&limit=300${withHistory ? "&history=1" : ""}`),
      ]);
      if (!isCurrentPoll("subs", version)) return;
      if (settings?.settings) subSettings = settings.settings;
      subSummary = inv.summary || {};
      subEntries = Array.isArray(inv.entries) ? inv.entries : [];
      if (withHistory) {
        subHistory = subSummary.fetchHistory || {};
        subLastFetch = subSummary.lastFetch || {};
      }
      renderSubscriptions();
    } catch (_) { /* 心跳容错 */ }
  }

  const SUB_STATE_LABEL = { pass: "能用", pending: "还没考", fail: "判死" };
  const subStateChip = (state) => {
    const s = state || "pending";
    const chip = document.createElement("span");
    chip.className = `state-chip ${s}`;
    chip.textContent = SUB_STATE_LABEL[s] || s;
    return chip;
  };

  function renderSubscriptions() {
    const sum = subSummary || {};
    const setNum = (id, v) => { const el = $(id); if (el) el.textContent = v == null ? "–" : String(v); };
    setNum("subTallyTotal", sum.total ?? 0);
    setNum("subTallyPending", sum.pending ?? 0);
    setNum("subTallyPass", sum.pass ?? 0);
    setNum("subTallyFail", sum.fail ?? 0);

    // 订阅页顶栏状态灯：有异常变空心，正常实心。用「最近一次采集/考试是不是
    // 报错」判断，而不是看有没有数据——本子非空但采集全挂也是一种故障。
    if (has("subDot")) {
      const broken = !!(sum.lastHarvestErr || sum.lastProbeErr);
      $("subDot").className = `st ${broken ? "idle" : "ok"}`;
      $("subDot").textContent = broken ? "◌" : "●";
      $("subStateLine").textContent = broken ? "有异常" : `本子 ${sum.total ?? 0} 条`;
    }

    const line = $("subTimeline");
    if (line) {
      const parts = [];
      parts.push(`最近采集 ${fmtAgo(sum.lastHarvestAt)}${sum.lastHarvested != null ? ` · 收到 ${sum.lastHarvested} 个` : ""}`);
      parts.push(`最近考试 ${fmtAgo(sum.lastProbeAt)}${sum.lastProbed != null ? ` · 考了 ${sum.lastProbed} 个、过 ${sum.lastPassed ?? 0} 个` : ""}`);
      if (subSettings.converter) parts.push(`转换器 ${subSettings.converter.replace(/^https:\/\//, "")}`);
      if (!subSettings.tokenSet) parts.push("token 未配置");
      line.textContent = parts.join(" ｜ ");
    }

    const flag = $("subErrorFlag");
    if (flag) {
      const errs = [];
      if (sum.lastHarvestErr) errs.push(`采集：${sum.lastHarvestErr}`);
      if (sum.lastProbeErr) errs.push(`考试：${sum.lastProbeErr}`);
      if (!subSettings.tokenSet) errs.push("订阅 token 未配置，收与考都不会跑");
      if (errs.length) {
        flag.hidden = false;
        flag.innerHTML = `<b>订阅区有异常</b>：${errs.join(" ｜ ")}`;
      } else {
        flag.hidden = true;
        flag.textContent = "";
      }
    }

    renderSubMaintainers();
    renderSubEntries();
  }

  /* 维护者表：一行一个，带启用开关、最近一次拉取成绩与删除。
     拉取记录来自后端本子，不是前端推算——页面刷新后仍然在。 */
  function renderSubMaintainers() {
    const body = $("subMaintainerBody");
    if (!body) return;
    const list = Array.isArray(subSettings.maintainers) ? subSettings.maintainers : [];
    const cnt = $("subMaintainerCount");
    if (cnt) cnt.textContent = String(list.length);
    body.replaceChildren();
    if (!list.length) {
      const tr = document.createElement("tr");
      const td = document.createElement("td");
      td.colSpan = 6;
      td.className = "inline-status";
      td.textContent = "还没有维护者。加一个域名就能开始收。";
      tr.append(td);
      body.append(tr);
      return;
    }
    const lastFetch = subLastFetch || {};
    const history = subHistory || {};
    const frag = document.createDocumentFragment();
    for (const item of list) {
      const host = item.host || "";
      const enabled = item.enabled !== false;
      const rec = lastFetch[host];
      const hist = Array.isArray(history[host]) ? history[host] : [];
      const tr = document.createElement("tr");
      if (!enabled) tr.className = "off";

      tr.append(Object.assign(document.createElement("td"), { className: "mono", textContent: host }));

      const stTd = document.createElement("td");
      stTd.append(Object.assign(document.createElement("span"), {
        className: `st ${enabled ? "ok" : "idle"}`,
        textContent: enabled ? "● 启用" : "◌ 停用",
      }));
      tr.append(stTd);

      let when = "–";
      let got = "–";
      let note = "";
      if (rec) {
        when = fmtAgo(rec.at);
        if (rec.ok) {
          got = String(rec.extracted ?? 0);
          if (rec.durationMs) note = `用时 ${(rec.durationMs / 1000).toFixed(1)}s`;
        } else {
          got = "0";
          note = rec.error || (rec.status ? `HTTP ${rec.status}` : "失败");
        }
        // 连续失败时点出来：一个维护者长期拉不到，通常是域名失效了，
        // 页面要能让人一眼看出该删哪个。
        const fails = hist.slice(-5).filter((h) => !h.ok).length;
        if (fails >= 3) note = `近 5 次失败 ${fails} 次` + (note ? " · " + note : "");
      }
      tr.append(Object.assign(document.createElement("td"), { className: "mono", textContent: when }));
      tr.append(Object.assign(document.createElement("td"), { className: "mono", textContent: got }));
      tr.append(Object.assign(document.createElement("td"), { className: "wrap", textContent: note || "—" }));

      const actTd = document.createElement("td");
      const acts = document.createElement("div");
      acts.className = "rowbtn";
      const toggle = document.createElement("button");
      toggle.className = "btn mini";
      toggle.type = "button";
      toggle.textContent = enabled ? "停用" : "启用";
      toggle.addEventListener("click", () => subUpdate(enabled ? { disable: [host] } : { enable: [host] },
        enabled ? `${host} 已停用` : `${host} 已启用`));
      const del = document.createElement("button");
      del.className = "btn mini";
      del.type = "button";
      del.textContent = "删";
      del.addEventListener("click", () => {
        if (!window.confirm(`删除维护者 ${host}？\n只是不再从它拉取；本子里已收的地址保留。`)) return;
        subUpdate({ remove: [host] }, `${host} 已删除`);
      });
      acts.append(toggle, del);
      actTd.append(acts);
      tr.append(actTd);
      frag.append(tr);
    }
    body.append(frag);
  }

  /* 本子表：每条候选一行。成绩列显示考试结论与来源，判死的带删除线。 */
  function renderSubEntries() {
    const body = $("subEntryBody");
    if (!body) return;
    body.replaceChildren();
    if (!subEntries.length) {
      const tr = document.createElement("tr");
      const td = document.createElement("td");
      td.colSpan = 6;
      td.className = "inline-status";
      td.textContent = subState === "all" ? "本子是空的。点「立即去收」拉一轮。" : "该状态下没有记录。";
      tr.append(td);
      body.append(tr);
      return;
    }
    const frag = document.createDocumentFragment();
    for (const e of subEntries) {
      const tr = document.createElement("tr");
      if (e.state === "fail") tr.className = "off";

      const addrTd = document.createElement("td");
      addrTd.className = "mono";
      addrTd.textContent = e.ip;
      if (e.colo) {
        addrTd.append(Object.assign(document.createElement("span"), {
          className: "chip mono", textContent: e.colo, style: "margin-left:6px",
        }));
      }
      tr.append(addrTd);

      const stTd = document.createElement("td");
      stTd.append(subStateChip(e.state));
      tr.append(stTd);

      const score = [];
      if (e.state === "pass") {
        if (e.mbps) score.push(`${Number(e.mbps).toFixed(1)} Mbps`);
        if (e.latencyMs) score.push(`${e.latencyMs} ms`);
        if (e.stage) score.push(e.stage === "vless" ? "真实数据面" : "仅握手");
      } else if (e.state === "fail") {
        score.push(e.errorClass || e.lastError || "失败");
        if (e.consecutiveFails) score.push(`连败 ${e.consecutiveFails}`);
      } else {
        score.push("等考");
      }
      tr.append(Object.assign(document.createElement("td"), { className: "mono", textContent: score.join(" · ") }));

      const srcTd = document.createElement("td");
      srcTd.className = "wrap";
      const sources = Array.isArray(e.sources) ? e.sources : [];
      const via = Array.isArray(e.via) ? e.via : [];
      srcTd.textContent = (sources.join("、") || "—") + (via.length ? `（经 ${via[0]}）` : "");
      tr.append(srcTd);

      tr.append(Object.assign(document.createElement("td"), { textContent: e.lastProbe ? fmtAgo(e.lastProbe) : "未考" }));

      const actTd = document.createElement("td");
      const del = document.createElement("button");
      del.className = "btn mini";
      del.type = "button";
      del.textContent = "删";
      del.title = "从本子里移除这个地址";
      del.addEventListener("click", async () => {
        if (!window.confirm(`从本子里删掉 ${e.ip}？`)) return;
        try {
          await api(`/api/subscriptions/inventory?ip=${encodeURIComponent(e.ip)}`, { method: "DELETE" });
          toast(`${e.ip} 已从本子移除`);
          await pullSubscriptions();
        } catch (err) {
          toast(`删除失败：${err.message}`);
        }
      });
      actTd.append(del);
      tr.append(actTd);
      frag.append(tr);
    }
    body.append(frag);
    const foot = $("subEntryFoot");
    if (foot) {
      const total = subSummary.total ?? 0;
      const shown = subEntries.length;
      foot.textContent = `显示 ${shown} / ${total} 条` +
        (total > shown ? "（只显示前 300 条，用上方筛选缩小范围）" : "");
    }
  }

  async function subUpdate(body, note) {
    invalidatePoll("subs");
    try {
      const data = await api("/api/subscriptions/settings", { method: "POST", body: JSON.stringify(body) });
      if (data.settings) subSettings = data.settings;
      const rejected = Array.isArray(data.rejected) ? data.rejected : [];
      toast(rejected.length ? `${note}；拒收：${rejected.join("、")}` : note);
      await pullSubscriptions(true);
    } catch (e) {
      toast(`订阅配置未生效：${e.message}`);
    }
  }

  async function subAddMaintainers() {
    const input = $("subAddInput");
    const raw = input.value.trim();
    if (!raw) { input.focus(); toast("先填一个维护者域名"); return; }
    const hosts = raw.split(/[\s,，]+/).filter(Boolean);
    await subUpdate({ add: hosts }, `已添加 ${hosts.length} 个维护者`);
    input.value = "";
  }

  async function subTrigger(stage, button) {
    button.disabled = true;
    const prev = button.textContent;
    button.textContent = "…";
    try {
      await api(`/api/subscriptions/refresh?stage=${stage}`, { method: "POST", body: "{}" });
      toast(stage === "harvest" ? "已开始拉取；完成后点「刷新」看结果" : "已开始考试；一批大约几分钟");
      // 后台任务不阻塞 HTTP，给一点时间再刷新一次，让人看到进度。
      setTimeout(() => pullSubscriptions(true), 4000);
      setTimeout(() => pullSubscriptions(true), 20000);
    } catch (e) {
      toast(`触发失败：${e.message}`);
    } finally {
      button.disabled = false;
      button.textContent = prev;
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

  /* ---------- 装配 ----------
     两页共用这份脚本，各自只绑定本页存在的元素。
     以前这里全是裸 $("x").addEventListener(...)：订阅区一拆出去，
     主页就会在某个 null 上抛 TypeError，整份脚本连时钟都不再走。 */
  on("optimizeBtn", "click", optimize);
  on("supplyBtn", "click", supply);
  on("endorseBtn", "click", endorse);
  on("pinBtn", "click", pinTyped);
  on("pinInput", "keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); pinTyped(); }
  });
  on("supplyInput", "input", updateSupplyHint);
  on("benchRefresh", "click", pullPool);
  on("logToggle", "click", () => { $("drawerLog").hidden = !$("drawerLog").hidden; if (!$("drawerLog").hidden) pullLogs(); });
  on("throneAll", "change", () => {
    const all = $("throneAll").checked;
    for (const b of document.querySelectorAll(".memchk")) b.checked = all;
    commitSelection(all ? "已全选参与转发" : "已全部取消勾选");
  });
  on("throneLock", "change", () =>
    commitSelection($("throneLock").checked ? "只转固定的：已开启" : "只转固定的：已关闭，已勾成员恢复轮转"));
  on("logTarget", "change", pullLogs);
  on("logQuery", "input", renderLogs);
  on("subAddBtn", "click", subAddMaintainers);
  on("subAddInput", "keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); subAddMaintainers(); }
  });
  on("subHarvestBtn", "click", (e) => subTrigger("harvest", e.currentTarget));
  on("subProbeBtn", "click", (e) => subTrigger("probe", e.currentTarget));
  on("subRefreshBtn", "click", () => pullSubscriptions(true));
  on("subFilter", "click", (e) => {
    const btn = e.target.closest("button[data-state]");
    if (!btn) return;
    subState = btn.dataset.state;
    for (const b of $("subFilter").querySelectorAll("button[data-state]")) {
      b.classList.toggle("on", b === btn);
    }
    invalidatePoll("subs");
    pullSubscriptions();
  });
  on("logCopy", "click", async () => {
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
    if (has("clock")) $("clock").textContent = t;
    if (has("footClock")) $("footClock").textContent = d.toLocaleDateString();
  }

  async function boot() {
    tick();
    setInterval(tick, 1000);
    if (SUB_PAGE) {
      // 订阅页：只拉订阅相关数据。不碰 cfnat 连接表、不外呼下游池——那些
      // 数据在这页上用不上，15 秒一次的轮询里白拉一遍纯属浪费。
      await pullSubscriptions(true);
      setInterval(() => pullSubscriptions(false), 15000);
      return;
    }
    updateSupplyHint();
    await pullConfig();
    // 主页不再轮询订阅本子：订阅区已迁到 /subscription，主页拉它纯属白跑一趟
    // （每次都是全量条目 JSON）。订阅页自己会在下面那条分支里拉。
    await Promise.allSettled([pullStatus(), pullPool(), pullUpstreams(), pullLan()]);
    setInterval(pullStatus, 3000);
    setInterval(pullUpstreams, 4000);
    setInterval(pullPool, 9000);
    setInterval(pullLan, 12000);
    setInterval(() => { if (!$("drawerLog").hidden) pullLogs(); }, 5000);
  }
  boot();
})();
